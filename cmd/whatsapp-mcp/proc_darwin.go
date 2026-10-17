//go:build darwin

package main

import (
	"bytes"
	"fmt"

	"golang.org/x/sys/unix"
)

// processImage is the path a running process was started from: the
// executable path the kernel keeps in front of its arguments
// (kern.procargs2: argc, then that path). launchd starts the daemon by its
// absolute path. It is the path at start: a process whose binary was moved
// aside since (swapInstall's .old) still reports the original name, so
// install stops the old daemon before it waits for the new one.
func processImage(pid int) (string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", fmt.Errorf("read pid %d: %w", pid, err)
	}
	if len(buf) < 5 {
		return "", fmt.Errorf("read pid %d: short kern.procargs2", pid)
	}
	path := buf[4:]
	if i := bytes.IndexByte(path, 0); i >= 0 {
		path = path[:i]
	}
	if len(path) == 0 {
		return "", fmt.Errorf("read pid %d: no executable path", pid)
	}
	return string(path), nil
}
