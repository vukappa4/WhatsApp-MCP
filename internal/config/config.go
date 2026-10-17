// Package config resolves the profile, data directory, port and bearer token.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Profile names a separate linked device with its own data dir and port.
type Profile string

const (
	ProfileProd Profile = "prod"
	ProfileDev  Profile = "dev"
)

// ParseProfile accepts "prod" or "dev".
func ParseProfile(s string) (Profile, error) {
	switch Profile(s) {
	case ProfileProd, ProfileDev:
		return Profile(s), nil
	}
	return "", fmt.Errorf("unknown profile %q: use prod or dev", s)
}

// Port is the MCP HTTP port of the profile.
func (p Profile) Port() int {
	if p == ProfileDev {
		return 8766
	}
	return 8765
}

// Config holds every path the daemon uses.
type Config struct {
	Profile Profile
	DataDir string
	Port    int
}

// New builds the config for a profile. dataDir overrides the default
// <AppDir>/<profile> location when not empty.
func New(p Profile, dataDir string) (*Config, error) {
	if dataDir == "" {
		base, err := AppDir()
		if err != nil {
			return nil, err
		}
		dataDir = filepath.Join(base, string(p))
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	return &Config{Profile: p, DataDir: abs, Port: p.Port()}, nil
}

// AppDir holds every profile's data dir, the installed binaries, the shared
// tools and the hook flags: %LOCALAPPDATA%\WhatsAppMCP on Windows,
// ~/Library/Application Support/WhatsAppMCP on macOS, and
// $XDG_DATA_HOME/WhatsAppMCP (default ~/.local/share) elsewhere. Never a
// cache folder: cleaners empty those, and the pairing lives here.
func AppDir() (string, error) {
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LOCALAPPDATA")
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, "Library", "Application Support")
		}
	default:
		base = os.Getenv("XDG_DATA_HOME")
		if !filepath.IsAbs(base) {
			base = ""
			if home, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(home, ".local", "share")
			}
		}
	}
	if base == "" {
		return "", errors.New("locate data dir: no home or local app data folder")
	}
	return filepath.Join(base, "WhatsAppMCP"), nil
}

// BinDir is where `install` puts the profile's binary:
// %LOCALAPPDATA%\WhatsAppMCP\bin for prod, bin-dev for dev. It is outside
// the data dir, so a binary swap never touches session or data files.
func BinDir(p Profile) (string, error) {
	base, err := AppDir()
	if err != nil {
		return "", err
	}
	name := "bin"
	if p != ProfileProd {
		name += "-" + string(p)
	}
	return filepath.Join(base, name), nil
}

// ToolsDir is %LOCALAPPDATA%\WhatsAppMCP\tools: the transcription binaries
// and models every profile shares.
func ToolsDir() (string, error) {
	base, err := AppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tools"), nil
}

// FFmpegCandidates lists, in order, where ffmpeg is looked for when it is not
// on PATH: a copy in the tools directory, then the package managers'
// install locations. A daemon started by Task Scheduler or launchd runs
// without the user's interactive PATH, which is where those live.
//   - Windows: winget's install of Gyan.FFmpeg (its Links shim, then the
//     package folders, newest first).
//   - macOS and others: Homebrew on Apple Silicon (/opt/homebrew), Homebrew
//     on Intel (/usr/local), MacPorts (/opt/local).
func FFmpegCandidates() []string {
	base, err := AppDir()
	if err != nil {
		return nil
	}
	if runtime.GOOS != "windows" {
		return []string{filepath.Join(base, "tools", "ffmpeg"), "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/opt/local/bin/ffmpeg"}
	}
	local := filepath.Dir(base)
	out := []string{filepath.Join(base, "tools", "ffmpeg.exe"), filepath.Join(local, "Microsoft", "WinGet", "Links", "ffmpeg.exe")}
	pkgs, _ := filepath.Glob(filepath.Join(local, "Microsoft", "WinGet", "Packages", "Gyan.FFmpeg*", "*", "bin", "ffmpeg.exe"))
	mod := func(p string) time.Time {
		fi, err := os.Stat(p)
		if err != nil {
			return time.Time{}
		}
		return fi.ModTime()
	}
	sort.SliceStable(pkgs, func(i, j int) bool { return mod(pkgs[i]).After(mod(pkgs[j])) })
	return append(out, pkgs...)
}

// FFmpegInstallHint is the command that installs ffmpeg on this OS.
func FFmpegInstallHint() string {
	switch runtime.GOOS {
	case "windows":
		return "winget install Gyan.FFmpeg"
	case "darwin":
		return "brew install ffmpeg"
	}
	return "install ffmpeg with the system package manager"
}

func (c *Config) SessionDB() string { return filepath.Join(c.DataDir, "session.db") }
func (c *Config) DataDB() string    { return filepath.Join(c.DataDir, "data.db") }
func (c *Config) MediaDir() string  { return filepath.Join(c.DataDir, "media") }
func (c *Config) LogDir() string    { return filepath.Join(c.DataDir, "logs") }
func (c *Config) TokenFile() string { return filepath.Join(c.DataDir, "token") }

// EnsureDirs creates the data, log and media directories.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.DataDir, c.LogDir(), c.MediaDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// EnsureToken returns the bearer token, creating a random 32-byte token on first use.
func (c *Config) EnsureToken() (string, error) {
	tok, err := c.ReadToken()
	if err == nil {
		return tok, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok = hex.EncodeToString(buf)
	f, err := os.OpenFile(c.TokenFile(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return c.ReadToken()
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(tok + "\n"); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadToken reads the existing bearer token.
func (c *Config) ReadToken() (string, error) {
	b, err := os.ReadFile(c.TokenFile())
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) < 32 {
		return "", fmt.Errorf("token file %s is too short", c.TokenFile())
	}
	return tok, nil
}
