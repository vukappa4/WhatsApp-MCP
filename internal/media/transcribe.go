package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/childproc"
)

// Transcriber turns an audio file into text.
type Transcriber interface {
	// Engine names the engine and model, stored with each transcript.
	Engine() string
	// Check returns nil when the engine can run now, else what is missing
	// and how to fix it.
	Check() error
	// Transcribe returns the text and, when the engine reports it, the
	// language. voice is true for a voice note (always Ogg Opus).
	Transcribe(ctx context.Context, path string, voice bool, duration time.Duration) (text, lang string, err error)
}

// Engine choices for the run --transcriber flag.
const (
	EngineParakeet = "parakeet"
	EngineWhisper  = "whisper"
	EngineOff      = "off"
)

// CLITranscriber runs a whisper.cpp command-line tool (see toolExe), fed
// 16 kHz mono WAV by ffmpeg on stdin, with a model from the shared tools
// directory. Child processes have no console window and run below normal
// priority.
type CLITranscriber struct {
	Choice   string // EngineParakeet or EngineWhisper
	ToolsDir string
	// LookPath finds ffmpeg and, where the tools are not pinned (see
	// toolExe), the engine on PATH; exec.LookPath when nil.
	LookPath func(string) (string, error)
	// FFmpegCandidates are tried in order when ffmpeg is not on PATH.
	FFmpegCandidates []string
}

// NewTranscriber returns the transcriber for a --transcriber choice; nil for
// "off". ffmpegCandidates are the places to find ffmpeg when PATH lacks it.
func NewTranscriber(choice, toolsDir string, ffmpegCandidates []string) (Transcriber, error) {
	switch choice {
	case EngineParakeet, EngineWhisper:
		return &CLITranscriber{Choice: choice, ToolsDir: toolsDir, FFmpegCandidates: ffmpegCandidates}, nil
	case EngineOff:
		return nil, nil
	}
	return nil, fmt.Errorf("unknown transcriber %q: use parakeet, whisper or off", choice)
}

func (t *CLITranscriber) model() artifact {
	if t.Choice == EngineWhisper {
		return whisperModel
	}
	return parakeetModel
}

func (t *CLITranscriber) exe() string {
	name := "parakeet-cli"
	if t.Choice == EngineWhisper {
		name = "whisper-cli"
	}
	return toolExe(t.ToolsDir, name, t.LookPath)
}

func (t *CLITranscriber) Engine() string {
	if t.Choice == EngineWhisper {
		return "whisper-large-v3-turbo-q8_0"
	}
	return "parakeet-tdt-0.6b-v3-q8_0"
}

// FFmpeg is the ffmpeg executable the transcriber runs.
func (t *CLITranscriber) FFmpeg() (string, error) {
	return childproc.FindFFmpeg(t.LookPath, t.FFmpegCandidates)
}

func (t *CLITranscriber) Check() error {
	if _, err := os.Stat(t.exe()); err != nil {
		return fmt.Errorf("%s is missing: %s", t.exe(), toolsHint(t.Choice))
	}
	m := t.model()
	if fi, err := os.Stat(modelPath(t.ToolsDir, m)); err != nil || fi.Size() != m.Size {
		return fmt.Errorf("model %s is missing: run `whatsapp-mcp setup-transcription --engine %s`", m.Name, t.Choice)
	}
	_, err := t.FFmpeg()
	return err
}

// threads leaves half the cores to the rest of the machine.
func threads() string {
	return fmt.Sprint(max(1, min(8, runtime.NumCPU()/2)))
}

// timeoutFor bounds one run: generous for the audio length, never endless.
func timeoutFor(d time.Duration) time.Duration {
	if d <= 0 {
		return 15 * time.Minute
	}
	return min(2*time.Minute+2*d, 30*time.Minute)
}

// Transcribe sniffs the container first: only a few audio formats reach
// ffmpeg, with the demuxer forced and the protocols limited to local files.
func (t *CLITranscriber) Transcribe(ctx context.Context, path string, voice bool, duration time.Duration) (string, string, error) {
	format, err := audioDemuxer(path, voice)
	if err != nil {
		return "", "", err
	}
	if err := t.Check(); err != nil {
		return "", "", err
	}
	ff, _ := t.FFmpeg()
	ctx, cancel := context.WithTimeout(ctx, timeoutFor(duration))
	defer cancel()
	model := modelPath(t.ToolsDir, t.model())
	args := []string{"-m", model, "-f", "-", "-t", threads()}
	var outBase string
	if t.Choice == EngineWhisper {
		// whisper-cli writes stdin results to the console, not to stdout: ask for a JSON file instead.
		dir, err := os.MkdirTemp("", "whatsapp-mcp-whisper-")
		if err != nil {
			return "", "", err
		}
		defer os.RemoveAll(dir)
		outBase = filepath.Join(dir, "out")
		args = append(args, "-l", "auto", "-np", "-oj", "-of", outBase)
	} else {
		args = append(args, "-np")
	}
	stdout, err := pipeline(ctx, ff, ffmpegArgs(path, format), t.exe(), args)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", "", fmt.Errorf("transcription timed out after %s", timeoutFor(duration))
		}
		return "", "", err
	}
	if t.Choice != EngineWhisper {
		return strings.TrimSpace(string(stdout)), "", nil
	}
	return readWhisperJSON(outBase + ".json")
}

// readWhisperJSON reads whisper-cli's -oj output: segments and the detected language.
func readWhisperJSON(path string) (string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("whisper-cli wrote no result: %w", err)
	}
	var out struct {
		Result struct {
			Language string `json:"language"`
		} `json:"result"`
		Transcription []struct {
			Text string `json:"text"`
		} `json:"transcription"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", "", fmt.Errorf("read whisper-cli result: %w", err)
	}
	var parts []string
	for _, s := range out.Transcription {
		if s := strings.TrimSpace(s.Text); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " "), out.Result.Language, nil
}

// tailBuffer keeps the last n bytes written: enough of a tool's stderr to
// explain a failure.
type tailBuffer struct {
	n int
	b []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.n {
		t.b = t.b[len(t.b)-t.n:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.b)) }

// engineLog is an engine's stderr: its tail, and whether a line reported an
// error. whisper.cpp's tools print "error: ..." for a failed input and carry
// on: parakeet-cli then exits 0 with no text, which is a failure, not silence.
type engineLog struct {
	tail   tailBuffer
	head   []byte // the current line's first bytes
	failed bool
}

func (l *engineLog) Write(p []byte) (int, error) {
	l.tail.Write(p)
	for _, c := range p {
		switch {
		case c == '\n' || c == '\r':
			l.head = l.head[:0]
		case len(l.head) < 32:
			l.head = append(l.head, c)
			l.failed = l.failed || isErrorLine(string(l.head))
		}
	}
	return len(p), nil
}

// isErrorLine reports whether a line is one of the "error: ..." lines
// whisper.cpp's tools print on a failure.
func isErrorLine(line string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "error:")
}

// cause explains a failed run. whisper.cpp prints the specific reason on the
// line before its generic error line, so the cause runs from that line to
// the end; without an error line it is the last line.
func (l *engineLog) cause() string {
	var lines []string
	for _, s := range strings.Split(l.tail.String(), "\n") {
		if s = strings.TrimSpace(s); s != "" {
			lines = append(lines, s)
		}
	}
	for i, s := range lines {
		if isErrorLine(s) {
			return strings.Join(lines[max(0, i-1):], "; ")
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// pipeline runs `first | second` and returns second's stdout.
func pipeline(ctx context.Context, first string, firstArgs []string, second string, secondArgs []string) ([]byte, error) {
	a := exec.CommandContext(ctx, first, firstArgs...)
	b := exec.CommandContext(ctx, second, secondArgs...)
	for _, c := range []*exec.Cmd{a, b} {
		childproc.Background(c)
		c.WaitDelay = 5 * time.Second
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	aErr, bErr := &tailBuffer{n: 2048}, &engineLog{tail: tailBuffer{n: 2048}}
	a.Stdout, a.Stderr = pw, aErr
	b.Stdin, b.Stdout, b.Stderr = pr, &out, bErr
	if err := a.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("start %s: %w", filepath.Base(first), err)
	}
	if err := b.Start(); err != nil {
		pr.Close()
		pw.Close()
		_ = a.Process.Kill()
		_ = a.Wait()
		return nil, fmt.Errorf("start %s: %w", filepath.Base(second), err)
	}
	// The children hold their own handles; ours would keep the pipe open.
	pr.Close()
	pw.Close()
	errA := a.Wait()
	errB := b.Wait()
	engineFailed := errB != nil || bErr.failed
	// A failing ffmpeg starves the engine, and an engine that quits breaks
	// ffmpeg's output: report the cause, not the symptom. Which one exits
	// first is a race, so only ffmpeg's own error tells them apart.
	switch {
	case errA != nil && (!engineFailed || !closedPipe(errA, aErr.String())):
		return nil, fmt.Errorf("ffmpeg failed: %v: %s", errA, lastLine(aErr.String()))
	case errB != nil:
		return nil, fmt.Errorf("%s failed: %v: %s", filepath.Base(second), errB, bErr.cause())
	case bErr.failed:
		return nil, fmt.Errorf("%s failed: %s", filepath.Base(second), bErr.cause())
	}
	return out.Bytes(), nil
}

// closedPipe reports whether ffmpeg died writing into a pipe whose reader
// had quit: killed by SIGPIPE, or reporting EPIPE. ffmpeg on Windows gets
// EINVAL for that write and reports it at the muxer.
func closedPipe(err error, stderr string) bool {
	return strings.Contains(err.Error(), "signal: broken pipe") || strings.Contains(stderr, "Broken pipe") ||
		runtime.GOOS == "windows" && strings.Contains(stderr, "Error submitting a packet to the muxer: Invalid argument")
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
