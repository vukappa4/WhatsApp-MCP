//go:build windows

package outgoing

import "os"

func openNoBlock(path string) (*os.File, error) { return os.Open(path) }
