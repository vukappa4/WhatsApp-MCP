//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/vukappa4/whatsapp-mcp/internal/childproc"
	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

const exeName = "whatsapp-mcp.exe"

func serviceName(p config.Profile) string { return fmt.Sprintf("task %q", taskName(p)) }

// registerService creates or replaces the profile's Task Scheduler task.
func registerService(cfg *config.Config, exe, workDir string) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	def, err := taskXML(u.Username, exe, workDir, cfg.Profile, time.Now())
	if err != nil {
		return "", err
	}
	name := taskName(cfg.Profile)
	if err := registerTask(name, def); err != nil {
		return "", err
	}
	return fmt.Sprintf("Task %q: starts at logon of %s, restarts after a failure every minute, watchdog every 5 minutes.", name, u.Username), nil
}

func startService(cfg *config.Config) error {
	_, err := schtasks("/Run", "/TN", taskName(cfg.Profile))
	return err
}

// removeService deletes the task, so its watchdog cannot restart the daemon.
func removeService(cfg *config.Config) (bool, error) {
	if !serviceInstalled(cfg.Profile) {
		return false, nil
	}
	if _, err := schtasks("/Delete", "/TN", taskName(cfg.Profile), "/F"); err != nil {
		return false, err
	}
	return true, nil
}

func serviceRestartNote(p config.Profile) string {
	return fmt.Sprintf("Task %q starts it again within 5 minutes", taskName(p))
}

// waitExit waits up to d for process pid to end.
func waitExit(pid int, d time.Duration) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return // already gone
	}
	defer windows.CloseHandle(h)
	_, _ = windows.WaitForSingleObject(h, uint32(d.Milliseconds()))
}

// processAlive reports whether process pid runs. A process that ended
// stays openable while a handle to it is open: it is signalled.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// processImage is the full path of a running process's executable.
func processImage(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

func schtasks(args ...string) (string, error) {
	cmd := exec.Command("schtasks.exe", args...)
	childproc.Hide(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("schtasks %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// registerTask creates or replaces the task from its XML definition.
func registerTask(name string, def []byte) error {
	f, err := os.CreateTemp("", "whatsapp-mcp-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(utf16LE(def)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = schtasks("/Create", "/TN", name, "/XML", f.Name(), "/F")
	return err
}

func serviceInstalled(p config.Profile) bool {
	_, err := schtasks("/Query", "/TN", taskName(p))
	return err == nil
}

// serviceState is the task's name and Task Scheduler status for status output.
func serviceState(p config.Profile) string {
	out, err := schtasks("/Query", "/TN", taskName(p), "/FO", "CSV", "/NH")
	if err != nil {
		return fmt.Sprintf("not installed - run `whatsapp-mcp install --profile %s` to start at logon", p)
	}
	fields := strings.Split(strings.TrimSpace(out), ",")
	return fmt.Sprintf("%q %s", taskName(p), strings.Trim(fields[len(fields)-1], `"`))
}
