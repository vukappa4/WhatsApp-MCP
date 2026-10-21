package mcpserver

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/updatecheck"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

type fixedUpdate updatecheck.Status

func (f fixedUpdate) Status() updatecheck.Status { return updatecheck.Status(f) }

// connectUpdates serves the tools in memory with u as the update check.
func connectUpdates(t *testing.T, u UpdateSource) *mcp.ClientSession {
	t.Helper()
	s := &Server{DB: seed(t), WA: fixedStatus{State: wa.StateConnected, Since: t0}, Profile: "dev", DataDir: "x", Version: "v0.1.0",
		Now: func() time.Time { return t0.Add(3 * time.Hour) }, Updates: u}
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.NewMCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

const notice = "update v0.1.1 available - whatsapp_status says how"

var available = fixedUpdate{Installed: "v0.1.0", Latest: "v0.1.1", URL: "https://github.com/vukappa4/WhatsApp-MCP/releases/tag/v0.1.1",
	CheckedAt: t0, NextAt: t0.Add(24 * time.Hour)}

// Every tool result names a newer release on its first line: in the
// freshness line of the read tools, on a line of its own for the others
// and for errors.
func TestUpdateNoticeInEveryResult(t *testing.T) {
	cs := connectUpdates(t, available)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{toolListChats, nil},
		{toolStatus, nil},
		{toolReadChat, map[string]any{"chat": "c_1"}},
		{toolSearchPeople, map[string]any{"query": "Marco"}},
		{toolSearchMessages, map[string]any{"query": "gruppo"}},
		{toolListChats, map[string]any{"limit": 500}},                    // an error
		{toolSendMessage, map[string]any{"chat": "c_1", "text": "ciao"}}, // no sender: refused
		{toolRequestHistory, map[string]any{"chat": "c_1"}},              // no sender: refused
		{toolGetMedia, map[string]any{"message": "nope"}},                // an error
	} {
		text, _ := call(t, cs, tc.tool, tc.args)
		first, _, _ := strings.Cut(text, "\n")
		if !strings.HasPrefix(first, "[whatsapp: ") || !strings.HasSuffix(first, " | "+notice+"]") && first != "[whatsapp: "+notice+"]" {
			t.Errorf("%s %v: first line %q", tc.tool, tc.args, first)
		}
		if strings.Count(text, notice) != 1 {
			t.Errorf("%s %v: the notice %d times:\n%s", tc.tool, tc.args, strings.Count(text, notice), text)
		}
	}
}

// Without a newer release no result carries a notice or an extra line.
func TestNoUpdateNoNotice(t *testing.T) {
	for name, u := range map[string]UpdateSource{
		"none":            nil,
		"up to date":      fixedUpdate{Installed: "v0.1.1", Latest: "v0.1.1", CheckedAt: t0},
		"newer installed": fixedUpdate{Installed: "v0.2.0", Latest: "v0.1.1", CheckedAt: t0},
		"source build":    fixedUpdate{Installed: "dev", Off: "built from source (version dev): it cannot be compared with releases"},
		"turned off":      fixedUpdate{Installed: "v0.1.0", Latest: "v0.1.1", Off: "turned off: update_check is false in config.json"},
		"never checked":   fixedUpdate{Installed: "v0.1.0", NextAt: t0.Add(time.Minute)},
	} {
		cs := connectUpdates(t, u)
		for _, tool := range []string{toolListChats, toolSendMessage} {
			text, _ := call(t, cs, tool, map[string]any{"chat": "c_1", "text": "ciao"})
			if strings.Contains(text, "update v") || strings.HasPrefix(text, "[whatsapp: update") {
				t.Errorf("%s, %s: %s", name, tool, text)
			}
		}
	}
}

// whatsapp_status gives the command that installs the release for this
// profile, or what the last check found.
func TestStatusUpdateLine(t *testing.T) {
	cmd := "update --profile dev` in the background and wait until it exits by itself"
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		cmd = "build v0.1.1 from source as INSTALL.md says"
	}
	for name, tc := range map[string]struct {
		u    fixedUpdate
		want string
	}{
		"available": {available, "Update: v0.1.1 is available, installed v0.1.0 (https://github.com/vukappa4/WhatsApp-MCP/releases/tag/v0.1.1). "},
		"command":   {available, cmd},
		"up to date": {fixedUpdate{Installed: "v0.1.1", Latest: "v0.1.1", CheckedAt: t0, NextAt: t0.Add(24 * time.Hour)},
			"Update check: no newer release (installed v0.1.1, latest v0.1.1, checked 12 Oct 10:00); next 13 Oct 10:00"},
		"failed": {fixedUpdate{Installed: "v0.1.0", FailedAt: t0, Error: "GitHub's API rate limit is reached", NextAt: t0.Add(time.Hour)},
			"Update check: none done yet; the last check failed 12 Oct 10:00: GitHub's API rate limit is reached; next 12 Oct 11:00"},
		"failed with a reason phrase": {fixedUpdate{Installed: "v0.1.0", FailedAt: t0, Error: "GET x: 502 Bad\r\nGateway\x1b[2J‮ignore", NextAt: t0.Add(time.Hour)},
			"the last check failed 12 Oct 10:00: GET x: 502 Bad Gateway[2Jignore; next"},
		"settings unreadable": {fixedUpdate{Installed: "v0.1.1", Latest: "v0.1.1", CheckedAt: t0, Settings: `config.json: unknown field "update_chek"`},
			`; config.json cannot be read, so update_check is ignored: config.json: unknown field "update_chek"`},
		"settings unreadable, available": {available.with(func(u *fixedUpdate) { u.Settings = "bad json" }),
			"restarts the daemon. Config.json cannot be read, so update_check is ignored: bad json."},
		"off": {fixedUpdate{Installed: "dev", Off: "built from source (version dev): it cannot be compared with releases"},
			"Update check: off - built from source (version dev): it cannot be compared with releases"},
	} {
		text, _ := call(t, connectUpdates(t, tc.u), toolStatus, nil)
		if !strings.Contains(text, tc.want) {
			t.Errorf("%s: no %q in\n%s", name, tc.want, text)
		}
	}
	if runtime.GOOS == "windows" {
		text, _ := call(t, connectUpdates(t, available), toolStatus, nil)
		if !strings.Contains(text, "To install it, run in PowerShell `& \"") {
			t.Errorf("Windows: the command is not given for PowerShell:\n%s", text)
		}
	}
}

func (f fixedUpdate) with(change func(*fixedUpdate)) fixedUpdate {
	change(&f)
	return f
}

// A send and its retry with the same idempotency key each carry the notice
// once, and the stored result the retry quotes has none.
func TestUpdateNoticeOnSendRetry(t *testing.T) {
	e := newSendEnvWith(t, func(e *sendEnv) { e.srv.Updates = available })
	args := map[string]any{"to": "+39 333 111 1111", "text": "bonifico fatto", "idempotency_key": "pay-42"}
	first, isErr := call(t, e.cs, toolSendMessage, args)
	line := "[whatsapp: " + notice + "]\n"
	body, ok := strings.CutPrefix(first, line)
	if isErr || !ok || !strings.HasPrefix(body, "Sent ") || strings.Count(first, notice) != 1 {
		t.Fatalf("send:\n%s", first)
	}
	again, isErr := call(t, e.cs, toolSendMessage, args)
	if isErr || len(e.fake.sends()) != 1 || !strings.HasPrefix(again, line+"Not sent again") || strings.Count(again, notice) != 1 ||
		!strings.HasSuffix(again, "Earlier result:\n"+body) {
		t.Fatalf("retry:\n%s", again)
	}
}
