package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/release"
)

// TestUpdateE2E updates the installed dev profile as a user would, from a
// fake GitHub on 127.0.0.1: it installs a v0.0.1 build, whose update
// replaces itself with a v0.0.2 release, refuses a tampered v0.0.3, and
// leaves prod alone. It reinstalls dev, so it runs only on request, and
// ends by installing a build of the working tree:
//
//	WHATSAPP_MCP_E2E_UPDATE=dev go test ./cmd/whatsapp-mcp -run TestUpdateE2E -v -timeout 30m
func TestUpdateE2E(t *testing.T) {
	if os.Getenv("WHATSAPP_MCP_E2E_UPDATE") != "dev" {
		t.Skip("set WHATSAPP_MCP_E2E_UPDATE=dev to update the installed dev profile")
	}
	const p = config.ProfileDev // never prod
	asset, err := release.AssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	// A test killed by go test -timeout runs no Cleanup: say up front how
	// to put a normal build back.
	fmt.Fprintln(os.Stderr, recoveryHint(filepath.Join(os.TempDir(), "whatsapp-mcp-recover"+filepath.Ext(exeName))))
	root, err := config.AppDir()
	if err != nil {
		t.Fatal(err)
	}
	devCfg, err := config.New(p, "")
	if err != nil {
		t.Fatal(err)
	}
	prodExe, _ := installedExe(config.ProfileProd)
	prodPIDFile := filepath.Join(root, string(config.ProfileProd), "daemon.pid")
	prodSHA, prodPID := fileSHA(prodExe), fileText(prodPIDFile)
	t.Logf("prod: binary SHA-256 %q, daemon.pid %q", prodSHA, prodPID)
	// Registered first, so it runs last: after dev's reinstall too.
	t.Cleanup(func() {
		if sha, pid := fileSHA(prodExe), fileText(prodPIDFile); sha != prodSHA || pid != prodPID {
			t.Errorf("prod changed: binary SHA-256 %q -> %q, daemon.pid %q -> %q", prodSHA, sha, prodPID, pid)
		}
	})

	gh := newFakeGitHub(t, ghRelease{Tag: "v0.0.0"})
	dir := t.TempDir()
	build := func(out, ldflags string) string {
		t.Helper()
		bin := filepath.Join(dir, out)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", out, err, b)
		}
		return bin
	}
	run := func(exe string, args ...string) (string, error) {
		t.Helper()
		b, err := exec.Command(exe, args...).CombinedOutput()
		t.Logf("$ %s %s\n%s(%v)", filepath.Base(exe), strings.Join(args, " "), b, err)
		return string(b), err
	}
	stamp := func(v string) string { return "-X main.version=" + v + " -X main.releaseAPIBase=" + gh.srv.URL }
	newBin := build(filepath.Join("new", asset), stamp("v0.0.2"))
	oldBin := build(filepath.Join("old", exeName), stamp("v0.0.1"))
	newBytes, err := os.ReadFile(newBin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		normal := build(filepath.Join("normal", exeName), "")
		if out, err := run(normal, "install", "--profile", string(p)); err != nil || !strings.Contains(out, "Daemon running from") {
			t.Errorf("reinstalling a normal build of dev failed: %v", err)
		}
	})

	if out, err := run(oldBin, "install", "--profile", string(p)); err != nil || !strings.Contains(out, "Daemon running from") {
		t.Fatalf("install v0.0.1: %v", err)
	}
	devExe, ok := installedExe(p)
	if !ok {
		t.Fatal("dev not installed")
	}
	gh.set(ghRelease{Tag: "v0.0.2", Files: map[string][]byte{asset: newBytes}})

	if out, _ := run(devExe, "update", "--profile", string(p), "--check"); !strings.Contains(out, "Update available: v0.0.1 -> v0.0.2") {
		t.Fatal("--check did not report v0.0.2")
	}
	out, err := run(devExe, "update", "--profile", string(p))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, w := range []string{"Daemon running from " + devExe, "Updated profile dev: v0.0.1 -> v0.0.2."} {
		if !strings.Contains(out, w) {
			t.Fatalf("update output lacks %q", w)
		}
	}
	if v, err := installedVersion(devExe); err != nil || v != "v0.0.2" {
		t.Fatalf("installed version %q, %v", v, err)
	}
	pid, err := daemonPID(devCfg)
	if err != nil {
		t.Fatal(err)
	}
	if img, err := processImage(pid); err != nil || !sameFile(img, devExe) {
		t.Fatalf("dev daemon pid %d runs %q, %v; want %s", pid, img, err, devExe)
	}
	if out, _ := run(devExe, "status", "--profile", string(p)); !strings.Contains(out, "running=yes") {
		t.Fatal("status: dev daemon not running")
	}
	switch runtime.GOOS {
	case "windows":
		// The updater ran from the replaced binary: its .old stays until the next install.
		if _, err := os.Stat(devExe + ".old"); err != nil {
			t.Errorf("no .old after a self-replacing update on Windows: %v", err)
		}
	case "darwin":
		if out, err := exec.Command("codesign", "-v", devExe).CombinedOutput(); err != nil {
			t.Errorf("codesign -v: %v %s", err, out)
		}
		if out, err := exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel(p))).CombinedOutput(); err != nil {
			t.Errorf("launchctl print: %v %s", err, out)
		}
	}
	if out, err := run(devExe, "update", "--profile", string(p)); err != nil || !strings.Contains(out, "Already up to date: v0.0.2.") {
		t.Fatalf("second update: %v", err)
	}

	// v0.0.3 with one byte flipped against its checksums.txt: nothing changes.
	bad := slices.Clone(newBytes)
	bad[len(bad)/2] ^= 1
	gh.set(ghRelease{Tag: "v0.0.3", Files: map[string][]byte{asset: bad}, Sums: map[string][]byte{asset: newBytes}})
	sha := fileSHA(devExe)
	out, err = run(devExe, "update", "--profile", string(p))
	if err == nil || !strings.Contains(out, "checksums.txt says") || !strings.Contains(out, "altered on the way. Nothing was installed.") {
		t.Fatalf("tampered update: %v", err)
	}
	if fileSHA(devExe) != sha {
		t.Fatal("tampered update changed the dev binary")
	}
	if now, err := daemonPID(devCfg); err != nil || now != pid {
		t.Fatalf("tampered update changed the dev daemon: pid %d -> %d, %v", pid, now, err)
	}
	if l, _ := filepath.Glob(filepath.Join(root, "update-*")); len(l) != 0 {
		t.Errorf("download folders left: %v", l)
	}
}

// recoveryHint is how to reinstall a normal build of dev, built to exe,
// after TestUpdateE2E was killed before its Cleanup ran.
func recoveryHint(exe string) string {
	h := "TestUpdateE2E: if this test is killed before it ends, dev keeps a test build that asks a dead fake GitHub. " +
		"Reinstall a normal build of dev from the repository root:\n"
	if runtime.GOOS == "windows" {
		h += fmt.Sprintf("  PowerShell: $env:CGO_ENABLED = \"0\"; go build -o \"%s\" ./cmd/whatsapp-mcp; & \"%s\" install --profile dev\n", exe, exe)
	}
	sh := filepath.ToSlash(exe)
	return h + fmt.Sprintf("  sh: CGO_ENABLED=0 go build -o '%s' ./cmd/whatsapp-mcp && '%s' install --profile dev", sh, sh)
}

// fileSHA is the file's SHA-256, "" when it cannot be read.
func fileSHA(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fileText is the file's trimmed content, "" when it cannot be read.
func fileText(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}
