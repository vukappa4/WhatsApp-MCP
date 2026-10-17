//go:build linux

package outgoing

import (
	"os"
	"strconv"
)

// handlePath reads /proc/self/fd: the path the kernel has for the open file.
func handlePath(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
}
