//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a new process group: Ctrl-C in the console
// no longer reaches it (Ctrl-C is disabled for a new group). It keeps the
// console, so its output still shows there.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
