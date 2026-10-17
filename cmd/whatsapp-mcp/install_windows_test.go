//go:build windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// install replaces a binary that is running: the running exe moves aside,
// the new one takes its name, and leftovers go once nothing runs them.
func TestSwapInstallReplacesRunningExe(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, exeName)
	if err := copyFile(self, dst); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(dst)
	running.Env = append(os.Environ(), "WHATSAPP_MCP_TEST_SLEEP=30s")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	defer running.Process.Kill()
	if err := os.Remove(dst); err == nil {
		t.Fatal("a running exe was deleted: the test does not exercise the swap")
	}

	src := filepath.Join(dir, "new.exe")
	if err := os.WriteFile(src, []byte("new binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := swapInstall(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, []byte("new binary")) {
		t.Fatalf("installed content = %.20q", b)
	}
	if img, err := processImage(running.Process.Pid); err != nil || !strings.HasSuffix(img, ".old") {
		t.Fatalf("running image = %q, %v; want the .old copy", img, err)
	}

	// A second install while the first old copy still runs.
	if err := os.WriteFile(src, []byte("newer binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := swapInstall(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, []byte("newer binary")) {
		t.Fatalf("installed content = %.20q", b)
	}

	removeLeftovers(dst)
	if left, _ := filepath.Glob(dst + ".old*"); len(left) != 1 {
		t.Fatalf("leftovers while one runs = %v, want only the running copy", left)
	}
	running.Process.Kill()
	running.Wait()
	// Windows releases the image of an ended process with a short delay.
	var left []string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		removeLeftovers(dst)
		if left, _ = filepath.Glob(dst + ".*"); len(left) == 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(left) != 0 {
		t.Fatalf("leftovers = %v", left)
	}
}
