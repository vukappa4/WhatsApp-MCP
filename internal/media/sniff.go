package media

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/vukappa4/whatsapp-mcp/internal/childproc"
)

// errUnsupportedAudio is a file whose bytes are none of the audio containers
// ffmpeg is allowed to read. It is never handed to ffmpeg: a playlist (.m3u,
// HLS), concat list or SDP file would make it open URLs or other files.
var errUnsupportedAudio = errors.New("not transcribed: the file is not Ogg, MP3, MP4/M4A, WAV, AAC, AMR, FLAC or WebM/Matroska audio")

// audioDemuxer sniffs the first bytes of path and returns the ffmpeg input
// format to force for it. Voice notes must be Ogg (WhatsApp records them as
// Ogg Opus); other audio may be one of a few containers. Anything else is
// errUnsupportedAudio.
func audioDemuxer(path string, voice bool) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 64)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("%w (%v)", errUnsupportedAudio, err)
	}
	format := childproc.SniffAudio(head[:n])
	if format == "" || (voice && format != "ogg") {
		return "", errUnsupportedAudio
	}
	return format, nil
}

// ffmpegArgs decodes path to 16 kHz mono WAV on stdout. The input format is
// forced (no probing) and only the file and pipe protocols are allowed, so
// no input can make ffmpeg open a URL.
func ffmpegArgs(path, format string) []string {
	return []string{"-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe", "-f", format,
		"-i", "file:" + path, "-vn", "-ar", "16000", "-ac", "1", "-f", "wav", "pipe:1"}
}
