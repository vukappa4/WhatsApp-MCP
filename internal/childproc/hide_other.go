//go:build !windows

package childproc

import (
	"os"
	"os/exec"
)

// Hide does nothing: outside Windows a child never opens a window.
func Hide(*exec.Cmd) {}

// nice lowers a child's priority; it ships with macOS and Linux.
const nice = "/usr/bin/nice"

// Background runs c below normal priority, through nice. Call it after
// exec.Command and before Start.
func Background(c *exec.Cmd) {
	if _, err := os.Stat(nice); err != nil || c.Err != nil {
		return
	}
	c.Args = append([]string{nice, "-n", "10", c.Path}, c.Args[1:]...)
	c.Path = nice
}

// KillChildrenOnExit does nothing outside Windows. Under launchd the
// children share the daemon's process group, which launchd kills when the
// daemon ends; in a terminal, Ctrl+C reaches the whole group.
func KillChildrenOnExit() error { return nil }
