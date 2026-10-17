//go:build unix

package outgoing

import (
	"os"
	"syscall"
)

// finalPath asks the kernel where the opened file really is (handlePath:
// every symlink resolved, from the open file itself, so a symlink swapped
// after the open cannot redirect the check). It also returns the file's
// hard link count.
func finalPath(f *os.File) (string, uint32, error) {
	p, err := handlePath(f)
	if err != nil {
		return "", 0, err
	}
	links := uint32(1)
	if st, err := f.Stat(); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			links = uint32(sys.Nlink)
		}
	}
	return p, links, nil
}
