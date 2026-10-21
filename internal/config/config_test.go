package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// The app dir is never a cache folder, which cleaners empty: on macOS it is
// in Application Support.
func TestAppDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_DATA_HOME", "")
	want := map[string]string{
		"windows": filepath.Join(home, "AppData", "Local", "WhatsAppMCP"),
		"darwin":  filepath.Join(home, "Library", "Application Support", "WhatsAppMCP"),
	}[runtime.GOOS]
	if want == "" {
		want = filepath.Join(home, ".local", "share", "WhatsAppMCP")
	}
	if got, err := AppDir(); err != nil || got != want {
		t.Fatalf("AppDir = %q, %v; want %q", got, err, want)
	}
}

// Outside Windows a daemon started by launchd lacks the user's PATH: ffmpeg
// is also looked for where Homebrew (Apple Silicon and Intel) and MacPorts
// put it.
func TestFFmpegLocationsIncludeHomebrew(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew is not on Windows")
	}
	got := FFmpegLocations().Fallback
	for _, want := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/opt/local/bin/ffmpeg"} {
		if !slices.Contains(got, want) {
			t.Errorf("fallback %v lacks %s", got, want)
		}
	}
}

// On macOS the ffmpeg build setup-transcription pins is the pinned place,
// tried before PATH; elsewhere nothing is pinned.
func TestFFmpegLocationsPinOnlyOnMac(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base, err := AppDir()
	if err != nil {
		t.Fatal(err)
	}
	s := FFmpegLocations()
	want := ""
	if runtime.GOOS == "darwin" {
		want = filepath.Join(base, "tools", PinnedFFmpegDir, "ffmpeg")
	}
	if s.Pinned != want {
		t.Fatalf("pinned = %q, want %q", s.Pinned, want)
	}
	if places := s.Places(); want != "" && places[0] != want {
		t.Fatalf("places = %v, want the pinned build first", places)
	}
}

// The winget install of Gyan.FFmpeg is a known ffmpeg location, both its
// Links shim and the package folder.
func TestFFmpegLocationsIncludeWinget(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("winget is Windows only")
	}
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	pkg := filepath.Join(local, "Microsoft", "WinGet", "Packages", "Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe", "ffmpeg-9.0.2-full_build", "bin", "ffmpeg.exe")
	if err := os.MkdirAll(filepath.Dir(pkg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := FFmpegLocations()
	want := []string{
		filepath.Join(local, "WhatsAppMCP", "tools", "ffmpeg.exe"),
		filepath.Join(local, "Microsoft", "WinGet", "Links", "ffmpeg.exe"),
		pkg,
	}
	if got.Pinned != "" || !slices.Equal(got.Fallback, want) {
		t.Fatalf("locations = %+v, want fallback %v", got, want)
	}
}

func TestLoadSettings(t *testing.T) {
	dir := t.TempDir()
	if s, err := LoadSettings(dir); err != nil || len(s.SendFileRoots)+len(s.SendFileExtraRoots) != 0 {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	abs := filepath.Join(dir, "Progetti")
	b, _ := json.Marshal(map[string]any{"send_file_extra_roots": []string{abs}})
	_ = os.WriteFile(SettingsFile(dir), append([]byte("\xEF\xBB\xBF"), b...), 0o600)
	if s, err := LoadSettings(dir); err != nil || len(s.SendFileExtraRoots) != 1 || s.SendFileExtraRoots[0] != abs {
		t.Fatalf("with BOM: %+v %v", s, err)
	}
	for _, bad := range []string{`{"send_files_roots": []}`, `{"send_file_roots": ["rel"]}`, `[1]`,
		// A stray brace must not hide the keys after it.
		`{"send_file_roots": []}, "send_file_extra_roots": ["rel"]}`, `{} {"send_files_roots": []}`} {
		_ = os.WriteFile(SettingsFile(dir), []byte(bad), 0o600)
		if _, err := LoadSettings(dir); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
