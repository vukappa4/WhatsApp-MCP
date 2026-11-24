//go:build !windows

package useracl

import (
	"fmt"
	"os"
)

// Restrict makes path readable by its owner only.
func Restrict(path string, dir bool) error {
	if dir {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}

// CurrentUserSID is empty outside Windows.
func CurrentUserSID() (string, error) { return "", nil }

// Check returns an error when group or others may access path.
func Check(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s: mode %v, want owner only", path, st.Mode().Perm())
	}
	return nil
}
