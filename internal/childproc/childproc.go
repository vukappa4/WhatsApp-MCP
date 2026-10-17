// Package childproc runs helper programs (ffmpeg, whisper.cpp) the same way
// everywhere: without a console window on Windows, and ffmpeg found without
// relying on the user's interactive PATH.
package childproc

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// FFmpeg returns ffmpeg from PATH, else from the known install locations
// (config.FFmpegCandidates): a daemon started by Task Scheduler or launchd
// can lack the user's PATH.
func FFmpeg() (string, error) { return FindFFmpeg(nil, config.FFmpegCandidates()) }

// FindFFmpeg returns ffmpeg from PATH (look, exec.LookPath when nil), else
// the first candidate that is an existing file.
func FindFFmpeg(look func(string) (string, error), candidates []string) (string, error) {
	if look == nil {
		look = exec.LookPath
	}
	if p, err := look("ffmpeg"); err == nil {
		return p, nil
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() {
			return c, nil
		}
	}
	return "", errors.New("ffmpeg not found on PATH nor in " + strings.Join(candidates, ", ") +
		": install it (" + config.FFmpegInstallHint() + ") and restart the daemon")
}
