package media

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/childproc"
	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// artifact is one pinned download, verified by size and SHA-256.
type artifact struct {
	Name   string
	URL    string
	SHA256 string
	Size   int64
}

// whisperCppRelease is the pinned whisper.cpp Windows build; it ships both
// parakeet-cli.exe and whisper-cli.exe. Homebrew's whisper.cpp 1.9.5 is the
// same commit.
const whisperCppRelease = "b5454"

var (
	whisperCppZip = artifact{
		Name:   "whisper-bin-x64.zip",
		URL:    "https://github.com/ggml-org/whisper.cpp/releases/download/" + whisperCppRelease + "/whisper-bin-x64.zip",
		SHA256: "6ba69e3482d7826214f90a6a9c84ca07782aec1e1d0c6a7c30c994fd5d816ccb",
		Size:   8928640,
	}
	// Parakeet TDT 0.6b v3, 8-bit: transcripts match f16 within a few words at half the size.
	parakeetModel = artifact{
		Name:   "ggml-parakeet-tdt-0.6b-v3-q8_0.bin",
		URL:    "https://huggingface.co/ggml-org/parakeet-GGUF/resolve/35156454d1a39de06863303dd209fd2bed6ee079/ggml-parakeet-tdt-0.6b-v3-q8_0.bin",
		SHA256: "4d64e9e96c2792186d072fde0034df0ad670cf680a2f53069052ead827fd600e",
		Size:   668757119,
	}
	whisperModel = artifact{
		Name:   "ggml-large-v3-turbo-q8_0.bin",
		URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-large-v3-turbo-q8_0.bin",
		SHA256: "317eb69c11673c9de1e1f0d459b253999804ec71ac4c23c17ecf5fbe24e259a1",
		Size:   874188075,
	}
)

// binDir holds the extracted whisper.cpp release.
func binDir(toolsDir string) string { return filepath.Join(toolsDir, "whisper.cpp-"+whisperCppRelease) }

// modelPath is where a model lives once verified. A file at this path always
// passed its hash check: downloads land under a temporary name first.
func modelPath(toolsDir string, a artifact) string { return filepath.Join(toolsDir, "models", a.Name) }

// SetupOptions configures Setup.
type SetupOptions struct {
	ToolsDir string
	Engine   string // EngineParakeet, EngineWhisper or "all"
	// SourceDirs are searched for already downloaded copies (same file
	// names); a copy is verified like a download.
	SourceDirs []string
	// FFmpegCandidates are checked when ffmpeg is not on PATH.
	FFmpegCandidates []string
	HTTP             *http.Client
	Out              io.Writer
}

// Setup installs the engine's model into the shared tools directory and, on
// Windows, the pinned whisper.cpp binaries, verifying every file's SHA-256.
// Elsewhere it checks that Homebrew's whisper.cpp tools are found.
func Setup(ctx context.Context, o SetupOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 2 * time.Hour}
	}
	var models []artifact
	switch o.Engine {
	case EngineParakeet:
		models = []artifact{parakeetModel}
	case EngineWhisper:
		models = []artifact{whisperModel}
	case "all":
		models = []artifact{parakeetModel, whisperModel}
	default:
		return fmt.Errorf("unknown engine %q: use parakeet, whisper or all", o.Engine)
	}
	if err := os.MkdirAll(filepath.Join(o.ToolsDir, "models"), 0o755); err != nil {
		return err
	}
	if err := installBinaries(ctx, o); err != nil {
		return err
	}
	for _, m := range models {
		dst := modelPath(o.ToolsDir, m)
		if fi, err := os.Stat(dst); err == nil && fi.Size() == m.Size {
			fmt.Fprintf(o.Out, "Model %s: already installed\n", m.Name)
			continue
		}
		tmp, err := obtain(ctx, o, m)
		if err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "Model %s: installed in %s\n", m.Name, dst)
	}
	if p, err := childproc.FindFFmpeg(nil, o.FFmpegCandidates); err != nil {
		fmt.Fprintln(o.Out, "ffmpeg: NOT FOUND - transcription needs it: "+config.FFmpegInstallHint()+", then restart the daemon")
	} else {
		fmt.Fprintf(o.Out, "ffmpeg: %s\n", p)
	}
	return nil
}

// installBinaries extracts the pinned release unless its marker says it is
// in place. Outside Windows it reports where the Homebrew tools are.
func installBinaries(ctx context.Context, o SetupOptions) error {
	if !pinnedBinaries {
		for _, name := range []string{"parakeet-cli", "whisper-cli"} {
			p := toolExe(o.ToolsDir, name, nil)
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("%s not found on PATH nor in %s: %s", name, strings.Join(toolBinDirs, ", "), toolsHint(""))
			}
			fmt.Fprintf(o.Out, "%s: %s\n", name, p)
		}
		return nil
	}
	dir := binDir(o.ToolsDir)
	marker := filepath.Join(dir, ".sha256")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == whisperCppZip.SHA256 {
		fmt.Fprintf(o.Out, "whisper.cpp %s: already installed\n", whisperCppRelease)
		return nil
	}
	zipPath, err := obtain(ctx, o, whisperCppZip)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	staging := dir + ".tmp"
	os.RemoveAll(staging)
	if err := extractZip(zipPath, staging); err != nil {
		os.RemoveAll(staging)
		return err
	}
	for _, exe := range []string{"parakeet-cli.exe", "whisper-cli.exe"} {
		if _, err := os.Stat(filepath.Join(staging, exe)); err != nil {
			os.RemoveAll(staging)
			return fmt.Errorf("%s is not in %s", exe, whisperCppZip.URL)
		}
	}
	if err := os.WriteFile(filepath.Join(staging, ".sha256"), []byte(whisperCppZip.SHA256+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "whisper.cpp %s: installed in %s\n", whisperCppRelease, dir)
	return nil
}

// obtain returns a verified copy of a under a temporary name in the tools
// directory: from SourceDir when a copy is there, otherwise downloaded.
func obtain(ctx context.Context, o SetupOptions, a artifact) (string, error) {
	tmp := filepath.Join(o.ToolsDir, "models", a.Name+".part")
	if a == whisperCppZip {
		tmp = filepath.Join(o.ToolsDir, a.Name+".part")
	}
	var src io.ReadCloser
	from := a.URL
	for _, dir := range o.SourceDirs {
		if f, err := os.Open(filepath.Join(dir, a.Name)); err == nil {
			src, from = f, f.Name()
			break
		}
	}
	if src == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return "", err
		}
		resp, err := o.HTTP.Do(req)
		if err != nil {
			return "", fmt.Errorf("download %s: %w", a.URL, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("download %s: %s", a.URL, resp.Status)
		}
		src = resp.Body
	}
	defer src.Close()
	fmt.Fprintf(o.Out, "Fetching %s (%s) from %s\n", a.Name, sizeMB(a.Size), from)
	if err := copyVerified(tmp, src, a, o.Out); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func sizeMB(n int64) string { return fmt.Sprintf("%.0f MB", float64(n)/(1<<20)) }

// copyVerified writes src to dst and checks size and SHA-256 against a.
func copyVerified(dst string, src io.Reader, a artifact, out io.Writer) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	h := sha256.New()
	p := &progress{w: out, total: a.Size, next: a.Size / 10}
	n, err := io.Copy(io.MultiWriter(f, h, p), io.LimitReader(src, a.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("fetch %s: %w", a.Name, err)
	}
	if n != a.Size {
		return fmt.Errorf("%s: got %d bytes, want %d", a.Name, n, a.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("%s: SHA-256 %s, want %s", a.Name, got, a.SHA256)
	}
	return nil
}

type progress struct {
	w           io.Writer
	total, done int64
	next        int64
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.next > 0 && p.done >= p.next && p.done < p.total {
		fmt.Fprintf(p.w, "  %d%%\n", p.done*100/p.total)
		p.next += p.total / 10
	}
	return len(b), nil
}

// extractZip unpacks the release, flattening its top "Release/" folder.
// Entries that would leave dir are refused.
func extractZip(path, dir string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range zr.File {
		name := strings.TrimPrefix(filepath.ToSlash(f.Name), "Release/")
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		rel := filepath.FromSlash(name)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("zip entry %q escapes the install dir", f.Name)
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := extractFile(f, dst); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, dst string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, io.LimitReader(r, 1<<30))
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}
