package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/claudecode"
	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/instance"
)

// The autostart service is per OS: a Task Scheduler task on Windows
// (install_windows.go), a launchd LaunchAgent on macOS (install_darwin.go).
// Each provides:
//   - exeName: the installed binary's file name.
//   - registerService(cfg, exe, workDir): create or replace the definition,
//     without touching a running daemon; it returns a line for the user.
//   - startService(cfg): start the daemon from the registered definition.
//   - removeService(cfg): remove the definition so nothing starts the
//     daemon again; reports whether there was one.
//   - serviceInstalled(p), serviceState(p): for stop and status.
//   - serviceRestartNote(p): what starts a stopped daemon again.

// installCmd copies this binary into the profile's bin dir, registers the
// autostart service and restarts the daemon from the installed copy.
func installCmd(args []string) error {
	cfg, _, err := flags("install", args, nil)
	if err != nil {
		return err
	}
	src, err := selfPath()
	if err != nil {
		return err
	}
	dir, err := config.BinDir(cfg.Profile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, exeName)
	removeLeftovers(dst)
	if sameFile(src, dst) {
		fmt.Println("Running from the installed binary: nothing to copy.")
	} else {
		if err := swapInstall(src, dst); err != nil {
			return err
		}
		fmt.Println("Installed", dst)
	}

	// The service is registered before the daemon stops: whatever fails
	// later, it starts the installed binary.
	desc, err := registerService(cfg, dst, dir)
	if err != nil {
		return err
	}
	fmt.Println(desc)
	if err := stopDaemon(cfg, stopTimeout, os.Stdout); err != nil {
		return fmt.Errorf("stop the running daemon: %w", err)
	}
	if err := startService(cfg); err != nil {
		return err
	}
	// The new daemon writes its pid as soon as it holds the profile, before
	// migrations: this wait covers a slow start too.
	pid, err := waitForDaemon(cfg, dst, 3*time.Minute)
	if err != nil {
		return fmt.Errorf("%w; see %s", err, filepath.Join(cfg.LogDir(), "whatsapp-mcp.log"))
	}
	fmt.Printf("Daemon running from %s (pid %d), MCP on http://127.0.0.1:%d/mcp\n", dst, pid, cfg.Port)
	removeLeftovers(dst)
	return nil
}

// uninstallCmd removes the service first (so nothing restarts the daemon),
// stops the daemon and deletes the installed binary. Data stays.
func uninstallCmd(args []string) error {
	cfg, _, err := flags("uninstall", args, nil)
	if err != nil {
		return err
	}
	removed, err := removeService(cfg)
	if err != nil {
		return err
	}
	if removed {
		fmt.Printf("Removed %s.\n", serviceName(cfg.Profile))
	} else {
		fmt.Printf("%s is not installed.\n", serviceName(cfg.Profile))
	}
	if err := stopDaemon(cfg, stopTimeout, os.Stdout); err != nil {
		return err
	}
	dir, err := config.BinDir(cfg.Profile)
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, exeName)
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Still running on Windows (this command, or a process not yet
		// gone): a running exe cannot be deleted there, only renamed.
		if rerr := os.Rename(dst, fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano())); rerr != nil {
			return fmt.Errorf("remove %s: %v; rename: %w", dst, err, rerr)
		}
		fmt.Printf("%s is still running: delete %s once it has ended.\n", dst, dir)
	}
	removeLeftovers(dst)
	_ = os.Remove(dir) // only when empty
	fmt.Printf("Data kept in %s.\n", cfg.DataDir)
	fmt.Printf("If Claude Code still lists this daemon, run `whatsapp-mcp setup-claude --remove --profile %s` from any copy of the binary:\n"+
		"until then its send-gate hooks, which run %s, block every WhatsApp send.\n", cfg.Profile, dst)
	return nil
}

// mcpServerName is the name the profile's daemon is registered under in
// Claude Code. The send gate reads the dev profile's data for any server
// whose name contains "dev".
func mcpServerName(p config.Profile) string {
	if p == config.ProfileProd {
		return "whatsapp"
	}
	return "whatsapp-" + string(p)
}

// installedExe is the profile's installed binary and whether it exists.
func installedExe(p config.Profile) (string, bool) {
	bin, err := config.BinDir(p)
	if err != nil {
		return "", false
	}
	exe := filepath.Join(bin, exeName)
	_, err = os.Stat(exe)
	return exe, err == nil
}

// setupClaudeCmd registers the installed daemon with Claude Code, or with
// --remove takes the registration out.
func setupClaudeCmd(args []string) error {
	var remove bool
	cfg, _, err := flags("setup-claude", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&remove, "remove", false, "remove the MCP server, the allow rules and the hooks instead")
	})
	if err != nil {
		return err
	}
	dir, err := claudecode.ConfigDir()
	if err != nil {
		return err
	}
	settings := filepath.Join(dir, "settings.json")
	name := mcpServerName(cfg.Profile)
	// The hooks are shared: they run prod's binary whenever prod is
	// installed, so removing or uninstalling dev never breaks prod's gate.
	others := []config.Profile{config.ProfileProd, config.ProfileDev}
	if remove {
		removed, err := claudecode.RemoveMCPServer(name)
		if err != nil {
			return err
		}
		keep := ""
		for _, p := range others {
			if exe, ok := installedExe(p); ok && p != cfg.Profile {
				keep = exe
				break
			}
		}
		if err := claudecode.Unregister(settings, name, keep); err != nil {
			return err
		}
		if removed {
			fmt.Printf("Removed MCP server %q.\n", name)
		} else {
			fmt.Printf("MCP server %q was not registered.\n", name)
		}
		if keep != "" {
			fmt.Printf("Settings %s: %q allow rules removed; the send-gate hooks stay for the other profile and run %s\n", settings, name, keep)
		} else {
			fmt.Printf("Settings %s: %q allow rules and the send-gate hooks removed\n", settings, name)
		}
		fmt.Println("Restart Claude Code.")
		return nil
	}
	exe, ok := installedExe(cfg.Profile)
	if !ok {
		return fmt.Errorf("%s not found: run `whatsapp-mcp install --profile %s` first", exe, cfg.Profile)
	}
	hookExe := exe
	if prod, ok := installedExe(config.ProfileProd); ok {
		hookExe = prod
	}
	token, err := cfg.EnsureToken()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", cfg.Port)
	if err := claudecode.AddMCPServer(name, url, token); err != nil {
		return err
	}
	fmt.Printf("MCP server %q: %s (user scope, bearer token from %s)\n", name, url, cfg.TokenFile())
	if err := claudecode.Register(settings, hookExe, name); err != nil {
		return err
	}
	fmt.Printf("Settings %s: read tools allowed, send-gate hooks run %s\n", settings, hookExe)
	fmt.Println("Restart Claude Code to load them.")
	return nil
}

// selfPath is the running binary's absolute path with links resolved.
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// swapInstall replaces dst with a copy of src without ever leaving it half
// written: the copy goes to dst.new, dst (possibly a running exe, which
// Windows lets rename but not delete) moves to dst.old, then dst.new takes
// its name. Between the two renames dst is missing for a moment. A failure
// puts the old binary back.
func swapInstall(src, dst string) error {
	next := dst + ".new"
	_ = os.Remove(next)
	if err := copyFile(src, next); err != nil {
		_ = os.Remove(next)
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		return os.Rename(next, dst)
	}
	old := dst + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		old = fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano()) // an older copy still runs
	}
	if err := os.Rename(dst, old); err != nil {
		_ = os.Remove(next)
		return fmt.Errorf("move the current binary aside: %w", err)
	}
	if err := os.Rename(next, dst); err != nil {
		if rerr := os.Rename(old, dst); rerr != nil {
			return fmt.Errorf("install %s: %v; restoring the old binary failed too (it is %s): %v", dst, err, old, rerr)
		}
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// removeLeftovers deletes copies an earlier install moved aside; on Windows
// one still running stays until the next install.
func removeLeftovers(dst string) {
	olds, _ := filepath.Glob(dst + ".old*")
	for _, p := range append(olds, dst+".new") {
		_ = os.Remove(p)
	}
}

// waitForDaemon waits until the profile's daemon runs from exe and returns its pid.
func waitForDaemon(cfg *config.Config, exe string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		if instance.Held(string(cfg.Profile)) {
			if pid, err := daemonPID(cfg); err == nil {
				if img, err := processImage(pid); err == nil && sameFile(img, exe) {
					return pid, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("the daemon did not start from %s within %s", exe, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sameFile reports whether two paths name the same file, whatever their
// spelling (case, 8.3 names, junctions, symlinks).
func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}
