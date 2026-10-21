package media

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
}

func TestExtractZipFlattensAndRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.zip")
	writeZip(t, good, map[string]string{"Release/parakeet-cli.exe": "x", "Release/ggml.dll": "y"})
	out := filepath.Join(dir, "out")
	if err := extractZip(good, out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "parakeet-cli.exe")); err != nil || string(b) != "x" {
		t.Fatalf("flattened file: %q %v", b, err)
	}
	bad := []string{"../evil.exe", "Release/../../evil.exe"}
	if runtime.GOOS == "windows" {
		bad = append(bad, "C:/evil.exe") // elsewhere a folder named "C:"
	}
	for _, name := range bad {
		bad := filepath.Join(dir, "bad.zip")
		writeZip(t, bad, map[string]string{name: "z"})
		if err := extractZip(bad, filepath.Join(dir, "out2")); err == nil {
			t.Errorf("entry %q accepted", name)
		}
	}
}

func TestCopyVerifiedChecksSizeAndHash(t *testing.T) {
	body := []byte("model bytes")
	sum := sha256.Sum256(body)
	a := artifact{Name: "m.bin", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	dst := filepath.Join(t.TempDir(), "m.bin.part")
	if err := copyVerified(dst, bytes.NewReader(body), a, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := copyVerified(dst, strings.NewReader("model bytez"), a, io.Discard); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("wrong hash: %v", err)
	}
	if err := copyVerified(dst, strings.NewReader("model bytes and more"), a, io.Discard); err == nil {
		t.Fatal("wrong size accepted")
	}
}

// Every pinned artifact is an https URL at a fixed revision with a full hash.
// Every OS and architecture the releases ship for gets both engines' tools;
// on macOS from this project's tools release, with ffmpeg where
// config.FFmpegLocations pins it.
func TestPinnedArtifacts(t *testing.T) {
	all := []artifact{parakeetModel, whisperModel}
	for _, p := range [][2]string{{"windows", "amd64"}, {"windows", "arm64"}, {"darwin", "arm64"}, {"darwin", "amd64"}} {
		bundles := toolBundles(p[0], p[1])
		var exes []string
		for _, b := range bundles {
			all = append(all, b.artifact)
			for _, e := range b.Exes {
				exes = append(exes, b.Dir+"/"+e)
			}
			if p[0] == "darwin" && (b.URL != toolsRelease+b.Name || !strings.HasSuffix(b.Name, "-darwin-"+p[1]+".tar.gz")) {
				t.Errorf("%s/%s: %s is not this project's %s build", p[0], p[1], b.URL, p[1])
			}
		}
		want := []string{whisperCppDir + "/parakeet-cli.exe", whisperCppDir + "/whisper-cli.exe"}
		if p[0] == "darwin" {
			want = []string{whisperCppDir + "/parakeet-cli", whisperCppDir + "/whisper-cli", config.PinnedFFmpegDir + "/ffmpeg"}
		}
		if !slices.Equal(exes, want) {
			t.Errorf("%s/%s installs %v, want %v", p[0], p[1], exes, want)
		}
	}
	if b := toolBundles("linux", "amd64"); b != nil {
		t.Errorf("linux pins %v", b)
	}
	for _, a := range all {
		if !strings.HasPrefix(a.URL, "https://") || len(a.SHA256) != 64 || a.Size <= 0 || strings.Contains(a.URL, "/main/") {
			t.Errorf("artifact %+v is not pinned", a)
		}
	}
}

type tarEntry struct {
	name string
	typ  byte
	mode int64
	body string
}

func writeTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.body))}
		if e.typ == tar.TypeSymlink {
			h.Linkname, h.Size = "/usr/bin/true", 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gz.Close()
	f.Close()
}

// The macOS archives unpack with the executable bit kept; an entry that
// leaves the folder, or a link, fails the whole install.
func TestExtractTarGzKeepsExecutablesAndRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.tar.gz")
	writeTarGz(t, good, []tarEntry{
		{"./", tar.TypeDir, 0o755, ""},
		{"./ffmpeg", tar.TypeReg, 0o755, "exe"},
		{"./LICENSE.md", tar.TypeReg, 0o644, "text"},
		{"licenses/COPYING", tar.TypeReg, 0o600, "more"},
	})
	out := filepath.Join(dir, "out")
	if err := extractTarGz(good, out); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"ffmpeg": "exe", "LICENSE.md": "text", "licenses/COPYING": "more"} {
		if b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(name))); err != nil || string(b) != body {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{"ffmpeg": 0o755, "LICENSE.md": 0o644} {
			if fi, err := os.Stat(filepath.Join(out, name)); err != nil || fi.Mode().Perm() != want {
				t.Errorf("%s mode = %v, %v; want %v", name, fi.Mode().Perm(), err, want)
			}
		}
	}
	for _, e := range []tarEntry{
		{"../evil", tar.TypeReg, 0o755, "x"},
		{"bin/../../evil", tar.TypeReg, 0o755, "x"},
		{"/evil", tar.TypeReg, 0o755, "x"},
		{"ffmpeg", tar.TypeSymlink, 0o755, ""},
	} {
		bad := filepath.Join(dir, "bad.tar.gz")
		writeTarGz(t, bad, []tarEntry{e})
		if err := extractTarGz(bad, filepath.Join(dir, "out2")); err == nil {
			t.Errorf("entry %q (type %c) accepted", e.name, e.typ)
		}
	}
}

// A bundle installs from a verified copy into its own folder, is not
// fetched again while its marker and programs are there, and an archive
// that lacks a program installs nothing.
func TestInstallBundle(t *testing.T) {
	src, tools := t.TempDir(), t.TempDir()
	archive := filepath.Join(src, "tools-test.tar.gz")
	writeTarGz(t, archive, []tarEntry{{"./whisper-cli", tar.TypeReg, 0o755, "exe"}, {"./LICENSE", tar.TypeReg, 0o644, "MIT"}})
	body, _ := os.ReadFile(archive)
	sum := sha256.Sum256(body)
	b := bundle{
		artifact: artifact{Name: "tools-test.tar.gz", URL: "https://example.invalid/tools-test.tar.gz", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))},
		Dir:      "test-1", Exes: []string{"whisper-cli"},
	}
	var out bytes.Buffer
	o := SetupOptions{ToolsDir: tools, SourceDirs: []string{src}, Out: &out}
	if err := installBundle(context.Background(), o, b); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(tools, "test-1", "whisper-cli")); err != nil || string(got) != "exe" {
		t.Fatalf("installed program: %q %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(tools, "*.part")); len(left) != 0 {
		t.Fatalf("download left behind: %v", left)
	}
	out.Reset()
	if err := installBundle(context.Background(), o, b); err != nil || !strings.Contains(out.String(), "already installed") {
		t.Fatalf("second install: %v\n%s", err, out.String())
	}
	os.Remove(filepath.Join(tools, "test-1", "whisper-cli"))
	if err := installBundle(context.Background(), o, b); err != nil || !strings.Contains(out.String(), "Fetching") {
		t.Fatalf("a missing program is installed again: %v\n%s", err, out.String())
	}

	b.Dir, b.Exes = "test-2", []string{"whisper-cli", "parakeet-cli"}
	if err := installBundle(context.Background(), o, b); err == nil || !strings.Contains(err.Error(), "parakeet-cli is not in") {
		t.Fatalf("incomplete archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tools, "test-2")); !os.IsNotExist(err) {
		t.Fatalf("incomplete archive left %v", err)
	}
}

// italianShare is the share of words that are common Italian function words:
// a plausibility check that never prints the text.
func italianShare(text string) float64 {
	common := map[string]bool{}
	for _, w := range strings.Fields("che di e il la le lo gli un una non per con mi ti si ci è a in da del della al ma se sono ho hai ha io tu poi anche come cosa questo questa quello perché però già più") {
		common[w] = true
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && r != 'è' })
	if len(words) == 0 {
		return 0
	}
	n := 0
	for _, w := range words {
		if common[w] {
			n++
		}
	}
	return float64(n) / float64(len(words))
}

// words are the lowercase words of s.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// Real engine on real voice notes, through the daemon's own ffmpeg lookup.
// Set WHATSAPP_MCP_VOICE_DIR to a folder of .ogg copies;
// WHATSAPP_MCP_TOOLS_DIR overrides <app dir>/tools and WHATSAPP_MCP_ENGINE
// picks parakeet (default) or whisper. A note with a .txt beside it
// (note.ogg, note.txt) is a recording of that text: its transcript must hold
// at least 80% of the text's words, and is logged. Of any other note only
// timing, length and an Italian-word score are logged, never the text.
func TestTranscribeRealVoiceNotes(t *testing.T) {
	dir := os.Getenv("WHATSAPP_MCP_VOICE_DIR")
	if dir == "" {
		t.Skip("WHATSAPP_MCP_VOICE_DIR not set")
	}
	tools := os.Getenv("WHATSAPP_MCP_TOOLS_DIR")
	if tools == "" {
		var err error
		if tools, err = config.ToolsDir(); err != nil {
			t.Fatal(err)
		}
	}
	engine := os.Getenv("WHATSAPP_MCP_ENGINE")
	if engine == "" {
		engine = EngineParakeet
	}
	tr, err := NewTranscriber(engine, tools, config.FFmpegLocations())
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Check(); err != nil {
		t.Fatalf("transcription tools not ready: %v", err)
	}
	cli := tr.(*CLITranscriber)
	ff, _ := cli.FFmpeg()
	t.Logf("engine %s, ffmpeg %s", cli.exe(), ff)
	files, _ := filepath.Glob(filepath.Join(dir, "*.ogg"))
	if len(files) == 0 {
		t.Fatal("no .ogg files in WHATSAPP_MCP_VOICE_DIR")
	}
	for i, f := range files {
		fi, _ := os.Stat(f)
		start := time.Now()
		text, lang, err := tr.Transcribe(context.Background(), f, true, 0)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("file %d: %v", i+1, err)
		}
		if said, err := os.ReadFile(strings.TrimSuffix(f, ".ogg") + ".txt"); err == nil {
			got := words(text)
			want := words(string(said))
			found := 0
			for _, w := range want {
				if slices.Contains(got, w) {
					found++
				}
			}
			t.Logf("file %d (%s): %s, lang %q, %d of %d words, engine %s: %q",
				i+1, filepath.Base(f), took.Round(100*time.Millisecond), lang, found, len(want), tr.Engine(), text)
			if len(want) == 0 || found*10 < len(want)*8 {
				t.Errorf("file %d: transcript %q holds %d of the %d words of %q", i+1, text, found, len(want), said)
			}
			continue
		}
		share := italianShare(text)
		t.Logf("file %d: %d KB, %s, %d words, lang %q, Italian function words %.0f%%, engine %s",
			i+1, fi.Size()>>10, took.Round(100*time.Millisecond), len(strings.Fields(text)), lang, share*100, tr.Engine())
		if len(strings.Fields(text)) == 0 || share < 0.08 {
			t.Errorf("file %d: transcript does not look like Italian speech (%d words, %.0f%%)", i+1, len(strings.Fields(text)), share*100)
		}
	}
}

// On Windows and macOS the engines are the pinned build in the tools dir,
// whatever PATH holds, and a missing one is fixed by setup-transcription.
func TestToolExePinned(t *testing.T) {
	if !pinnedBinaries {
		t.Skip("the tools are not pinned on " + runtime.GOOS)
	}
	tools := t.TempDir()
	onPath := func(name string) (string, error) { return "/somewhere/bin/" + name, nil }
	want := filepath.Join(tools, whisperCppDir, "parakeet-cli")
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got := toolExe(tools, "parakeet-cli", onPath); got != want {
		t.Fatalf("toolExe = %q, want %q", got, want)
	}
	err := (&CLITranscriber{Choice: EngineWhisper, ToolsDir: tools, LookPath: onPath}).Check()
	if err == nil || !strings.Contains(err.Error(), "setup-transcription --engine whisper") || strings.Contains(err.Error(), "brew") {
		t.Fatalf("Check = %v, want the setup-transcription hint", err)
	}
}

// Where the tools are not pinned they come from PATH first, then the
// Homebrew and MacPorts bin folders a daemon started without the user's PATH
// lacks.
func TestToolExeWhereNotPinned(t *testing.T) {
	if pinnedBinaries {
		t.Skip(runtime.GOOS + " runs the pinned build from the tools dir")
	}
	onPath := func(name string) (string, error) { return "/somewhere/bin/" + name, nil }
	if got := toolExe(t.TempDir(), "parakeet-cli", onPath); got != "/somewhere/bin/parakeet-cli" {
		t.Fatalf("PATH = %q", got)
	}
	noPath := func(string) (string, error) { return "", os.ErrNotExist }
	got := toolExe(t.TempDir(), "whisper-cli", noPath)
	if filepath.Base(got) != "whisper-cli" || !slices.Contains(toolBinDirs, filepath.Dir(got)) {
		t.Fatalf("fallback = %q, want a Homebrew or MacPorts path", got)
	}
	if err := (&CLITranscriber{Choice: EngineWhisper, ToolsDir: t.TempDir(), LookPath: noPath}).Check(); err != nil &&
		!strings.Contains(err.Error(), "brew install whisper.cpp") && !strings.Contains(err.Error(), "model") {
		t.Fatalf("Check = %v, want the brew hint", err)
	}
}
