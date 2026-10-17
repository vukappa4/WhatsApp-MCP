package media

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPipelineHelperProcess is not a test: pipeline tests run the test
// binary as a fake ffmpeg or engine, picked by the argument after "--".
func TestPipelineHelperProcess(t *testing.T) {
	if os.Getenv("WHATSAPP_MCP_PIPELINE_HELPER") != "1" {
		t.Skip("helper process")
	}
	role := os.Args[len(os.Args)-1]
	switch role {
	case "ffmpeg-streams":
		// Decodes forever; a write to a closed pipe ends it, as it ends ffmpeg.
		buf := make([]byte, 32<<10)
		for {
			if _, err := os.Stdout.Write(buf); err != nil {
				fmt.Fprintln(os.Stderr, "av_interleaved_write_frame(): Broken pipe")
				time.Sleep(100 * time.Millisecond)
				os.Exit(1)
			}
		}
	case "ffmpeg-fails":
		fmt.Fprintln(os.Stderr, "Invalid data found when processing input")
		os.Exit(1)
	case "engine-fails":
		fmt.Fprintln(os.Stderr, "error: failed to load model")
		os.Exit(1)
	case "engine-reads":
		if n, _ := io.Copy(io.Discard, os.Stdin); n == 0 {
			fmt.Fprintln(os.Stderr, "error: no audio")
			os.Exit(1)
		}
		fmt.Println("ok")
		os.Exit(0)
	}
	os.Exit(2)
}

// TestPipelineReportsTheCause names the process that failed first: an engine
// that quits breaks ffmpeg's output, and a failing ffmpeg starves the engine.
func TestPipelineReportsTheCause(t *testing.T) {
	t.Setenv("WHATSAPP_MCP_PIPELINE_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := func(role string) []string { return []string{"-test.run=^TestPipelineHelperProcess$", "--", role} }
	for _, tc := range []struct {
		ffmpeg, engine, want string
	}{
		{"ffmpeg-streams", "engine-fails", "failed to load model"},
		{"ffmpeg-fails", "engine-reads", "Invalid data found when processing input"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, err := pipeline(ctx, exe, args(tc.ffmpeg), exe, args(tc.engine))
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s | %s: error %v, want it to name %q", tc.ffmpeg, tc.engine, err, tc.want)
		}
	}
}
