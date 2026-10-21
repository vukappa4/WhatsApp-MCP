package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/release"
)

const repo = "vukappa4/WhatsApp-MCP"

// t0 lies ahead of the real clock, which a Retry-After counts from.
var t0 = time.Date(2030, 1, 15, 9, 0, 0, 0, time.UTC)

// fakeGitHub answers releases/latest like GitHub's API: with tag (a
// prerelease when pre), or with status and headers, and counts the calls.
// Its html_url points elsewhere: the notice must never show it.
type fakeGitHub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	tag     string
	pre     bool
	status  int
	headers map[string]string
	calls   int
}

func newFakeGitHub(t *testing.T, tag string) *fakeGitHub {
	g := &fakeGitHub{tag: tag}
	g.srv = httptest.NewServer(g)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.calls++
	tag, pre, status, headers := g.tag, g.pre, g.status, g.headers
	g.mu.Unlock()
	if r.URL.Path != "/repos/"+repo+"/releases/latest" {
		http.NotFound(w, r)
		return
	}
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "prerelease": pre, "draft": false,
		"html_url": "https://evil.example/" + tag, "assets": []any{}})
}

func (g *fakeGitHub) set(f func(g *fakeGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g)
}

func (g *fakeGitHub) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// checker checks g for a daemon at version, keeping its answer in dir, at
// the time *now.
func checker(g *fakeGitHub, version, dir string, now *time.Time) *Checker {
	return &Checker{Client: &release.Client{APIBase: g.srv.URL, Repo: repo, UserAgent: "whatsapp-mcp/test"},
		Version: version, Log: slog.New(slog.DiscardHandler), File: filepath.Join(dir, "update-check.json"), Now: func() time.Time { return *now }}
}

func TestCheck(t *testing.T) {
	reset := t0.Add(3 * time.Hour).Truncate(time.Second)
	limited := func(at time.Time) func(g *fakeGitHub) {
		return func(g *fakeGitHub) {
			g.status, g.headers = 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(at.Unix(), 10)}
		}
	}
	for name, tc := range map[string]struct {
		installed string
		answer    func(g *fakeGitHub)
		available bool
		latest    string
		err       string
		next      time.Duration
	}{
		"available":                    {"v0.1.0", nil, true, "v0.1.1", "", 24 * time.Hour},
		"up to date":                   {"v0.1.1", nil, false, "v0.1.1", "", 24 * time.Hour},
		"newer installed":              {"v0.2.0", nil, false, "v0.1.1", "", 24 * time.Hour},
		"release candidate of it":      {"v0.1.1-rc.1", nil, true, "v0.1.1", "", 24 * time.Hour},
		"prerelease named latest":      {"v0.1.0", func(g *fakeGitHub) { g.tag, g.pre = "v0.2.0-rc.1", true }, false, "", "a draft or prerelease", time.Hour},
		"tag that is not a version":    {"v0.1.0", func(g *fakeGitHub) { g.tag = "latest" }, false, "", `"latest" as the latest release, not a vMAJOR.MINOR.PATCH tag`, time.Hour},
		"server error":                 {"v0.1.0", func(g *fakeGitHub) { g.status = 502 }, false, "", "502", time.Hour},
		"rate limit until reset":       {"v0.1.0", limited(reset), false, "", "rate limit", 3 * time.Hour},
		"rate limit beyond a day":      {"v0.1.0", limited(t0.Add(72 * time.Hour)), false, "", "rate limit", 24 * time.Hour},
		"build suffix named latest":    {"v0.1.0", func(g *fakeGitHub) { g.tag = "v0.2.0+evil" }, false, "", "not a vMAJOR.MINOR.PATCH tag", time.Hour},
		"unflagged prerelease latest":  {"v0.1.0", func(g *fakeGitHub) { g.tag = "v0.2.0-rc.1" }, false, "", "not a vMAJOR.MINOR.PATCH tag", time.Hour},
		"rate limit sooner than retry": {"v0.1.0", func(g *fakeGitHub) { g.status, g.headers = 429, map[string]string{"Retry-After": "60"} }, false, "", "rate limit", time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			g := newFakeGitHub(t, "v0.1.1")
			if tc.answer != nil {
				g.set(tc.answer)
			}
			now := t0
			c := checker(g, tc.installed, t.TempDir(), &now)
			c.check(context.Background())
			st := c.Status()
			if st.Available() != tc.available || st.Latest != tc.latest || !strings.Contains(st.Error, tc.err) || (tc.err == "") != (st.Error == "") {
				t.Fatalf("status %+v, available %v", st, st.Available())
			}
			if !st.NextAt.Equal(t0.Add(tc.next)) {
				t.Fatalf("next check %s, want %s", st.NextAt, t0.Add(tc.next))
			}
			if tc.err == "" && (!st.CheckedAt.Equal(t0) || st.URL != "https://github.com/"+repo+"/releases/tag/"+tc.latest) {
				t.Fatalf("status %+v", st)
			}
		})
	}
}

// A failed check keeps the last answer that worked: the notice stays while
// GitHub cannot be reached.
func TestFailureKeepsLastAnswer(t *testing.T) {
	g := newFakeGitHub(t, "v0.1.1")
	now := t0
	c := checker(g, "v0.1.0", t.TempDir(), &now)
	c.check(context.Background())
	now = t0.Add(25 * time.Hour)
	g.srv.Close()
	c.check(context.Background())
	st := c.Status()
	if !st.Available() || st.Latest != "v0.1.1" || !st.CheckedAt.Equal(t0) || !st.FailedAt.Equal(now) || !strings.Contains(st.Error, "contact GitHub") {
		t.Fatalf("status %+v", st)
	}
	if !st.NextAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("next check %s, want an hour after the failure", st.NextAt)
	}
}

// A restart keeps the answer and asks GitHub only when it is due; a clock
// set back does not postpone the next check beyond a day.
func TestRestartKeepsAnswer(t *testing.T) {
	g := newFakeGitHub(t, "v0.1.1")
	dir := t.TempDir()
	now := t0
	c := checker(g, "v0.1.0", dir, &now)
	c.load()
	if next := c.Status().NextAt; !next.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first check at %s, want a minute after start", next)
	}
	c.check(context.Background())
	// A clock set back while the daemon runs does not stop the checks either.
	now = t0.Add(-72 * time.Hour)
	if next := c.due(); !next.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("clock set back while running: next check %s, want a day from now", next)
	}

	now = t0.Add(2 * time.Hour)
	again := checker(g, "v0.1.0", dir, &now)
	again.load()
	st := again.Status()
	if !st.Available() || st.Latest != "v0.1.1" || !st.NextAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("after a restart: %+v", st)
	}
	// The same file read by the release just installed: no notice.
	if st := (Status{Installed: "v0.1.1", Latest: st.Latest}); st.Available() {
		t.Fatal("v0.1.1 installed: still a notice")
	}

	now = t0.Add(-72 * time.Hour)
	back := checker(g, "v0.1.0", dir, &now)
	back.load()
	if next := back.Status().NextAt; !next.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("clock set back: next check %s, want a day from now", next)
	}

	os.WriteFile(filepath.Join(dir, "update-check.json"), []byte("{"), 0o600)
	now = t0
	broken := checker(g, "v0.1.0", dir, &now)
	broken.load()
	if st := broken.Status(); st.Latest != "" || !st.NextAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("unreadable file: %+v", st)
	}
	if n := g.count(); n != 1 {
		t.Fatalf("GitHub asked %d times, want 1", n)
	}
}

// Run checks a minute after start, then not again before the answer says,
// and survives GitHub being down.
func TestRun(t *testing.T) {
	g := newFakeGitHub(t, "v0.1.1")
	c := &Checker{Client: &release.Client{APIBase: g.srv.URL, Repo: repo, UserAgent: "whatsapp-mcp/test"}, Version: "v0.1.0",
		Log:        slog.New(slog.DiscardHandler),
		File:       filepath.Join(t.TempDir(), "update-check.json"),
		firstDelay: 20 * time.Millisecond, interval: 2 * time.Second, retryAfter: 30 * time.Millisecond, maxSleep: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return c.Status().Available() })
	time.Sleep(30 * time.Millisecond)
	if n := g.count(); n != 1 {
		t.Fatalf("GitHub asked %d times within the interval, want 1", n)
	}

	// Down: retried after retryAfter, not in a loop.
	g.set(func(g *fakeGitHub) { g.status = 500 })
	waitFor(t, func() bool { return c.Status().Error != "" })
	n := g.count()
	time.Sleep(100 * time.Millisecond)
	if got := g.count() - n; got > 4 {
		t.Fatalf("%d retries in 100 ms with a 30 ms retry", got)
	}
	if !c.Status().Available() {
		t.Fatal("failures dropped the notice")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

// A build from source is never checked, and update_check false stops the
// checks and the notice.
func TestOff(t *testing.T) {
	g := newFakeGitHub(t, "v0.1.1")
	now := t0
	c := checker(g, "dev", t.TempDir(), &now)
	c.Run(context.Background()) // returns at once
	if st := c.Status(); st.Available() || !strings.Contains(st.Off, "built from source") {
		t.Fatalf("dev: %+v", st)
	}

	dir := t.TempDir()
	on := checker(g, "v0.1.0", dir, &now)
	on.check(context.Background())
	now = t0.Add(25 * time.Hour) // a check is due: only Enabled stops it
	off := checker(g, "v0.1.0", dir, &now)
	off.Enabled = func() (bool, error) { return false, nil }
	off.firstDelay, off.maxSleep = time.Millisecond, time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	off.Run(ctx)
	if st := off.Status(); st.Available() || !strings.Contains(st.Off, "update_check is false") {
		t.Fatalf("turned off: %+v", st)
	}
	if n := g.count(); n != 1 {
		t.Fatalf("GitHub asked %d times, want only the check before turning it off", n)
	}
}

// Settings that cannot be read keep the check and the notice on, and say
// why; only update_check false turns them off.
func TestUnreadableSettingsKeepChecking(t *testing.T) {
	g := newFakeGitHub(t, "v0.1.1")
	c := checker(g, "v0.1.0", t.TempDir(), new(time.Time))
	c.Now = nil
	c.Enabled = func() (bool, error) { return true, errors.New(`config.json: unknown field "update_chek"`) }
	c.firstDelay, c.maxSleep = time.Millisecond, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return c.Status().Available() })
	if st := c.Status(); st.Off != "" || !strings.Contains(st.Settings, "update_chek") {
		t.Fatalf("status %+v", st)
	}
}

// A later answer replaces the file a check wrote before, and a write that
// fails leaves no temporary file behind.
func TestSave(t *testing.T) {
	g := newFakeGitHub(t, "v0.1.1")
	dir := t.TempDir()
	now := t0
	c := checker(g, "v0.1.0", dir, &now)
	c.check(context.Background())
	g.set(func(g *fakeGitHub) { g.tag = "v0.2.0" })
	now = t0.Add(25 * time.Hour)
	c.check(context.Background())
	again := checker(g, "v0.1.0", dir, &now)
	again.load()
	if st := again.Status(); st.Latest != "v0.2.0" || !st.CheckedAt.Equal(now) {
		t.Fatalf("after saving over the file: %+v", st)
	}
	if _, err := os.Stat(c.File + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left: %v", err)
	}

	// A folder where the file belongs: the rename fails.
	blocked := checker(g, "v0.1.0", t.TempDir(), &now)
	if err := os.Mkdir(blocked.File, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked.File, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked.check(context.Background())
	if _, err := os.Stat(blocked.File + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left after a failed write: %v", err)
	}
	if !blocked.Status().Available() {
		t.Fatal("a failed write dropped the answer")
	}
}
