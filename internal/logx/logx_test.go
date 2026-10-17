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
