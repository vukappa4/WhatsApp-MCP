package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// When the rotated name cannot be taken, logging moves to a numbered file
// instead of retrying the rotation on every write.
func TestRotationFallsBackWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	// Non-empty directories on every rotated name make the renames fail.
	for i := 1; i <= keepOlds; i++ {
		if err := os.MkdirAll(filepath.Join(dir, "whatsapp-mcp.log."+string(rune('0'+i)), "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r, err := OpenRotating(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.size = maxSize // force a rotation on the next write
	if _, err := r.Write([]byte("after rotation\n")); err != nil {
		t.Fatal(err)
	}
	if r.path == r.base || !strings.HasPrefix(filepath.Base(r.path), "whatsapp-mcp.") {
		t.Fatalf("path = %s, want a numbered fallback", r.path)
	}
	for range 3 {
		if _, err := r.Write([]byte("more\n")); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(r.path)
	if !strings.Contains(string(b), "after rotation") {
		t.Fatalf("fallback file content = %q", b)
	}
}

// A single write larger than maxSize into an empty log is written as is: no
// rotation of the empty file pushes the oldest real log out.
func TestHugeWriteIntoEmptyLogKeepsOldLogs(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= keepOlds; i++ {
		if err := os.WriteFile(filepath.Join(dir, "whatsapp-mcp.log."+string(rune('0'+i))), []byte("old"+string(rune('0'+i))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := OpenRotating(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Write(make([]byte, maxSize+1)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= keepOlds; i++ {
		name := "whatsapp-mcp.log." + string(rune('0'+i))
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if want := "old" + string(rune('0'+i)); string(b) != want {
			t.Errorf("%s = %q, want %q", name, b, want)
		}
	}
}
