package media

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// pinnedBinaries is true where setup-transcription installs the pinned
// whisper.cpp release into the tools dir: Windows, the only OS that release
// ships command-line builds for. Elsewhere the tools come from Homebrew's
// whisper.cpp formula (`brew install whisper.cpp`, the same release, built
// with Metal on Apple Silicon); the models are pinned everywhere.
var pinnedBinaries = runtime.GOOS == "windows"

// toolBinDirs are where Homebrew (Apple Silicon, then Intel) and MacPorts
// put binaries. A daemon started by launchd runs without the user's PATH.
var toolBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin"}

// toolExe resolves a whisper.cpp tool (parakeet-cli, whisper-cli): the
// pinned release in the tools dir on Windows; elsewhere PATH (look,
// exec.LookPath when nil), then toolBinDirs. When nothing is found it
// returns the first place it looked, for the error message.
func toolExe(toolsDir, name string, look func(string) (string, error)) string {
	if pinnedBinaries {
		return filepath.Join(binDir(toolsDir), name+".exe")
	}
	if look == nil {
		look = exec.LookPath
	}
	if p, err := look(name); err == nil {
		return p
	}
	for _, d := range toolBinDirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return filepath.Join(toolBinDirs[0], name)
}

// toolsHint is how to get the whisper.cpp tools on this OS.
func toolsHint(engine string) string {
	if pinnedBinaries {
		return "run `whatsapp-mcp setup-transcription --engine " + engine + "`"
	}
	return "install them with `brew install whisper.cpp`"
}
