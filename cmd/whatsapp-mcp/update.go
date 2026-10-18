package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/instance"
	"github.com/vukappa4/whatsapp-mcp/internal/release"
)

// releaseAPIBase is GitHub's API. A variable only so that a test build can
// point it at a local server (-ldflags "-X main.releaseAPIBase=..."); never
// an environment variable: a project's Claude Code settings can set those
// without running any code, and would then choose the binary update installs.
var releaseAPIBase = "https://api.github.com"

const releaseRepo = "vukappa4/WhatsApp-MCP"

// installDownloadStep is INSTALL.md's step that downloads a release binary
// and checks its SHA-256 by hand.
const installDownloadStep = "https://github.com/" + releaseRepo + "/blob/main/INSTALL.md#2-download"

// updateOptions is one update run; tests fill it directly.
type updateOptions struct {
	cfg     *config.Config
	c       common
	Check   bool   // report only
	Force   bool   // install over a build from source, the same version or a newer one
	Tag     string // install this release instead of the latest
	APIBase string
	HTTP    *http.Client // nil: release.NewHTTP
	Out     io.Writer
	Err     io.Writer // the install's errors; nil: Out
}

// updateCmd replaces the profile's installed binary with a GitHub release.
func updateCmd(args []string) error {
	var o updateOptions
	cfg, c, err := flags("update", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&o.Check, "check", false, "only report whether a newer release exists")
		fs.BoolVar(&o.Force, "force", false, "also replace a build from source, the same version, or a newer one with the latest release")
		fs.StringVar(&o.Tag, "version", "", "install this release tag (v0.2.0, v0.3.0-rc.1) instead of the latest")
	})
	if err != nil {
		return err
	}
	o.cfg, o.c, o.APIBase, o.Out, o.Err = cfg, c, releaseAPIBase, os.Stdout, os.Stderr
	ctx, cancel := signalContext()
	defer cancel()
	return update(ctx, o)
}

// update finds the release, downloads its binary for this OS, checks it
// against the release's checksums.txt and lets that binary install itself:
// its own install command replaces the installed binary (which may be the
// one running this update) and restarts the daemon.
func update(ctx context.Context, o updateOptions) error {
	out, p := o.Out, o.cfg.Profile
	// The installed daemon always runs on the profile's own data dir: with
	// another, update would find no daemon there, and install would wait
	// for one that never starts.
	if o.c.dataDir != "" {
		return fmt.Errorf("update does not take --data-dir: it updates the installed profile, whose daemon always uses %s's own data dir", p)
	}
	asset, err := release.AssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("update supports Windows and macOS: %w", err)
	}
	if o.Tag != "" {
		if _, err := release.ParseVersion(o.Tag); err != nil {
			return fmt.Errorf("--version: %w", err)
		}
	}
	installed, ok := installedExe(p)
	if !ok && !o.Check {
		return fmt.Errorf("profile %s is not installed: run `whatsapp-mcp install --profile %s` first", p, p)
	}
	cur := "unknown"
	if ok {
		if self, err := selfPath(); err == nil && sameFile(self, installed) {
			cur = version
		} else if v, err := installedVersion(installed); err == nil {
			cur = v
		}
	}
	// A pair, reingest, unlink or import-legacy holds the profile while no
	// daemon runs: install would wait minutes for a daemon that is not
	// there, then fail.
	if ok && !o.Check && instance.Held(string(p)) && !daemonRuns(o.cfg) {
		return fmt.Errorf("another whatsapp-mcp command (pair, reingest, unlink or import-legacy) holds profile %s: let it finish, then update", p)
	}

	client := &release.Client{APIBase: o.APIBase, Repo: releaseRepo, HTTP: o.HTTP,
		UserAgent: "whatsapp-mcp/" + version + " (+https://github.com/" + releaseRepo + ")"}
	var r *release.Release
	which := "Latest release"
	if o.Tag != "" {
		which = "Requested release"
		r, err = client.ByTag(ctx, o.Tag)
	} else {
		r, err = client.Latest(ctx)
		if err == nil && (r.Draft || r.Prerelease) {
			err = fmt.Errorf("GitHub named %s, a draft or prerelease, as the latest release: not installing it", r.Tag)
		}
	}
	if err != nil {
		return githubError(ctx, err, asset, o.Tag, p)
	}
	target, err := release.ParseVersion(r.Tag)
	if err != nil {
		return fmt.Errorf("release tag: %w", err)
	}
	if !ok {
		fmt.Fprintf(out, "Profile %s: not installed. %s: %s.\n", p, which, r.Tag)
		fmt.Fprintf(out, "Install it with `whatsapp-mcp install --profile %s` from the release binary.\n", p)
		return nil
	}
	fmt.Fprintf(out, "Profile %s: installed %s (%s). %s: %s.\n", p, cur, installed, which, r.Tag)

	// The command that does what this one only reports, with the flags it needs.
	again := "whatsapp-mcp update"
	if o.Tag != "" {
		again += " --version " + o.Tag
	}
	downgrade := false
	if cv, err := release.ParseVersion(cur); err != nil {
		if !o.Force {
			msg := fmt.Sprintf("the installed binary is version %s, which cannot be compared with releases: run `%s --force --profile %s` to replace it with release %s",
				describeVersion(cur), again, p, r.Tag)
			if o.Check {
				fmt.Fprintln(out, strings.ToUpper(msg[:1])+msg[1:]+".")
				return nil
			}
			return errors.New(msg)
		}
		again += " --force"
	} else {
		c := release.Compare(cv, target)
		switch {
		case c == 0 && !o.Force:
			fmt.Fprintf(out, "Already up to date: %s.\n", r.Tag)
			return nil
		case c > 0 && o.Tag == "" && !o.Force:
			fmt.Fprintf(out, "The installed %s is newer than the latest release %s: nothing to do.\n", cur, r.Tag)
			return nil
		case c > 0:
			downgrade = true
		}
		if c >= 0 && o.Tag == "" {
			again += " --force"
		}
	}
	if o.Check {
		fmt.Fprintf(out, "Update available: %s -> %s (%s). Run `%s --profile %s`.\n", cur, r.Tag, r.HTMLURL, again, p)
		return nil
	}

	bin, ok := r.Asset(asset)
	if !ok {
		return fmt.Errorf("release %s has no %s (it has: %s): nothing was installed", r.Tag, asset, strings.Join(r.AssetNames(), ", "))
	}
	sums, err := client.Checksums(ctx, r)
	if err != nil {
		return githubError(ctx, err, asset, o.Tag, p)
	}
	want, ok := sums[asset]
	if !ok {
		return fmt.Errorf("%s of release %s does not list %s: nothing was installed", release.ChecksumsName, r.Tag, asset)
	}

	root, err := config.AppDir()
	if err != nil {
		return err
	}
	removeStaleUpdates(root, time.Now())
	tmp, err := os.MkdirTemp(root, "update-*")
	if err != nil {
		return err
	}
	defer removeAllRetry(tmp, 10*time.Second)
	newExe := filepath.Join(tmp, exeName)
	fmt.Fprintf(out, "Downloading %s %s (%d MB)...\n", asset, r.Tag, (bin.Size+500_000)/1_000_000)
	if err := client.Download(ctx, bin, newExe, want); err != nil {
		var ce *release.ChecksumError
		if errors.As(err, &ce) {
			return fmt.Errorf("%w. Nothing was installed.", err)
		}
		return githubError(ctx, err, asset, o.Tag, p)
	}
	fmt.Fprintf(out, "SHA-256 verified against %s.\n", release.ChecksumsName)
	if err := os.Chmod(newExe, 0o700); err != nil {
		return err
	}
	// Run it before touching anything: a binary for another architecture,
	// one macOS refuses to run, or a mislabelled asset stops here. A Ctrl-C
	// also reaches the probe, which then fails: say what happened.
	got, err := installedVersion(newExe)
	if ctx.Err() != nil {
		return errors.New("interrupted: nothing was installed")
	}
	if err != nil || got != r.Tag {
		if err != nil {
			got = err.Error()
		}
		return fmt.Errorf("the downloaded %s does not run as %s (it reports %q): nothing was installed", asset, r.Tag, got)
	}

	if downgrade {
		fmt.Fprintf(out, "Installing %s, older than the installed %s. If %s changed the database, %s will not start and install fails after 3 minutes: then run update again to go back.\n",
			r.Tag, cur, cur, r.Tag)
	}
	fmt.Fprintf(out, "Installing %s with the new binary. Do not close this window until it ends:\n", r.Tag)
	if err := handOver(newExe, o); err != nil {
		return fmt.Errorf("the new binary's install failed (%w): its output above says how far it got", err)
	}
	fmt.Fprintf(out, "Updated profile %s: %s -> %s.\n", p, cur, r.Tag)
	if notes := updateNotes(runtime.GOOS, installed, p); notes != "" {
		fmt.Fprintln(out, notes)
	}
	return nil
}

// daemonRuns reports whether the profile's daemon.pid names a running
// whatsapp-mcp binary. The file outlives a daemon that was killed hard, and
// its pid may since belong to any process.
func daemonRuns(cfg *config.Config) bool {
	pid, err := daemonPID(cfg)
	if err != nil || pid <= 0 || !processAlive(pid) {
		return false
	}
	img, err := processImage(pid)
	return err == nil && isDaemonImage(img)
}

// describeVersion names a version that is not a release.
func describeVersion(v string) string {
	switch v {
	case "dev":
		return "dev (built from source)"
	case "unknown":
		return "unknown (built before the version command)"
	}
	return v
}

// githubError turns a failure to read a release into what the user can do.
func githubError(ctx context.Context, err error, asset, tag string, p config.Profile) error {
	if ctx.Err() != nil {
		return errors.New("interrupted: nothing was installed")
	}
	var rl *release.RateLimitError
	var ne *release.NetError
	page := "releases/latest"
	if tag != "" {
		page = "releases/tag/" + tag
	}
	switch {
	case errors.As(err, &rl):
		when := "in an hour"
		if !rl.Reset.IsZero() {
			when = "after " + rl.Reset.Local().Format("15:04")
		}
		return fmt.Errorf("GitHub's API rate limit is reached (60 requests an hour per network without login). Try again %s, "+
			"or download %s of https://github.com/%s/%s and check its SHA-256 as %s says, then run its install --profile %s.",
			when, asset, releaseRepo, page, installDownloadStep, p)
	case errors.As(err, &ne):
		return fmt.Errorf("%w. Behind a proxy, set HTTPS_PROXY.", err)
	case errors.Is(err, release.ErrNoRelease):
		return fmt.Errorf("%w yet: nothing to update to", err)
	}
	return err
}

// installedVersion runs exe version: a release tag, or dev for a build
// from source. A binary built before the version command exits non-zero:
// unknown.
func installedVersion(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, exe, "version").Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return "unknown", nil
	}
	if err != nil {
		return "", fmt.Errorf("run %s version: %w", exe, err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	for _, tok := range strings.Fields(first) {
		if _, err := release.ParseVersion(tok); err == nil || tok == "dev" {
			return tok, nil
		}
	}
	return "", fmt.Errorf("%s version printed no version: %q", exe, bytes.TrimSpace(b))
}

// handOver runs the new binary's install for the profile, in this
// terminal, and waits for it. A stopped install could leave the old daemon
// stopped and, on macOS, the LaunchAgent unloaded, so the install runs in
// its own process group: Ctrl-C reaches only this process, which says to
// wait. That is all the group shields: on Windows Ctrl-Break and closing
// the console window still end the install, and on macOS closing the
// terminal window can. Hence the message not to close the window.
func handOver(newExe string, o updateOptions) error {
	args := []string{"install", "--profile", string(o.cfg.Profile)}
	if o.c.debug {
		args = append(args, "--debug")
	}
	cmd := exec.Command(newExe, args...)
	cmd.Stdout, cmd.Stderr = o.Out, o.Err
	if o.Err == nil {
		cmd.Stderr = o.Out
	}
	ownProcessGroup(cmd)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-sig:
				fmt.Fprintln(o.Out, "install is still running: wait for it to end, and do not close this window")
			case <-done:
				return
			}
		}
	}()
	return cmd.Run()
}

// updateNotes is what the user has to do after an update on goos, where
// dst is the installed binary.
func updateNotes(goos, dst string, p config.Profile) string {
	if goos != "darwin" {
		return ""
	}
	return fmt.Sprintf("macOS: the new binary is a different program to macOS privacy settings. If you had added %s under "+
		"System Settings > Privacy & Security > Full Disk Access, that entry no longer applies: remove it and add it again "+
		"(press +, then Cmd+Shift+G to type the path), then run \"%s\" install --profile %s. macOS may also ask again the "+
		"first time Claude sends a file from Desktop, Documents or Downloads.", dst, dst, p)
}

// removeStaleUpdates deletes the download folders of updates that ended
// without cleaning up (killed, or a file Windows kept busy) more than an
// hour ago.
func removeStaleUpdates(root string, now time.Time) {
	dirs, _ := filepath.Glob(filepath.Join(root, "update-*"))
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && now.Sub(fi.ModTime()) > time.Hour {
			_ = os.RemoveAll(d)
		}
	}
}

// removeAllRetry removes dir, retrying for up to d: Windows releases the
// image of a process that just ended with a short delay.
func removeAllRetry(dir string, d time.Duration) {
	for deadline := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		if err := os.RemoveAll(dir); err == nil || time.Now().After(deadline) {
			return
		}
	}
}
