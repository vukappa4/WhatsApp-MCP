//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own: the terminal
// sends Ctrl-C's SIGINT to the foreground group only, so it no longer
// reaches cmd. cmd still writes to the terminal.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
