package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/instance"
)

// TestMain doubles as a helper process: with WHATSAPP_MCP_TEST_SLEEP set the
// test binary just sleeps, standing in for a running daemon exe; with
// WHATSAPP_MCP_TEST_FAKE set it stands in for an installed or a release
// binary (fakeBinary). Outside Windows the profile locks live in the app
// dir, so it moves to a temp dir: the real prod and dev locks are never
// touched.
func TestMain(m *testing.M) {
	if d := os.Getenv("WHATSAPP_MCP_TEST_SLEEP"); d != "" {
		dur, _ := time.ParseDuration(d)
		time.Sleep(dur)
		os.Exit(0)
	}
	if os.Getenv("WHATSAPP_MCP_TEST_FAKE") != "" {
		os.Exit(fakeBinary(os.Args[1:]))
	}
	if runtime.GOOS == "windows" {
		os.Exit(m.Run())
	}
	home, err := os.MkdirTemp("", "whatsapp-mcp-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_DATA_HOME", "")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// processImage names the exe a running process started from, and waitExit
// returns as soon as it ends: install and stop rely on both.
func TestProcessImageAndWaitExit(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), exeName)
	if err := copyFile(self, exe); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(exe)
	running.Env = append(os.Environ(), "WHATSAPP_MCP_TEST_SLEEP=30s")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { running.Wait(); close(ended) }() // reaped, so it does not linger as a zombie
	defer running.Process.Kill()
	img, err := processImage(running.Process.Pid)
	if err != nil || !sameFile(img, exe) {
		t.Fatalf("processImage = %q, %v; want %s", img, err, exe)
	}
	running.Process.Kill()
	<-ended
	start := time.Now()
	waitExit(running.Process.Pid, 10*time.Second)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("waitExit took %s for an ended process", took)
	}
}

// A second process refused by the profile lock says so with its own pid and
// states that the running daemon is unaffected.
func TestRefusedSecondInstanceLogsPid(t *testing.T) {
	// A profile name of its own: the real prod and dev locks are never touched.
	cfg := &config.Config{Profile: "unit-test-refusal", DataDir: t.TempDir()}
	held, err := instance.Acquire(string(cfg.Profile))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	log, closeLog, err := openLog(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(cfg, log, "run", "stop the daemon first"); !errors.Is(err, instance.ErrRunning) {
		t.Fatalf("acquire = %v, want ErrRunning", err)
	}
	closeLog()
	b, err := os.ReadFile(filepath.Join(cfg.LogDir(), "whatsapp-mcp.log"))
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	for _, want := range []string{"pid=" + strconv.Itoa(os.Getpid()), "run refused", "unaffected", "level=WARN"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line lacks %q: %s", want, line)
		}
	}
}

// The autostart retries a refused run every minute: it logs once an hour.
func TestRefusalLoggedOncePerHour(t *testing.T) {
	cfg := &config.Config{Profile: "unit-test-refusal-rate", DataDir: t.TempDir()}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if !refusalLogDue(cfg, now) || refusalLogDue(cfg, now.Add(time.Minute)) {
		t.Fatal("second refusal within the hour logged")
	}
	if !refusalLogDue(cfg, now.Add(61*time.Minute)) {
		t.Fatal("refusal after an hour not logged")
	}
}

// stop asks a daemon that does not listen yet (still migrating) again until
// it does, instead of killing it.
func TestStopWaitsForAStartingDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := &config.Config{Profile: "unit-test-stop", DataDir: t.TempDir(), Port: port}
	if _, err := cfg.EnsureToken(); err != nil {
		t.Fatal(err)
	}
	lock, err := instance.Acquire(string(cfg.Profile))
	if err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Bool
	go func() {
		time.Sleep(2 * time.Second) // migrations
		srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			stopped.Store(true)
			lock.Release()
		})}
		go srv.ListenAndServe()
		time.Sleep(5 * time.Second)
		srv.Close()
	}()
	var out bytes.Buffer
	if err := stopDaemon(cfg, 20*time.Second, &out); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() || !strings.Contains(out.String(), "stopped") || strings.Contains(out.String(), "Killed") {
		t.Fatalf("stop output: %s", out.String())
	}
}

func TestInternationalPhone(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "+39 333 123 4567": "393331234567", "0039 333-123.4567": "393331234567",
		" +1 (415) 555-0100 ": "14155550100",
		// As copied from WhatsApp: no-break spaces inside direction marks.
		"\u202a+39\u00a0333\u00a0123\u00a04567\u202c": "393331234567",
		"\u2066+39\u202f333 123 4567\u2069\n":         "393331234567",
	} {
		if got, err := internationalPhone(in); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"333 123 4567", "+39 333 123 4567 ext", "+39+333", "+0 333 123 4567", "+39 12", "00+39 3331234567", "+1234567890123456"} {
		if got, err := internationalPhone(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

// version prints the version alone on a line: the release tag the release
// build stamps (.goreleaser.yaml), dev otherwise. update reads it back. The
// binary is built with the release's own ldflags, which keep the symbol
// table: Microsoft Defender's machine learning flagged a stripped (-s -w)
// build as a trojan, while unstripped builds pass.
func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if err := versionCmd(nil, &out); err != nil || out.String() != "dev\n" {
		t.Fatalf("versionCmd = %q, %v; want \"dev\n\"", out.String(), err)
	}
	if err := versionCmd([]string{"extra"}, &out); err == nil {
		t.Fatal("versionCmd accepted an argument")
	}
	if testing.Short() {
		t.Skip("builds the binary")
	}
	const tag = "v0.2.0-rc.1"
	ldflags := releaseLdflags(t)
	if f := strings.Fields(ldflags); slices.Contains(f, "-s") || slices.Contains(f, "-w") {
		t.Fatalf(".goreleaser.yaml strips the binaries (%s): Defender flags stripped builds", ldflags)
	}
	bin := filepath.Join(t.TempDir(), exeName)
	build := exec.Command("go", "build", "-trimpath", "-ldflags", strings.ReplaceAll(ldflags, "{{ .Tag }}", tag), "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	for _, arg := range []string{"version", "--version", "-v"} {
		b, err := exec.Command(bin, arg).Output()
		if err != nil || string(b) != tag+"\n" {
			t.Errorf("%s = %q, %v; want %q", arg, b, err, tag+"\n")
		}
	}
	if v, err := installedVersion(bin); err != nil || v != tag {
		t.Errorf("installedVersion = %q, %v; want %s", v, err, tag)
	}
}

// releaseLdflags is the one ldflags entry of the release build in
// .goreleaser.yaml.
func releaseLdflags(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "ldflags:" {
			continue
		}
		for _, e := range lines[i+1:] {
			v, ok := strings.CutPrefix(strings.TrimSpace(e), "- ")
			if !ok {
				break
			}
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		t.Fatalf(".goreleaser.yaml: want one ldflags entry, found %q", found)
	}
	return found[0]
}
