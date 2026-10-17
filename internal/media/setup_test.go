package media

import (
	"archive/zip"
	"bytes"
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
func TestPinnedArtifacts(t *testing.T) {
	for _, a := range []artifact{whisperCppZip, parakeetModel, whisperModel} {
		if !strings.HasPrefix(a.URL, "https://") || len(a.SHA256) != 64 || a.Size <= 0 || strings.Contains(a.URL, "/main/") {
			t.Errorf("artifact %+v is not pinned", a)
		}
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

// Real engine on real voice notes. Set WHATSAPP_MCP_VOICE_DIR to a folder of
// .ogg copies; WHATSAPP_MCP_TOOLS_DIR overrides <app dir>/tools
// and WHATSAPP_MCP_ENGINE picks parakeet (default) or whisper. Only timing,
// length and an Italian-word score are logged, never the text.
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
	tr, err := NewTranscriber(engine, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Check(); err != nil {
		t.Skipf("transcription tools not ready: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.ogg"))
	if len(files) == 0 {
		t.Skip("no .ogg files in WHATSAPP_MCP_VOICE_DIR")
	}
	for i, f := range files {
		fi, _ := os.Stat(f)
		start := time.Now()
		text, lang, err := tr.Transcribe(context.Background(), f, true, 0)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("file %d: %v", i+1, err)
		}
		share := italianShare(text)
		t.Logf("file %d: %d KB, %s, %d words, lang %q, Italian function words %.0f%%, engine %s",
			i+1, fi.Size()>>10, took.Round(100*time.Millisecond), len(strings.Fields(text)), lang, share*100, tr.Engine())
		if len(strings.Fields(text)) == 0 || share < 0.08 {
			t.Errorf("file %d: transcript does not look like Italian speech (%d words, %.0f%%)", i+1, len(strings.Fields(text)), share*100)
		}
	}
}

// Outside Windows the engines come from Homebrew: PATH first, then the
// Homebrew and MacPorts bin folders a launchd daemon lacks on its PATH.
func TestToolExeOutsideWindows(t *testing.T) {
	if pinnedBinaries {
		t.Skip("Windows runs the pinned release from the tools dir")
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
