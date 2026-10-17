package outgoing

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/childproc"
)

// convertTimeout bounds one ffmpeg run.
const convertTimeout = 2 * time.Minute

// Voice is an audio file converted to a WhatsApp voice note.
type Voice struct {
	Data     []byte // Ogg Opus, 16 kHz mono
	Seconds  uint32
	Waveform []byte // 64 values 0-100, as the WhatsApp app draws them
}

// waveformRate is the PCM rate the waveform and duration are computed from.
const waveformRate = 8000

// ConvertVoice converts an audio file (or a video's soundtrack) to Ogg Opus
// 16 kHz mono, and measures its duration and waveform. See convertOpened.
func ConvertVoice(ctx context.Context, path string) (*Voice, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return convertOpened(ctx, f, filepath.Base(path))
}

// convertOpened converts the audio read from f. The bytes are copied to a
// private temporary file first, so ffmpeg reads exactly what was checked,
// and the container is sniffed: only Ogg, WAV, MP4/M4A/MOV, AMR,
// Matroska/WebM, FLAC, MP3 and AAC are converted, with that demuxer forced.
// A playlist or concat list never reaches ffmpeg.
func convertOpened(ctx context.Context, f *os.File, name string) (*Voice, error) {
	head := make([]byte, 64)
	n, _ := f.ReadAt(head, 0)
	format := childproc.SniffAudio(head[:n])
	if format == "" {
		return nil, fmt.Errorf("%s is not an audio file ffmpeg may convert (Ogg, MP3, M4A/MP4, WAV, AAC, AMR, FLAC, WebM/Matroska)", name)
	}
	bin, err := childproc.FFmpeg()
	if err != nil {
		return nil, fmt.Errorf("voice notes need ffmpeg: %w", err)
	}
	dir, err := os.MkdirTemp("", "whatsapp-mcp-voice-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in")
	tmp, err := os.OpenFile(in, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(tmp, io.NewSectionReader(f, 0, maxVoiceInput+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("copy %s for conversion: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()

	opus, err := runFFmpeg(ctx, bin, in, format, name, "-ac", "1", "-ar", "16000", "-c:a", "libopus", "-b:a", "32k",
		"-application", "voip", "-f", "ogg")
	if err != nil {
		return nil, err
	}
	// Measure the converted voice note itself, so duration and waveform match what is sent.
	cmd := exec.CommandContext(ctx, bin, "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "pipe", "-f", "ogg", "-i", "pipe:0",
		"-ac", "1", "-ar", fmt.Sprint(waveformRate), "-f", "s16le", "pipe:1")
	childproc.Hide(cmd)
	cmd.Stdin = bytes.NewReader(opus)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	peaks, samples, readErr := readPeaks(bufio.NewReader(out), waveformRate/10)
	if readErr != nil {
		cancel() // stop ffmpeg instead of waiting for the timeout
	}
	if err := cmd.Wait(); err != nil || readErr != nil {
		return nil, fmt.Errorf("measure voice note: %v %v %s", err, readErr, strings.TrimSpace(stderr.String()))
	}
	if samples == 0 {
		return nil, fmt.Errorf("%s has no audio", name)
	}
	secs := uint32(math.Round(float64(samples) / waveformRate))
	return &Voice{Data: opus, Seconds: max(secs, 1), Waveform: waveform(peaks, 64)}, nil
}

// runFFmpeg converts in, read with the forced demuxer format, with the given
// output options and returns stdout. name is the file's name for messages.
func runFFmpeg(ctx context.Context, bin, in, format, name string, outArgs ...string) ([]byte, error) {
	// Local files only, demuxer forced: nothing makes ffmpeg fetch a URL.
	args := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,pipe",
		"-f", format, "-i", "file:" + in, "-vn", "-map_metadata", "-1"}, outArgs...)
	args = append(args, "pipe:1")
	cmd := exec.CommandContext(ctx, bin, args...)
	childproc.Hide(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ffmpeg took longer than %s converting %s", convertTimeout, name)
		}
		return nil, fmt.Errorf("ffmpeg cannot convert %s to a voice note: %v: %s", name, err, msg)
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg produced no audio from %s", name)
	}
	return stdout.Bytes(), nil
}

// readPeaks reads 16-bit little-endian mono PCM and returns the RMS of every
// window of n samples, plus the total sample count.
func readPeaks(r io.Reader, n int) ([]float64, int, error) {
	var peaks []float64
	var sum float64
	count, total := 0, 0
	buf := make([]byte, 2)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, total, err
		}
		v := float64(int16(binary.LittleEndian.Uint16(buf)))
		sum += v * v
		count++
		total++
		if count == n {
			peaks = append(peaks, math.Sqrt(sum/float64(count)))
			sum, count = 0, 0
		}
	}
	if count > 0 {
		peaks = append(peaks, math.Sqrt(sum/float64(count)))
	}
	return peaks, total, nil
}

// waveform resamples window levels to size bars scaled 0-100 (loudest = 100).
func waveform(levels []float64, size int) []byte {
	out := make([]byte, size)
	if len(levels) == 0 {
		return out
	}
	bars := make([]float64, size)
	top := 0.0
	for i := range bars {
		lo := i * len(levels) / size
		hi := max((i+1)*len(levels)/size, lo+1)
		hi = min(hi, len(levels))
		for _, l := range levels[min(lo, len(levels)-1):hi] {
			bars[i] = max(bars[i], l)
		}
		top = max(top, bars[i])
	}
	if top == 0 {
		return out
	}
	for i, b := range bars {
		out[i] = byte(math.Round(b / top * 100))
	}
	return out
}
