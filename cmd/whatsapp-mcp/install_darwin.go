//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/instance"
)

const exeName = "whatsapp-mcp"

func serviceName(p config.Profile) string { return fmt.Sprintf("LaunchAgent %q", launchdLabel(p)) }

// plistPath is ~/Library/LaunchAgents/<label>.plist: launchd loads every
// agent there at the user's login.
func plistPath(p config.Profile) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel(p)+".plist"), nil
}

// guiTarget is the job in the user's login session domain.
func guiTarget(p config.Profile) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel(p))
}

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("launchctl %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// registerService writes the LaunchAgent. A loaded job keeps running its
// old definition until startService reloads it.
func registerService(cfg *config.Config, exe, workDir string) (string, error) {
	path, err := plistPath(cfg.Profile)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, launchdPlist(exe, workDir, cfg.Profile), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return fmt.Sprintf("LaunchAgent %s: starts at login, restarts after a crash (at most once a minute), stays stopped after `whatsapp-mcp stop`.", path), nil
}

func serviceLoaded(p config.Profile) bool {
	_, err := launchctl("print", guiTarget(p))
	return err == nil
}

// unload boots the agent out and waits until launchd has removed it. The
// daemon is stopped first, cleanly: a clean exit stays stopped
// (SuccessfulExit false), so launchd has nothing to restart, and bootout
// never waits on a send in flight. A daemon that had to be killed is
// restarted by launchd at once; bootout then sends that new one SIGTERM.
// launchctl may answer "Operation now in progress" while the job ends: the
// wait below covers it.
func unload(cfg *config.Config) error {
	if !serviceLoaded(cfg.Profile) {
		return nil
	}
	if instance.Held(string(cfg.Profile)) {
		if err := stopDaemon(cfg, stopTimeout, os.Stdout); err != nil {
			return fmt.Errorf("stop the running daemon: %w", err)
		}
	}
	_, bootErr := launchctl("bootout", guiTarget(cfg.Profile))
	for deadline := time.Now().Add(stopTimeout); serviceLoaded(cfg.Profile); time.Sleep(500 * time.Millisecond) {
		if time.Now().After(deadline) {
			if bootErr != nil {
				return bootErr
			}
			return fmt.Errorf("launchd still lists %s after %s", guiTarget(cfg.Profile), stopTimeout)
		}
	}
	return nil
}

// startService reloads the agent, which starts the daemon (RunAtLoad). An
// agent the user switched off (System Settings > General > Login Items)
// is enabled again: install is the request to run it.
func startService(cfg *config.Config) error {
	path, err := plistPath(cfg.Profile)
	if err != nil {
		return err
	}
	if err := unload(cfg); err != nil {
		return err
	}
	if _, err := launchctl("enable", guiTarget(cfg.Profile)); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	// launchd can refuse a bootstrap for a moment after a bootout.
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err = launchctl("bootstrap", domain, path)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w; if System Settings > General > Login Items lists whatsapp-mcp as off, turn it on and run install again", err)
		}
		time.Sleep(time.Second)
	}
}

// removeService unloads the agent and deletes its plist, so nothing starts
// the daemon again.
func removeService(cfg *config.Config) (bool, error) {
	path, err := plistPath(cfg.Profile)
	if err != nil {
		return false, err
	}
	loaded := serviceLoaded(cfg.Profile)
	if err := unload(cfg); err != nil {
		return false, err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return loaded, nil
	}
	return err == nil, err
}

func serviceInstalled(p config.Profile) bool {
	path, err := plistPath(p)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// serviceState is the agent's label and launchd state for status output.
func serviceState(p config.Profile) string {
	if !serviceInstalled(p) {
		return fmt.Sprintf("not installed - run `whatsapp-mcp install --profile %s` to start at login", p)
	}
	out, err := launchctl("print", guiTarget(p))
	if err != nil {
		return fmt.Sprintf("%q installed, not loaded: it starts at the next login", launchdLabel(p))
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "state = "); ok {
			return fmt.Sprintf("%q %s", launchdLabel(p), v)
		}
	}
	return fmt.Sprintf("%q loaded", launchdLabel(p))
}

func serviceRestartNote(config.Profile) string {
	return "It starts again at the next login (at once if it had to be killed)"
}
