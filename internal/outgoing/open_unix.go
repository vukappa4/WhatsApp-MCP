//go:build unix

package outgoing

import (
	"os"
	"syscall"
)

// openNoBlock opens path for reading without waiting: a named pipe would
// block the open until a writer comes. Reads of a regular file are not
// affected; anything else is refused by checkOpened.
func openNoBlock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
