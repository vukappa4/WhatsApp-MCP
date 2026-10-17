//go:build unix

package instance

import (
	"os"
	"path/filepath"
	"testing"
)

// createLockedDown makes the profile's lock file unreadable to this user,
// as a lock file of another account or of root is.
func createLockedDown(t *testing.T, profile string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	path, err := lockPath(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	return func() { os.Remove(path) }
}
