//go:build unix && !darwin && !linux

package outgoing

import (
	"os"
	"path/filepath"
)

// handlePath resolves the name the file was opened by: without a kernel
// call for the open file, a symlink swapped after the open goes unseen.
func handlePath(f *os.File) (string, error) {
	p, err := filepath.EvalSymlinks(f.Name())
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}
