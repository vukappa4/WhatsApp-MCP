// Package updatecheck asks GitHub, in the daemon's background, whether a
// release newer than the running binary exists: a minute after start, then
// once a day. Tools read the last answer from memory, so neither start-up
// nor a tool call ever waits on the network. The answer is kept in a file:
// a restart keeps it and does not ask GitHub again before it is due, so a
// daemon restarted in a loop stays within GitHub's rate limit.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/release"
)

const (
	firstDelay = time.Minute      // from start to the first check: WhatsApp connects first
	interval   = 24 * time.Hour   // from a check that worked to the next
	retryAfter = time.Hour        // from a failed check to the next; a rate limit may say later
	maxSleep   = 10 * time.Minute // the loop reads the wall clock at least this often
)

// Checker checks for a newer release. Run starts it; Status is safe to call
// at any time from any goroutine.
type Checker struct {
	Client  *release.Client
	Version string // the running binary's: a release tag, or dev for a build from source
	// File keeps the last answer across restarts; empty keeps it in memory.
	File string
	// Enabled is read before every check: false stops checking, and the
	// notice, until it is true again. An error keeps checking: a typo in
	// the settings must not hide the notice. nil: always enabled.
	Enabled func() (bool, error)
	Log     *slog.Logger
	Now     func() time.Time // nil: time.Now

	// Tests shorten the waits; zero means the constants above.
	firstDelay, interval, retryAfter, maxSleep time.Duration

	mu          sync.Mutex
	st          state
	off         string
	settingsErr string
}

// state is the last answer, as File keeps it.
type state struct {
	Latest    string    `json:"latest,omitempty"` // tag of the latest stable release
	CheckedAt time.Time `json:"checked_at,omitzero"`
	FailedAt  time.Time `json:"failed_at,omitzero"` // the last check, when it failed
	Error     string    `json:"error,omitempty"`
	NextAt    time.Time `json:"next_at,omitzero"`
}

// Status is what the checker knows.
type Status struct {
	Installed   string    // the running version
	Latest, URL string    // the latest stable release and its page when a check worked, else empty
	CheckedAt   time.Time // the last check that worked
	FailedAt    time.Time // the last check, when it failed; zero after one that worked
	Error       string    // why it failed
	NextAt      time.Time // the next check
	Off         string    // why the daemon does not check; empty while it does
	Settings    string    // why the settings cannot be read; the daemon checks meanwhile
}

// Available reports whether Latest is a release newer than Installed, with
// the version order update uses. A build from source (dev) cannot be
// compared with releases: update --check offers no update for it either.
func (s Status) Available() bool {
	if s.Off != "" || !release.PlainTag(s.Latest) {
		return false
	}
	cur, err := release.ParseVersion(s.Installed)
	if err != nil {
		return false
	}
	latest, _ := release.ParseVersion(s.Latest)
	return release.Compare(cur, latest) < 0
}

// Status returns the last answer.
func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{Installed: c.Version, Latest: c.st.Latest, CheckedAt: c.st.CheckedAt,
		FailedAt: c.st.FailedAt, Error: c.st.Error, NextAt: c.st.NextAt, Off: c.off, Settings: c.settingsErr}
	if release.PlainTag(st.Latest) && c.Client != nil {
		st.URL = c.Client.PageURL(st.Latest)
	}
	return st
}

// Run checks until ctx ends. A version that is not a release tag is never
// checked: Run returns at once and Status says why.
func (c *Checker) Run(ctx context.Context) {
	if _, err := release.ParseVersion(c.Version); err != nil {
		c.mu.Lock()
		c.off = "this build's version " + c.Version + " cannot be compared with releases"
		if c.Version == "dev" {
			c.off = "built from source (version dev): it cannot be compared with releases"
		}
		c.mu.Unlock()
		return
	}
	c.load()
	for {
		wait := c.dur(c.maxSleep, maxSleep)
		if c.enabled() {
			if wait = c.due().Sub(c.now()); wait <= 0 {
				if c.check(ctx); ctx.Err() != nil {
					return
				}
				continue
			}
		}
		// A long timer may not count system sleep: wake up often and read
		// the wall clock again.
		t := time.NewTimer(min(wait, c.dur(c.maxSleep, maxSleep)))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// due is when the next check runs: never more than a day ahead, so a clock
// that was wrong at the last check, or is set back, does not stop the checks.
func (c *Checker) due() time.Time {
	limit := c.now().Add(c.dur(c.interval, interval))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.NextAt.After(limit) {
		c.st.NextAt = limit
	}
	return c.st.NextAt
}

// enabled reads Enabled and records why checks are off. Only an explicit
// false turns them off: settings that cannot be read keep them on.
func (c *Checker) enabled() bool {
	on, err := true, error(nil)
	if c.Enabled != nil {
		on, err = c.Enabled()
	}
	off, settingsErr := "", ""
	switch {
	case err != nil:
		settingsErr = err.Error()
	case !on:
		off = "turned off: update_check is false in config.json"
	}
	c.mu.Lock()
	changed := off != c.off || settingsErr != c.settingsErr
	c.off, c.settingsErr = off, settingsErr
	c.mu.Unlock()
	switch {
	case !changed:
	case off != "":
		c.logger().Warn("update check off", "why", off)
	case settingsErr != "":
		c.logger().Warn("update check: settings unreadable, checking anyway", "err", settingsErr)
	}
	return off == ""
}

// load reads File. The first check comes a minute after start, or when the
// last answer says, whichever is later; never more than a day ahead, so a
// clock set back does not stop the checks.
func (c *Checker) load() {
	now := c.now()
	var st state
	if c.File != "" {
		b, err := os.ReadFile(c.File)
		if err == nil {
			err = json.Unmarshal(b, &st)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			c.logger().Warn("update check: last answer unreadable, checking again", "file", c.File, "err", err)
			st = state{}
		}
	}
	first := now.Add(c.dur(c.firstDelay, firstDelay))
	if st.NextAt.Before(first) {
		st.NextAt = first
	}
	if limit := now.Add(c.dur(c.interval, interval)); st.NextAt.After(limit) {
		st.NextAt = limit
	}
	c.mu.Lock()
	c.st = st
	c.mu.Unlock()
}

// check asks GitHub once and records the answer. A failure keeps the last
// answer that worked: the notice stays while GitHub cannot be reached.
func (c *Checker) check(ctx context.Context) {
	r, err := c.Client.LatestStable(ctx)
	if ctx.Err() != nil {
		return
	}
	now := c.now()
	c.mu.Lock()
	st := c.st
	if err != nil {
		st.FailedAt, st.Error = now, err.Error()
		st.NextAt = now.Add(c.dur(c.retryAfter, retryAfter))
		var rl *release.RateLimitError
		if errors.As(err, &rl) && rl.Reset.After(st.NextAt) {
			st.NextAt = rl.Reset
			if limit := now.Add(c.dur(c.interval, interval)); st.NextAt.After(limit) {
				st.NextAt = limit
			}
		}
	} else {
		st = state{Latest: r.Tag, CheckedAt: now, NextAt: now.Add(c.dur(c.interval, interval))}
	}
	before := Status{Installed: c.Version, Latest: c.st.Latest}.Available()
	c.st = st
	c.mu.Unlock()

	if err != nil {
		c.logger().Info("update check failed", "err", err, "next", st.NextAt)
	} else if after := c.Status().Available(); after && !before {
		c.logger().Info("newer release available", "installed", c.Version, "latest", st.Latest)
	}
	c.save(st)
}

// save writes st to File through a temporary file, so a crash leaves the
// previous answer or the new one, never half of one.
func (c *Checker) save(st state) {
	if c.File == "" {
		return
	}
	b, err := json.Marshal(st)
	if err == nil {
		tmp := c.File + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, c.File)
		}
		if err != nil {
			os.Remove(tmp)
		}
	}
	if err != nil {
		c.logger().Warn("update check: cannot keep the answer for the next start", "dir", filepath.Dir(c.File), "err", err)
	}
}

// now is the wall clock without its monotonic reading: the times are
// compared with times read from File, and the monotonic clock does not
// count system sleep on every OS.
func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().Round(0)
	}
	return time.Now().Round(0)
}

func (c *Checker) dur(set, def time.Duration) time.Duration {
	if set > 0 {
		return set
	}
	return def
}

func (c *Checker) logger() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}
