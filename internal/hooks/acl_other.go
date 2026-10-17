//go:build !windows

package hooks

import "os"

// restrictToUser makes path readable by its owner only.
func restrictToUser(path string, dir bool) error {
	if dir {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}

func currentUserSID() (string, error) { return "", nil }

func fileDACL(string) (string, error) { return "", nil }
