package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/instance"
	"github.com/vukappa4/whatsapp-mcp/internal/release"
)

// fakeBinary stands in for an installed or a release binary when
// WHATSAPP_MCP_TEST_FAKE is set. version prints <exe>.fakeversion, failing
// like a binary built before the version command when that file is empty;
// without the file it prints WHATSAPP_MCP_TEST_FAKE_VERSION, the version
// of the downloaded release. install writes its arguments to the file
// WHATSAPP_MCP_TEST_FAKE_ARGV and exits with WHATSAPP_MCP_TEST_FAKE_EXIT.
func fakeBinary(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		return 3
	}
	switch {
	case len(args) == 1 && args[0] == "version":
		v := os.Getenv("WHATSAPP_MCP_TEST_FAKE_VERSION")
		if b, err := os.ReadFile(exe + ".fakeversion"); err == nil {
			v = strings.TrimSpace(string(b))
		}
		if v == "" {
			fmt.Fprintln(os.Stderr, `unknown command "version"`)
			return 2
		}
		fmt.Println(v)
		return 0
	case len(args) > 0 && args[0] == "install":
		if err := os.WriteFile(os.Getenv("WHATSAPP_MCP_TEST_FAKE_ARGV"), []byte(strings.Join(args, " ")), 0o600); err != nil {
			return 3
		}
		fmt.Println("Daemon running from", exe)
		code, _ := strconv.Atoi(os.Getenv("WHATSAPP_MCP_TEST_FAKE_EXIT"))
		return code
	}
	return 2
}

// ghRelease is a release the fake GitHub serves. checksums.txt lists the
// SHA-256 of Sums (nil: of Files) unless NoChecksums.
type ghRelease struct {
	Tag         string
	Prerelease  bool
	Files       map[string][]byte
	Sums        map[string][]byte
	NoChecksums bool
}

// fakeGitHub serves one release like the GitHub API (latest and by tag)
// and its downloads, and records every path asked for.
type fakeGitHub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	rel     ghRelease
	paths   []string
	status  int // answer every API call with it
	headers map[string]string
}

func newFakeGitHub(t *testing.T, rel ghRelease) *fakeGitHub {
	g := &fakeGitHub{rel: rel}
	g.srv = httptest.NewServer(g)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) set(rel ghRelease) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rel = rel
}

func (g *fakeGitHub) requested() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.paths)
}

func checksums(files map[string][]byte) []byte {
	var b bytes.Buffer
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range files {
			if !yield(n) {
				return
			}
		}
	}) {
		s := sha256.Sum256(files[name])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(s[:]), name)
	}
	return b.Bytes()
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.paths = append(g.paths, r.URL.Path)
	rel, status, headers := g.rel, g.status, g.headers
	g.mu.Unlock()
	files := map[string][]byte{}
	for n, b := range rel.Files {
		files[n] = b
	}
	if !rel.NoChecksums {
		sums := rel.Sums
		if sums == nil {
			sums = rel.Files
		}
		files[release.ChecksumsName] = checksums(sums)
	}
	api := "/repos/" + releaseRepo + "/releases/"
	switch {
	case strings.HasPrefix(r.URL.Path, api) && status != 0:
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
	case r.URL.Path == api+"latest" || r.URL.Path == api+"tags/"+rel.Tag:
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int    `json:"size"`
		}
		assets := []asset{}
		for n, b := range files {
			assets = append(assets, asset{n, g.srv.URL + "/download/" + rel.Tag + "/" + n, len(b)})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": rel.Tag, "prerelease": rel.Prerelease, "draft": false,
			"html_url": "https://github.com/" + releaseRepo + "/releases/tag/" + rel.Tag, "assets": assets})
	case strings.HasPrefix(r.URL.Path, "/download/"+rel.Tag+"/") && files[path.Base(r.URL.Path)] != nil:
		w.Write(files[path.Base(r.URL.Path)])
	default:
		http.NotFound(w, r)
	}
}

var selfBytes = sync.OnceValues(func() ([]byte, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(self)
})

const testProfile config.Profile = "unit-test-update"

// updateEnv is an installed profile of its own, in a temporary app dir,
// whose binary is this test binary in fake mode reporting installedVer,
// and a fake GitHub whose latest release, tag, is this test binary too.
type updateEnv struct {
	o         updateOptions
	out       bytes.Buffer
	gh        *fakeGitHub
	root      string
	installed string
	argv      string
	asset     string
	bin       []byte
}

func newUpdateEnv(t *testing.T, installedVer, tag string) *updateEnv {
	t.Helper()
	dir := t.TempDir()
	tempAppDir(t, dir)
	e := &updateEnv{argv: filepath.Join(dir, "install-argv")}
	t.Setenv("WHATSAPP_MCP_TEST_FAKE", "1")
	t.Setenv("WHATSAPP_MCP_TEST_FAKE_VERSION", tag)
	t.Setenv("WHATSAPP_MCP_TEST_FAKE_ARGV", e.argv)
	t.Setenv("WHATSAPP_MCP_TEST_FAKE_EXIT", "0")
	var err error
	if e.root, err = config.AppDir(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Profile: testProfile, DataDir: filepath.Join(e.root, string(testProfile)), Port: 1}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if installedVer != "-" {
		bin, err := config.BinDir(testProfile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		e.installed = filepath.Join(bin, exeName)
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := copyFile(self, e.installed); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.installed+".fakeversion", []byte(installedVer), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if e.asset, err = release.AssetName(runtime.GOOS, runtime.GOARCH); err != nil {
		t.Skip(err)
	}
	if e.bin, err = selfBytes(); err != nil {
		t.Fatal(err)
	}
	e.gh = newFakeGitHub(t, ghRelease{Tag: tag, Files: map[string][]byte{e.asset: e.bin}})
	e.o = updateOptions{cfg: cfg, APIBase: e.gh.srv.URL, Out: &e.out}
	return e
}

func (e *updateEnv) run() error {
	return update(context.Background(), e.o)
}

// installArgs is what the release binary's install was run with; "" when
// it did not run.
func (e *updateEnv) installArgs(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.argv)
	if os.IsNotExist(err) {
		return ""
	} else if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// leftovers are the update download folders still in the app dir.
func (e *updateEnv) leftovers() []string {
	l, _ := filepath.Glob(filepath.Join(e.root, "update-*"))
	return l
}

// A newer release is downloaded, verified against checksums.txt, run once
// to check its version, then its own install replaces the binary; the
// download folder goes.
func TestUpdateInstallsVerifiedRelease(t *testing.T) {
	e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
	if err := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, e.out.String())
	}
	want := []string{
		fmt.Sprintf("Profile unit-test-update: installed v0.2.0 (%s). Latest release: v0.3.0.\n", e.installed),
		fmt.Sprintf("Downloading %s v0.3.0 (%d MB)...\n", e.asset, (len(e.bin)+500_000)/1_000_000),
		"SHA-256 verified against checksums.txt.\nInstalling v0.3.0 with the new binary:\nDaemon running from ",
		"Updated profile unit-test-update: v0.2.0 -> v0.3.0.\n",
	}
	for _, w := range want {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, e.out.String())
		}
	}
	if got := e.installArgs(t); got != "install --profile unit-test-update" {
		t.Errorf("install ran as %q", got)
	}
	if l := e.leftovers(); len(l) != 0 {
		t.Errorf("download folders left: %v", l)
	}
}

// --data-dir and --debug reach the install; stale download folders of
// killed updates go, recent ones (another update running) stay.
func TestUpdateForwardsFlagsAndCleansStaleDownloads(t *testing.T) {
	e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
	stale, recent := filepath.Join(e.root, "update-111"), filepath.Join(e.root, "update-222")
	for _, d := range []string{stale, recent} {
		if err := os.MkdirAll(filepath.Join(d, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	e.o.c = common{dataDir: filepath.Join(e.root, "elsewhere"), debug: true}
	if err := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, e.out.String())
	}
	if got, want := e.installArgs(t), "install --profile unit-test-update --data-dir "+filepath.Join(e.root, "elsewhere")+" --debug"; got != want {
		t.Errorf("install ran as %q, want %q", got, want)
	}
	if l := e.leftovers(); len(l) != 1 || l[0] != recent {
		t.Errorf("download folders = %v, want only %s", l, recent)
	}
}

// Up to date, nothing is downloaded: one API call, nothing more.
func TestUpdateAlreadyUpToDate(t *testing.T) {
	e := newUpdateEnv(t, "v0.3.0", "v0.3.0")
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.out.String(), "Already up to date: v0.3.0.\n") {
		t.Errorf("output:\n%s", e.out.String())
	}
	if p := e.gh.requested(); len(p) != 1 || !strings.HasSuffix(p[0], "/releases/latest") {
		t.Errorf("requests %v", p)
	}
	if e.installArgs(t) != "" {
		t.Error("install ran")
	}
}

// --check reports and downloads nothing.
func TestUpdateCheck(t *testing.T) {
	e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
	e.o.Check = true
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	want := "Update available: v0.2.0 -> v0.3.0 (https://github.com/vukappa4/WhatsApp-MCP/releases/tag/v0.3.0). Run `whatsapp-mcp update --profile unit-test-update`.\n"
	if !strings.HasSuffix(e.out.String(), want) {
		t.Errorf("output:\n%s\nwant suffix %s", e.out.String(), want)
	}
	if p := e.gh.requested(); len(p) != 1 || e.installArgs(t) != "" || len(e.leftovers()) != 0 {
		t.Errorf("--check went further: requests %v", p)
	}
}

// A build from source, or one from before the version command, cannot be
// compared with releases: update refuses unless --force.
func TestUpdateRefusesUncomparableWithoutForce(t *testing.T) {
	for ver, says := range map[string]string{"dev": "version dev (built from source)", "": "version unknown (built before the version command)"} {
		t.Run(says, func(t *testing.T) {
			e := newUpdateEnv(t, ver, "v0.3.0")
			err := e.run()
			want := "the installed binary is " + says + ", which cannot be compared with releases: " +
				"run `whatsapp-mcp update --force --profile unit-test-update` to replace it with release v0.3.0"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v\nwant %s", err, want)
			}
			if e.installArgs(t) != "" || len(e.gh.requested()) != 1 {
				t.Fatal("went further than the release lookup")
			}
			e.o.Force = true
			if err := e.run(); err != nil {
				t.Fatalf("--force: %v\n%s", err, e.out.String())
			}
			if e.installArgs(t) != "install --profile unit-test-update" {
				t.Fatal("--force did not install")
			}
		})
	}
}

// An installed version newer than the latest release stays; --version
// installs an older release on purpose, with a warning.
func TestUpdateInstalledNewer(t *testing.T) {
	e := newUpdateEnv(t, "v0.4.0", "v0.3.0")
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.out.String(), "The installed v0.4.0 is newer than the latest release v0.3.0: nothing to do.\n") || e.installArgs(t) != "" {
		t.Fatalf("output:\n%s", e.out.String())
	}
	e.o.Tag = "v0.3.0"
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Requested release: v0.3.0.", "Installing v0.3.0, older than the installed v0.4.0. If v0.4.0 changed the database, v0.3.0 will not start"} {
		if !strings.Contains(e.out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, e.out.String())
		}
	}
	if e.installArgs(t) == "" {
		t.Error("--version did not install")
	}
	if p := e.gh.requested(); !slices.ContainsFunc(p, func(s string) bool { return strings.HasSuffix(s, "/releases/tags/v0.3.0") }) {
		t.Errorf("requests %v", p)
	}
}

// A download that does not match checksums.txt, or a binary that does not
// report the release's version, never reaches install.
func TestUpdateRefusesBadDownloads(t *testing.T) {
	t.Run("tampered", func(t *testing.T) {
		e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
		bad := slices.Clone(e.bin)
		bad[len(bad)/2] ^= 1
		e.gh.set(ghRelease{Tag: "v0.3.0", Files: map[string][]byte{e.asset: bad}, Sums: map[string][]byte{e.asset: e.bin}})
		err := e.run()
		if err == nil || !strings.HasPrefix(err.Error(), e.asset+": SHA-256 ") ||
			!strings.HasSuffix(err.Error(), ": the download is corrupted or was tampered with. Nothing was installed.") {
			t.Fatalf("err = %v", err)
		}
		if e.installArgs(t) != "" || len(e.leftovers()) != 0 {
			t.Fatal("install ran or the download stayed")
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
		t.Setenv("WHATSAPP_MCP_TEST_FAKE_VERSION", "v0.0.9")
		err := e.run()
		if err == nil || err.Error() != fmt.Sprintf(`the downloaded %s does not run as v0.3.0 (it reports "v0.0.9"): nothing was installed`, e.asset) {
			t.Fatalf("err = %v", err)
		}
		if e.installArgs(t) != "" || len(e.leftovers()) != 0 {
			t.Fatal("install ran or the download stayed")
		}
	})
}

func TestUpdateIncompleteRelease(t *testing.T) {
	for name, tc := range map[string]struct {
		rel  func(e *updateEnv) ghRelease
		want string
	}{
		"no binary": {func(e *updateEnv) ghRelease {
			return ghRelease{Tag: "v0.3.0", Files: map[string][]byte{"whatsapp-mcp_plan9_amd64": []byte("x")}}
		}, "release v0.3.0 has no {asset} (it has: "},
		"no checksums": {func(e *updateEnv) ghRelease {
			return ghRelease{Tag: "v0.3.0", Files: map[string][]byte{e.asset: e.bin}, NoChecksums: true}
		}, "release v0.3.0 has no checksums.txt"},
		"binary not listed": {func(e *updateEnv) ghRelease {
			return ghRelease{Tag: "v0.3.0", Files: map[string][]byte{e.asset: e.bin}, Sums: map[string][]byte{"other": nil}}
		}, "checksums.txt of release v0.3.0 does not list {asset}: nothing was installed"},
		"prerelease as latest": {func(e *updateEnv) ghRelease {
			return ghRelease{Tag: "v0.3.0-rc.1", Prerelease: true, Files: map[string][]byte{e.asset: e.bin}}
		}, "GitHub named v0.3.0-rc.1, a draft or prerelease, as the latest release"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
			e.gh.set(tc.rel(e))
			want := strings.ReplaceAll(tc.want, "{asset}", e.asset)
			if err := e.run(); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if e.installArgs(t) != "" {
				t.Fatal("install ran")
			}
		})
	}
}

// A failed install is an error of the update too.
func TestUpdateInstallFails(t *testing.T) {
	e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
	t.Setenv("WHATSAPP_MCP_TEST_FAKE_EXIT", "1")
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "the new binary's install failed (exit status 1)") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(e.out.String(), "Updated profile") || e.installArgs(t) == "" || len(e.leftovers()) != 0 {
		t.Fatalf("output:\n%s", e.out.String())
	}
}

func TestUpdateNotInstalled(t *testing.T) {
	e := newUpdateEnv(t, "-", "v0.3.0")
	err := e.run()
	if err == nil || err.Error() != "profile unit-test-update is not installed: run `whatsapp-mcp install --profile unit-test-update` first" {
		t.Fatalf("err = %v", err)
	}
	if len(e.gh.requested()) != 0 {
		t.Fatal("asked GitHub")
	}
	e.o.Check = true
	if err := e.run(); err != nil || !strings.HasPrefix(e.out.String(), "Profile unit-test-update: not installed. Latest release: v0.3.0.\n") {
		t.Fatalf("--check: %v\n%s", err, e.out.String())
	}
}

// A pair, reingest or unlink holding the profile (no daemon.pid) would
// make install wait minutes for nothing: update refuses before asking
// GitHub.
func TestUpdateRefusesWhileAnotherCommandHoldsTheProfile(t *testing.T) {
	e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
	lock, err := instance.Acquire(string(testProfile))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "holds profile unit-test-update: let it finish") {
		t.Fatalf("err = %v", err)
	}
	if len(e.gh.requested()) != 0 {
		t.Fatal("asked GitHub")
	}
}

// GitHub's rate limit and an unreachable GitHub say what to do.
func TestUpdateGitHubFailures(t *testing.T) {
	e := newUpdateEnv(t, "v0.2.0", "v0.3.0")
	reset := time.Date(2026, 10, 18, 15, 42, 0, 0, time.Local)
	e.gh.mu.Lock()
	e.gh.status, e.gh.headers = 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}
	e.gh.mu.Unlock()
	err := e.run()
	want := "GitHub's API rate limit is reached (60 requests an hour per network without login). Try again after 15:42, or download " +
		e.asset + " from https://github.com/vukappa4/WhatsApp-MCP/releases/latest and run its install --profile unit-test-update."
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(err.Error(), "curl -LO https://github.com/vukappa4/WhatsApp-MCP/releases/latest/download/"+e.asset) {
		t.Fatalf("no curl hint on macOS: %v", err)
	}
	e.gh.srv.Close()
	if err := e.run(); err == nil || !strings.HasPrefix(err.Error(), "contact GitHub: ") || !strings.HasSuffix(err.Error(), ". Behind a proxy, set HTTPS_PROXY.") {
		t.Fatalf("closed server: %v", err)
	}
}

// After an update on macOS the user is told that Full Disk Access no
// longer applies to the binary; Windows needs nothing.
func TestUpdateNotes(t *testing.T) {
	dst := "/Users/me/Library/Application Support/WhatsAppMCP/bin-dev/whatsapp-mcp"
	n := updateNotes("darwin", dst, config.ProfileDev)
	for _, w := range []string{"Full Disk Access", "remove it and add it again", "Cmd+Shift+G", `run "` + dst + `" install --profile dev`, "Desktop, Documents or Downloads"} {
		if !strings.Contains(n, w) {
			t.Errorf("darwin notes lack %q: %s", w, n)
		}
	}
	if n := updateNotes("windows", `C:\x\whatsapp-mcp.exe`, config.ProfileDev); n != "" {
		t.Errorf("windows notes: %s", n)
	}
}
