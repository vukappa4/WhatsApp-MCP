//go:build unix

package main

import (
	"errors"
	"syscall"
	"time"
)

// waitExit waits up to d for process pid to end.
func waitExit(pid int, d time.Duration) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
	}
}
