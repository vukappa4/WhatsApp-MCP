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

// FFmpeg returns the ffmpeg to run (see FindFFmpeg) from this OS's known
// locations (config.FFmpegLocations): a daemon started by Task Scheduler or
// launchd can lack the user's PATH.
func FFmpeg() (string, error) { return FindFFmpeg(nil, config.FFmpegLocations()) }

// FindFFmpeg returns s.Pinned when it is an existing file, else ffmpeg from
// PATH (look, exec.LookPath when nil), else the first of s.Fallback that is
// an existing file.
func FindFFmpeg(look func(string) (string, error), s config.FFmpegSearch) (string, error) {
	if s.Pinned != "" && isFile(s.Pinned) {
		return s.Pinned, nil
	}
	if look == nil {
		look = exec.LookPath
	}
	if p, err := look("ffmpeg"); err == nil {
		return p, nil
	}
	for _, c := range s.Fallback {
		if isFile(c) {
			return c, nil
		}
	}
	return "", errors.New("ffmpeg not found on PATH nor in " + strings.Join(s.Places(), ", ") +
		": install it (" + config.FFmpegInstallHint() + ") and restart the daemon")
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
