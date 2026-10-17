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
// install location; PATH wins when it has ffmpeg.
func TestFindFFmpegFallsBackToKnownLocations(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "Links", "ffmpeg.exe")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "tools", "ffmpeg.exe")
	noPath := func(string) (string, error) { return "", errors.New("not found") }
	onPath := func(string) (string, error) { return `C:\bin\ffmpeg.exe`, nil }

	if p, err := FindFFmpeg(noPath, []string{missing, filepath.Dir(installed), installed}); err != nil || p != installed {
		t.Fatalf("fallback = %q, %v; want %q", p, err, installed)
	}
	if p, err := FindFFmpeg(onPath, []string{installed}); err != nil || p != `C:\bin\ffmpeg.exe` {
		t.Fatalf("PATH = %q, %v", p, err)
	}
	if _, err := FindFFmpeg(noPath, []string{missing}); err == nil || !strings.Contains(err.Error(), config.FFmpegInstallHint()) {
		t.Fatalf("nothing found: %v", err)
	}

}
