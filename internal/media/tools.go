package media

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// pinnedBinaries is true where setup-transcription installs pinned
// whisper.cpp builds into the tools dir (see toolBundles): Windows and
// macOS. Elsewhere the tools come from PATH or Homebrew's whisper.cpp
// formula; the models are pinned everywhere.
var pinnedBinaries = len(toolBundles(runtime.GOOS, runtime.GOARCH)) > 0

// toolBinDirs are where Homebrew and MacPorts put binaries, for a daemon
// that runs without the user's PATH where the tools are not pinned.
var toolBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin"}

// toolExe resolves a whisper.cpp tool (parakeet-cli, whisper-cli): the
// pinned build in the tools dir on Windows and macOS; elsewhere PATH (look,
// exec.LookPath when nil), then toolBinDirs. When nothing is found it
// returns the first place it looked, for the error message.
func toolExe(toolsDir, name string, look func(string) (string, error)) string {
	if pinnedBinaries {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		return filepath.Join(binDir(toolsDir), name)
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

// engineTools are the whisper.cpp tools an engine choice (parakeet,
// whisper or all) runs.
func engineTools(engine string) []string {
	switch engine {
	case EngineParakeet:
		return []string{"parakeet-cli"}
	case EngineWhisper:
		return []string{"whisper-cli"}
	}
	return []string{"parakeet-cli", "whisper-cli"}
}

// toolsHint is how to get the whisper.cpp tools on this OS.
func toolsHint(engine string) string {
	if pinnedBinaries {
		return "run `whatsapp-mcp setup-transcription --engine " + engine + "`"
	}
	return "install them with `brew install whisper.cpp`"
}
