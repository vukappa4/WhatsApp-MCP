//go:build darwin

package outgoing

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// handlePath is fcntl(F_GETPATH): the path the kernel has for the open file.
func handlePath(f *os.File) (string, error) {
	var buf [unix.PathMax]byte
	if _, _, errno := unix.Syscall(unix.SYS_FCNTL, f.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	n := bytes.IndexByte(buf[:], 0)
	if n < 0 {
		n = len(buf)
	}
	return string(buf[:n]), nil
}
