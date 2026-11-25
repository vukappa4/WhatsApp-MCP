package childproc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// A daemon started without the user's PATH still finds ffmpeg at a known
// install location; PATH wins over those, and the pinned build wins over
// PATH.
func TestFindFFmpegOrder(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "Links", "ffmpeg.exe")
	pinned := filepath.Join(dir, "tools", "ffmpeg-8.1.3", "ffmpeg")
	for _, p := range []string{installed, pinned} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(dir, "tools", "ffmpeg.exe")
	noPath := func(string) (string, error) { return "", errors.New("not found") }
	onPath := func(string) (string, error) { return "/opt/homebrew/bin/ffmpeg", nil }

	if p, err := FindFFmpeg(noPath, config.FFmpegSearch{Fallback: []string{missing, filepath.Dir(installed), installed}}); err != nil || p != installed {
		t.Fatalf("fallback = %q, %v; want %q", p, err, installed)
	}
	if p, err := FindFFmpeg(onPath, config.FFmpegSearch{Fallback: []string{installed}}); err != nil || p != "/opt/homebrew/bin/ffmpeg" {
		t.Fatalf("PATH = %q, %v", p, err)
	}
	if p, err := FindFFmpeg(onPath, config.FFmpegSearch{Pinned: pinned, Fallback: []string{installed}}); err != nil || p != pinned {
		t.Fatalf("pinned = %q, %v; want %q over PATH", p, err, pinned)
	}
	if p, err := FindFFmpeg(onPath, config.FFmpegSearch{Pinned: missing, Fallback: []string{installed}}); err != nil || p != "/opt/homebrew/bin/ffmpeg" {
		t.Fatalf("pinned build missing: %q, %v; want PATH", p, err)
	}
	if _, err := FindFFmpeg(noPath, config.FFmpegSearch{Pinned: missing}); err == nil || !strings.Contains(err.Error(), config.FFmpegInstallHint()) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("nothing found: %v", err)
	}
}
