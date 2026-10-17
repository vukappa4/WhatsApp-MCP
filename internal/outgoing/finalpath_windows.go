//go:build windows

package outgoing

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// fileNameNormalizedDOS is FILE_NAME_NORMALIZED | VOLUME_NAME_DOS (both 0):
// the normalized long path with a drive letter.
const fileNameNormalizedDOS = 0

// finalPath asks Windows where the opened file really is: every symlink,
// junction, mount point and short name resolved. It also returns the
// file's hard link count.
func finalPath(f *os.File) (string, uint32, error) {
	h := windows.Handle(f.Fd())
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalizedDOS)
	if err != nil {
		return "", 0, err
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		if n, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalizedDOS); err != nil {
			return "", 0, err
		}
	}
	p := windows.UTF16ToString(buf[:n])
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		p = `\\` + p[len(`\\?\UNC\`):] // a network share: refused by the caller
	case strings.HasPrefix(p, `\\?\`):
		p = p[len(`\\?\`):]
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", 0, err
	}
	return p, info.NumberOfLinks, nil
}
