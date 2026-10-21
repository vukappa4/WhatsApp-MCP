package media

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
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

// whisperCppRelease is the pinned whisper.cpp release. On Windows it is
// ggml-org's own build, which ships parakeet-cli.exe and whisper-cli.exe; on
// macOS this project builds the same commit (.github/workflows/tools.yml).
const whisperCppRelease = "b5454"

const whisperCppDir = "whisper.cpp-" + whisperCppRelease

// toolsRelease is where this project publishes its macOS builds: the
// release tools.yml made for whisperCppRelease.
const toolsRelease = "https://github.com/vukappa4/WhatsApp-MCP/releases/download/tools-whisper.cpp-" + whisperCppRelease + "-2/"

// toolsArtifact is an archive of the tools release.
func toolsArtifact(name, sha256 string, size int64) artifact {
	return artifact{Name: name, URL: toolsRelease + name, SHA256: sha256, Size: size}
}

// darwinTools are the macOS archives by GOARCH: parakeet-cli and
// whisper-cli (Metal on arm64), and ffmpeg with libopus.
var darwinTools = map[string]struct{ whisperCpp, ffmpeg artifact }{
	"arm64": {
		whisperCpp: toolsArtifact(whisperCppDir+"-darwin-arm64.tar.gz", "236074595d879337895ca2380404c264211b8dc605657ea6f870c69d9ef67c80", 4005444),
		ffmpeg:     toolsArtifact(config.PinnedFFmpegDir+"-darwin-arm64.tar.gz", "d3c0e532ac72029f6b6d807e8a5a7b815c33ce0332e45115ed0648a2e20189e5", 1784718),
	},
	"amd64": {
		whisperCpp: toolsArtifact(whisperCppDir+"-darwin-amd64.tar.gz", "0207605aee6dd1f152a9f153e91274070c4acd78baf0ad2da7cd8e97735ec73e", 2429630),
		ffmpeg:     toolsArtifact(config.PinnedFFmpegDir+"-darwin-amd64.tar.gz", "82e936807ac444d3007c61e8fd56d777ae22d27b99f8d63d5a2b9fb8565e2ddf", 1837379),
	},
}

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

// bundle is a pinned archive of programs, extracted into its own folder of
// the tools dir.
type bundle struct {
	artifact
	Dir  string   // folder in the tools dir
	Exes []string // programs it must hold
}

// toolBundles are the archives setup-transcription installs on goos and
// goarch; nil where the whisper.cpp tools are not pinned.
func toolBundles(goos, goarch string) []bundle {
	switch goos {
	case "windows":
		return []bundle{{artifact: whisperCppZip, Dir: whisperCppDir, Exes: []string{"parakeet-cli.exe", "whisper-cli.exe"}}}
	case "darwin":
		t, ok := darwinTools[goarch]
		if !ok {
			return nil
		}
		return []bundle{
			{artifact: t.whisperCpp, Dir: whisperCppDir, Exes: []string{"parakeet-cli", "whisper-cli"}},
			{artifact: t.ffmpeg, Dir: config.PinnedFFmpegDir, Exes: []string{"ffmpeg"}},
		}
	}
	return nil
}

// binDir holds the extracted whisper.cpp tools.
func binDir(toolsDir string) string { return filepath.Join(toolsDir, whisperCppDir) }

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
	// FFmpegSearch is where ffmpeg is looked for.
	FFmpegSearch config.FFmpegSearch
	HTTP         *http.Client
	Out          io.Writer
}

// Setup installs the engine's model into the shared tools directory and, on
// Windows and macOS, the pinned whisper.cpp tools (on macOS also ffmpeg),
// verifying every file's SHA-256. Elsewhere it checks that the engine's
// whisper.cpp tool is found.
func Setup(ctx context.Context, o SetupOptions) error {
	o = o.withDefaults()
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
	if err := installTools(ctx, o); err != nil {
		return err
	}
	for _, m := range models {
		dst := modelPath(o.ToolsDir, m)
		if fi, err := os.Stat(dst); err == nil && fi.Size() == m.Size {
			fmt.Fprintf(o.Out, "Model %s: already installed\n", m.Name)
			continue
		}
		tmp, err := obtain(ctx, o, m, filepath.Join(o.ToolsDir, "models"))
		if err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "Model %s: installed in %s\n", m.Name, dst)
	}
	if p, err := childproc.FindFFmpeg(nil, o.FFmpegSearch); err != nil {
		fmt.Fprintln(o.Out, "ffmpeg: NOT FOUND - transcription needs it: "+config.FFmpegInstallHint()+", then restart the daemon")
	} else {
		fmt.Fprintf(o.Out, "ffmpeg: %s\n", p)
	}
	return nil
}

func (o SetupOptions) withDefaults() SetupOptions {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 2 * time.Hour}
	}
	return o
}

// RefreshTools installs the pinned bundles (see toolBundles) that are
// missing from o.ToolsDir or older than this version's pins, when
// transcription is set up there: a model setup-transcription installs is in
// place. A new version can pin new tools, and the daemon runs only the
// pinned ones: install calls this, so an update keeps transcribing. It
// prints nothing when there is nothing to do; its error names the command
// that installs everything.
func RefreshTools(ctx context.Context, o SetupOptions) error {
	return refreshTools(ctx, o, toolBundles(runtime.GOOS, runtime.GOARCH))
}

func refreshTools(ctx context.Context, o SetupOptions, bundles []bundle) error {
	o = o.withDefaults()
	engine := setUpEngine(o.ToolsDir)
	if engine == "" {
		return nil
	}
	var stale []bundle
	for _, b := range bundles {
		if !installed(filepath.Join(o.ToolsDir, b.Dir), b) {
			stale = append(stale, b)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	fmt.Fprintln(o.Out, "Transcription is set up: installing the programs this version runs.")
	for _, b := range stale {
		if err := installBundle(ctx, o, b); err != nil {
			return fmt.Errorf("%w; run `whatsapp-mcp setup-transcription --engine %s`", err, engine)
		}
	}
	return nil
}

// setUpEngine is the engine choice whose models are installed in toolsDir:
// parakeet, whisper, all, or "" when transcription was never set up there.
func setUpEngine(toolsDir string) string {
	has := func(m artifact) bool {
		fi, err := os.Stat(modelPath(toolsDir, m))
		return err == nil && fi.Size() == m.Size
	}
	p, w := has(parakeetModel), has(whisperModel)
	switch {
	case p && w:
		return "all"
	case p:
		return EngineParakeet
	case w:
		return EngineWhisper
	}
	return ""
}

// installTools installs every pinned bundle. Where the tools are not
// pinned it reports where the engine's tools are.
func installTools(ctx context.Context, o SetupOptions) error {
	bundles := toolBundles(runtime.GOOS, runtime.GOARCH)
	if bundles == nil {
		for _, name := range engineTools(o.Engine) {
			p := toolExe(o.ToolsDir, name, nil)
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("%s not found on PATH nor in %s: %s", name, strings.Join(toolBinDirs, ", "), toolsHint(o.Engine))
			}
			fmt.Fprintf(o.Out, "%s: %s\n", name, p)
		}
		return nil
	}
	for _, b := range bundles {
		if err := installBundle(ctx, o, b); err != nil {
			return err
		}
	}
	return nil
}

// installBundle extracts b into its folder unless that folder already holds
// b (see installed).
func installBundle(ctx context.Context, o SetupOptions, b bundle) error {
	dir := filepath.Join(o.ToolsDir, b.Dir)
	if installed(dir, b) {
		fmt.Fprintf(o.Out, "%s: already installed\n", b.Dir)
		return nil
	}
	archive, err := obtain(ctx, o, b.artifact, o.ToolsDir)
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	staging := dir + ".tmp"
	os.RemoveAll(staging)
	extract := extractZip
	if strings.HasSuffix(b.Name, ".tar.gz") {
		extract = extractTarGz
	}
	if err := extract(archive, staging); err != nil {
		os.RemoveAll(staging)
		return err
	}
	for _, exe := range b.Exes {
		if fi, err := os.Stat(filepath.Join(staging, exe)); err != nil || !fi.Mode().IsRegular() {
			os.RemoveAll(staging)
			return fmt.Errorf("%s is not in %s", exe, b.URL)
		}
	}
	if err := os.WriteFile(filepath.Join(staging, ".sha256"), []byte(b.SHA256+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "%s: installed in %s\n", b.Dir, dir)
	return nil
}

// installed reports whether dir holds b: its .sha256 marker names b's
// archive and every program is there.
func installed(dir string, b bundle) bool {
	m, err := os.ReadFile(filepath.Join(dir, ".sha256"))
	if err != nil || strings.TrimSpace(string(m)) != b.SHA256 {
		return false
	}
	for _, exe := range b.Exes {
		if fi, err := os.Stat(filepath.Join(dir, exe)); err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// obtain returns a verified copy of a under a temporary name in dir: from
// SourceDirs when a copy is there, otherwise downloaded.
func obtain(ctx context.Context, o SetupOptions, a artifact, dir string) (string, error) {
	tmp := filepath.Join(dir, a.Name+".part")
	var src io.ReadCloser
	from := a.URL
	for _, d := range o.SourceDirs {
		if f, err := os.Open(filepath.Join(d, a.Name)); err == nil {
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

// extractZip unpacks the Windows release, flattening its top "Release/"
// folder. Entries that would leave dir are refused.
func extractZip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
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
		r, err := f.Open()
		if err != nil {
			return err
		}
		err = writeExtracted(dst, r, 0o644)
		r.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// extractTarGz unpacks a macOS tools archive: folders and regular files
// only, and a file with any executable bit becomes 0755. Entries that would
// leave dir, links and devices are refused.
func extractTarGz(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if name == "." || h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		rel := filepath.FromSlash(name)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("archive entry %q escapes the install dir", h.Name)
		}
		dst := filepath.Join(dir, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			if err := writeExtracted(dst, tr, mode); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q is not a file or folder", h.Name)
		}
	}
}

// writeExtracted writes up to 1 GiB of r to dst, created with mode.
func writeExtracted(dst string, r io.Reader, mode os.FileMode) error {
	w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, io.LimitReader(r, 1<<30))
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}
