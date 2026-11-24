package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/autosend"
	"github.com/vukappa4/whatsapp-mcp/internal/desktop"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

const (
	allowTool  = "mcp__whatsapp__whatsapp_send_autosend_allow"
	revokeTool = "mcp__whatsapp__whatsapp_send_autosend_revoke"
	history    = "mcp__whatsapp__whatsapp_request_history"
	marcoLID   = "123456789@lid"
	calcettoJ  = "120363000000000001@g.us"
)

// auto is a gate plus Claude Desktop files: the task "follow-up" (shown as
// "Follow up Marco") approved for c_1 (Marco) and c_2 (Calcetto), and
// session files for a scheduled run (sched-1), a run the user continued
// (sched-cont), a session with no task (plain-1) and a run of a task that
// no longer exists (sched-gone).
type auto struct {
	*gate
	desk  string // sessions root
	org   string // account/org folder
	skill string // the task's SKILL.md
	task  map[string]any
}

func newAuto(t *testing.T) *auto {
	t.Helper()
	a := &auto{gate: newGate(t)}
	base := filepath.Dir(a.root)
	a.desk = filepath.Join(base, "claude-code-sessions")
	a.org = filepath.Join(a.desk, "acct-1", "org-1")
	a.skill = filepath.Join(base, "claude", "scheduled-tasks", "follow-up", "SKILL.md")
	a.writeSkill("Every weekday ask Marco (c_1) whether the invoice arrived; post a summary in c_2.")
	a.task = map[string]any{"id": "follow-up", "displayName": "Follow up Marco", "filePath": a.skill, "cronExpression": "0 9 * * 1-5",
		"enabled": true, "permissionMode": "default", "cwd": filepath.Join(base, "work"), "createdAt": 1}
	a.writeTasks()
	a.writeRun("sched-1", map[string]any{"scheduledTaskId": "follow-up", "scheduledRunContinued": false, "permissionMode": "default"})
	a.writeRun("sched-cont", map[string]any{"scheduledTaskId": "follow-up", "scheduledRunContinued": true})
	a.writeRun("plain-1", map[string]any{})
	a.writeRun("sched-gone", map[string]any{"scheduledTaskId": "gone"})
	a.approve("prod", "c_1", "c_2")
	return a
}

func (a *auto) write(path string, b []byte) {
	a.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		a.t.Fatal(err)
	}
}

func (a *auto) writeSkill(body string) {
	a.write(a.skill, []byte("---\nname: follow-up\ndescription: Follow up with Marco\n---\n"+body+"\n"))
}

func (a *auto) writeTasks(extra ...map[string]any) {
	b, _ := json.Marshal(map[string]any{"scheduledTasks": append([]map[string]any{a.task}, extra...)})
	a.write(filepath.Join(a.org, desktop.TasksFile), b)
}

func (a *auto) writeRun(session string, fields map[string]any) {
	fields["cliSessionId"] = session
	fields["sessionId"] = "local_" + session
	b, _ := json.Marshal(fields)
	a.write(filepath.Join(a.org, "local_"+session+".json"), b)
}

// approve stores the rule for follow-up in profile's data.db, as the daemon would.
func (a *auto) approve(profile string, refs ...string) {
	a.t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(a.root, profile), 0o700); err != nil {
		a.t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(a.root, profile, "data.db"))
	if err != nil {
		a.t.Fatal(err)
	}
	defer db.Close()
	task, err := desktop.TaskByID(a.org, "follow-up")
	if err != nil {
		a.t.Fatal(err)
	}
	fp, err := desktop.Fingerprint(task)
	if err != nil {
		a.t.Fatal(err)
	}
	r := store.AutosendRule{TaskID: task.ID, TaskDir: task.Dir, TaskName: task.Title(), Schedule: task.Schedule(), Fingerprint: fp, ApprovedAt: a.now.Unix()}
	for _, ref := range refs {
		c, err := destination(ctx, db.DB, ref)
		if err != nil {
			a.t.Fatal(err)
		}
		r.Chats = append(r.Chats, store.AutosendChat{JID: c.JID, Ref: c.Ref, Label: c.Label})
	}
	if err := store.PutAutosendRule(ctx, db, r); err != nil {
		a.t.Fatal(err)
	}
}

// copyProfile copies the prod data.db, rules included, to profile.
func (a *auto) copyProfile(profile string) {
	a.t.Helper()
	if err := os.MkdirAll(filepath.Join(a.root, profile), 0o700); err != nil {
		a.t.Fatal(err)
	}
	a.exec(`VACUUM INTO ?`, filepath.Join(a.root, profile, "data.db"))
}

func (a *auto) exec(q string, args ...any) {
	a.t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(a.root, "prod", "data.db"))
	if err != nil {
		a.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		a.t.Fatal(err)
	}
}

func (a *auto) env(scheduled bool) PreToolEnv {
	// Desktop's save is immediate in these fixtures: a short wait.
	return PreToolEnv{Dir: a.dir, Now: a.now.Add(time.Minute), Resolver: DBResolver{Root: a.root}, Scheduled: scheduled,
		DesktopRoots: []string{a.desk}, AppDir: a.root, RunWait: 400 * time.Millisecond, ClaudeDir: a.claudeDir()}
}

// claudeDir is the test's Claude config folder; userSettings and
// projectSettings write the settings a task without a mode takes its mode from.
func (a *auto) claudeDir() string { return filepath.Join(filepath.Dir(a.root), "claude-config") }

func (a *auto) userSettings(body string) {
	a.write(filepath.Join(a.claudeDir(), "settings.json"), []byte(body))
}

func (a *auto) projectSettings(name, body string) {
	a.write(filepath.Join(filepath.Dir(a.root), "work", ".claude", name), []byte(body))
}

func (a *auto) dropTaskMode() { delete(a.task, "permissionMode"); a.writeTasks() }

// call runs PreTool in permission mode default and returns the decision and
// its reason ("" twice when silent).
func (a *auto) call(env PreToolEnv, session, tool string, args map[string]any) (kind, reason string) {
	a.t.Helper()
	return a.callMode(env, "default", session, tool, args)
}

func (a *auto) callMode(env PreToolEnv, mode, session, tool string, args any) (kind, reason string) {
	a.t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": session, "transcript_path": "x.jsonl", "hook_event_name": "PreToolUse",
		"tool_name": tool, "tool_input": args, "tool_use_id": "toolu_01", "permission_mode": mode})
	return a.callRaw(env, in)
}

func (a *auto) callRaw(env PreToolEnv, in []byte) (kind, reason string) {
	a.t.Helper()
	var out bytes.Buffer
	if err := PreTool(bytes.NewReader(in), &out, env); err != nil {
		a.t.Fatal(err)
	}
	if out.Len() == 0 {
		return "", ""
	}
	var got map[string]map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		a.t.Fatalf("output is not JSON: %q", out.String())
	}
	h := got["hookSpecificOutput"]
	if h["hookEventName"] != "PreToolUse" || h["permissionDecisionReason"] == "" {
		a.t.Fatalf("decision = %v", h)
	}
	return h["permissionDecision"], h["permissionDecisionReason"]
}

func (a *auto) send(env PreToolEnv, session, to string) (kind, reason string) {
	a.t.Helper()
	return a.call(env, session, send, map[string]any{"to": to, "text": "Ciao Marco, è arrivata la fattura?"})
}

func (a *auto) prompt(opts PromptOptions, session, text string) {
	a.t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": session, "prompt": text})
	if err := UserPrompt(bytes.NewReader(in), a.dir, a.now, opts); err != nil {
		a.t.Fatal(err)
	}
}

func (a *auto) logPath() string { return filepath.Join(a.root, "prod", "logs", autosend.LogName) }

func (a *auto) auditLines() []string {
	a.t.Helper()
	b, err := os.ReadFile(a.logPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// noScan makes a Desktop folder scan fail the test.
func noScan(t *testing.T) {
	t.Helper()
	old := findRun
	t.Cleanup(func() { findRun = old })
	findRun = func(context.Context, []string, string) (desktop.Run, error) {
		t.Error("the Desktop folders were scanned")
		return desktop.Run{}, desktop.ErrNotFound
	}
}

// An approved scheduled run sends to its approved chats by any id that
// names them, and each send is logged without its text.
func TestAutosendAllowsApprovedChats(t *testing.T) {
	a := newAuto(t)
	for _, tc := range []struct{ to, label string }{
		{"c_1", "Marco Rossi (+393331234567)"},
		{"p_1", "Marco Rossi (+393331234567)"},
		{" c_2 ", "group Calcetto del Giovedì (c_2)"},
	} {
		kind, r := a.send(a.env(true), "sched-1", tc.to)
		if kind != allow || r != "WhatsApp autosend: task Follow up Marco approved 2026-10-13 sends to "+tc.label+"." {
			t.Errorf("to %q: %s %q", tc.to, kind, r)
		}
	}
	lines := a.auditLines()
	if len(lines) != 3 {
		t.Fatalf("audit lines: %q", lines)
	}
	var e auditEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatal(err)
	}
	if e.SessionID != "sched-1" || e.TaskID != "follow-up" || e.To != "c_1" || e.JID != marcoLID || e.TextChars != 34 || e.At == "" ||
		strings.Contains(lines[0], "fattura") {
		t.Fatalf("audit entry %s", lines[0])
	}
	if json.Unmarshal([]byte(lines[2]), &e); e.JID != calcettoJ || e.To != "c_2" {
		t.Fatalf("audit entry %s", lines[2])
	}
	// All optional arguments, spelled exactly, still autosend.
	if kind, r := a.call(a.env(true), "sched-1", send, map[string]any{"to": "c_1", "text": "x", "reply_to": "m_1", "idempotency_key": "k"}); kind != allow {
		t.Errorf("all arguments: %s %q", kind, r)
	}
}

// In an unattended run, anything short of an approved, unchanged task sending to an approved chat
// is denied, saying why, so Claude can tell the user.
func TestAutosendFallbacksDeny(t *testing.T) {
	type tc struct {
		name, session, to string
		tool              string
		args              any
		setup             func(a *auto)
		why               string
	}
	for _, c := range []tc{
		{name: "run file missing", session: "sched-404", to: "c_1", why: "had not saved this run's session file after 400ms"},
		{name: "run file unreadable", session: "sched-bad", to: "c_1", why: "cannot find this run",
			setup: func(a *auto) {
				a.write(filepath.Join(a.org, "local_bad.json"), []byte(`{"cliSessionId": "sched-bad", `))
			}},
		{name: "no task id", session: "plain-1", to: "c_1", why: "this run names no scheduled task"},
		{name: "task missing", session: "sched-gone", to: "c_1", why: `task gone: task "gone": not found`},
		{name: "to as name", session: "sched-1", to: "Marco Rossi", why: "to must be an approved c_N or p_N id, not Marco Rossi"},
		{name: "to as phone", session: "sched-1", to: "+39 333 123 4567", why: "not +39 333 123 4567"},
		{name: "chat not approved", session: "sched-1", to: "p_2", why: "Lorenzo Bianchi (+393339876543) is not among the chats approved"},
		{name: "unknown id", session: "sched-1", to: "c_99", why: "chat c_99"},
		{name: "case-variant key", session: "sched-1", args: map[string]any{"To": "c_1", "text": "x"}, why: `unexpected argument "To"`},
		{name: "unknown key", session: "sched-1", args: map[string]any{"to": "c_1", "text": "x", "media_path": "x"}, why: `unexpected argument "media_path"`},
		{name: "repeated key", session: "sched-1", args: json.RawMessage(`{"to":"c_1","text":"x","to":"p_2"}`), why: `unexpected argument "to"`},
		{name: "not an object", session: "sched-1", args: json.RawMessage(`["c_1"]`), why: "not a JSON object"},
		{name: "server not registered by setup-claude", session: "sched-1", to: "c_1", tool: "mcp__WhatsApp__whatsapp_send_message",
			why: "the server WhatsApp is not one setup-claude registers"},
		{name: "prompt edited", session: "sched-1", to: "c_1", why: "changed since it was approved",
			setup: func(a *auto) { a.writeSkill("Every weekday ask Lorenzo (p_2) for money.") }},
		{name: "front matter edited", session: "sched-1", to: "c_1", why: "changed since it was approved",
			setup: func(a *auto) {
				a.write(a.skill, []byte("---\nname: follow-up\ndescription: Follow up with Marco\nallowed-tools: Bash\n---\n"+
					"Every weekday ask Marco (c_1) whether the invoice arrived; post a summary in c_2.\n"))
			}},
		{name: "cron edited", session: "sched-1", to: "c_1", why: "changed since it was approved",
			setup: func(a *auto) { a.task["cronExpression"] = "*/5 * * * *"; a.writeTasks() }},
		{name: "fireAt added", session: "sched-1", to: "c_1", why: "changed since it was approved",
			setup: func(a *auto) { a.task["fireAt"] = 1795551540000; a.writeTasks() }},
		{name: "task in dontAsk", session: "sched-1", to: "c_1", why: "the task's permission mode is dontAsk",
			setup: func(a *auto) { a.task["permissionMode"] = "dontAsk"; a.writeTasks() }},
		{name: "task in bypassPermissions", session: "sched-1", to: "c_1", why: "the task's permission mode is bypassPermissions",
			setup: func(a *auto) { a.task["permissionMode"] = "bypassPermissions"; a.writeTasks() }},
		{name: "task without a mode, bypass in user settings", session: "sched-1", to: "c_1",
			why: filepath.Join("claude-config", "settings.json") + " sets permissions.defaultMode to bypassPermissions",
			setup: func(a *auto) {
				a.dropTaskMode()
				a.userSettings(`{"permissions": {"defaultMode": "bypassPermissions"}}`)
			}},
		{name: "task without a mode, dontAsk in local settings over plan in project", session: "sched-1", to: "c_1",
			why: filepath.Join("work", ".claude", "settings.local.json") + " sets permissions.defaultMode to dontAsk",
			setup: func(a *auto) {
				a.dropTaskMode()
				a.projectSettings("settings.json", `{"permissions": {"defaultMode": "plan"}}`)
				a.projectSettings("settings.local.json", `{"permissions": {"defaultMode": "dontAsk"}}`)
			}},
		{name: "task without a mode, broken project settings", session: "sched-1", to: "c_1",
			why: "the default mode from your Claude Code settings cannot be read",
			setup: func(a *auto) {
				a.dropTaskMode()
				a.userSettings(`{"permissions": {"defaultMode": "default"}}`)
				a.projectSettings("settings.json", `{"permissions": `)
			}},
		{name: "run in an unknown mode", session: "sched-new", to: "c_1", why: "the run's permission mode is yolo",
			setup: func(a *auto) {
				a.writeRun("sched-new", map[string]any{"scheduledTaskId": "follow-up", "permissionMode": "yolo"})
			}},
		{name: "run in dontAsk", session: "sched-dontask", to: "c_1", why: "the run's permission mode is dontAsk",
			setup: func(a *auto) {
				a.writeRun("sched-dontask", map[string]any{"scheduledTaskId": "follow-up", "permissionMode": "dontAsk"})
			}},
		{name: "folder edited", session: "sched-1", to: "c_1", why: "changed since it was approved",
			setup: func(a *auto) { a.task["cwd"] = filepath.Join(a.root, "elsewhere"); a.writeTasks() }},
		{name: "SKILL.md moved", session: "sched-1", to: "c_1", why: "changed since it was approved",
			setup: func(a *auto) {
				b, _ := os.ReadFile(a.skill)
				moved := filepath.Join(filepath.Dir(a.skill), "copy", "SKILL.md")
				a.write(moved, b)
				a.task["filePath"] = moved
				a.writeTasks()
			}},
		{name: "relative filePath", session: "sched-1", to: "c_1", why: "is not an absolute path",
			setup: func(a *auto) { a.task["filePath"] = filepath.Join("claude", "SKILL.md"); a.writeTasks() }},
		{name: "SKILL.md gone", session: "sched-1", to: "c_1", why: "read the task's SKILL.md",
			setup: func(a *auto) { os.Remove(a.skill) }},
		{name: "no rule", session: "sched-1", to: "c_1", why: "task Follow up Marco is not approved",
			setup: func(a *auto) { a.exec(`DELETE FROM autosend_rules`) }},
		{name: "rule only in the dev profile", session: "sched-1", to: "c_1", why: "is not approved",
			setup: func(a *auto) { a.copyProfile("dev"); a.exec(`DELETE FROM autosend_rules`) }},
		{name: "rule for another Desktop account", session: "sched-1", to: "c_1", why: "another Claude Desktop account or organization",
			setup: func(a *auto) {
				a.exec(`UPDATE autosend_rules SET task_dir = ?`, filepath.Join(a.desk, "acct-2", "org-2"))
			}},
		{name: "reused c_N with another JID", session: "sched-1", to: "c_1", why: "Lorenzo Bianchi (+393339876543) is not among",
			setup: func(a *auto) {
				a.exec(`UPDATE chats SET person_id = 2, jid = '393339876543@s.whatsapp.net' WHERE id = 1`)
			}},
		{name: "two LIDs", session: "sched-1", to: "p_1", why: "p_1 holds 2 WhatsApp accounts (LIDs)",
			setup: func(a *auto) { a.exec(`INSERT INTO jids(jid, person_id, kind) VALUES ('987@lid', 1, 'lid')`) }},
		{name: "run bypasses permissions", session: "sched-bypass", to: "c_1", why: "the run's permission mode is bypassPermissions",
			setup: func(a *auto) {
				a.writeRun("sched-bypass", map[string]any{"scheduledTaskId": "follow-up", "permissionMode": "bypassPermissions"})
			}},
		{name: "audit log fails", session: "sched-1", to: "c_1", why: "cannot write the autosend log",
			setup: func(a *auto) {
				a.write(filepath.Join(a.root, "prod", "logs", autosend.LogName), nil)
				os.Chmod(a.logPath(), 0o400)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAuto(t)
			if c.setup != nil {
				c.setup(a)
			}
			tool, args := send, c.args
			if c.tool != "" {
				tool = c.tool
			}
			if args == nil {
				args = map[string]any{"to": c.to, "text": "Ciao"}
			}
			before := len(a.auditLines())
			kind, r := a.callMode(a.env(true), "default", c.session, tool, args)
			if kind != deny || !strings.HasPrefix(r, "WhatsApp: this scheduled run has nobody present and is not approved for this send to ") ||
				!strings.Contains(r, c.why) || !strings.HasSuffix(r, "). Do not retry; tell the user in your final message.") {
				t.Fatalf("%s %q, want deny with %q", kind, r, c.why)
			}
			if after := len(a.auditLines()); after != before {
				t.Fatalf("logged a send that was not allowed")
			}
		})
	}
}

// The send brake: past autosend.MaxPerDay sends of a task to one chat in
// 24 hours, the next asks. Older sends, other chats and other tasks do not
// count; the rotated log does.
func TestAutosendBrake(t *testing.T) {
	a := newAuto(t)
	now := a.now.Add(time.Minute)
	line := func(at time.Time, task, jid string) string {
		b, _ := json.Marshal(auditEntry{At: at.UTC().Format(time.RFC3339), TaskID: task, JID: jid, Label: "x"})
		return string(b) + "\n"
	}
	var old, cur strings.Builder
	for i := range autosend.MaxPerDay - 1 {
		old.WriteString(line(now.Add(-25*time.Hour), "follow-up", marcoLID))
		cur.WriteString(line(now.Add(-time.Duration(i)*time.Minute), "other-task", marcoLID))
		cur.WriteString(line(now.Add(-time.Duration(i)*time.Minute), "follow-up", calcettoJ))
	}
	for i := range autosend.MaxPerDay - 2 {
		cur.WriteString(line(now.Add(-time.Duration(i)*time.Hour), "follow-up", marcoLID))
	}
	cur.WriteString("not json\n")
	a.write(a.logPath()+".1", []byte(old.String()+line(now.Add(-23*time.Hour), "follow-up", marcoLID)))
	a.write(a.logPath(), []byte(cur.String()))
	// 19 sends in 24 hours: the 20th goes.
	if kind, r := a.send(a.env(true), "sched-1", "c_1"); kind != allow {
		t.Fatalf("20th send: %s %q", kind, r)
	}
	kind, r := a.send(a.env(true), "sched-1", "p_1")
	want := fmt.Sprintf("WhatsApp: this scheduled run has nobody present and is not approved for this send to Marco Rossi (+393331234567) "+
		"(the task reached %d autosends to Marco Rossi (+393331234567) in 24 hours). Do not retry; tell the user in your final message.", autosend.MaxPerDay)
	if kind != deny || r != want {
		t.Fatalf("21st send: %s %q", kind, r)
	}
	if kind, _ := a.send(a.env(true), "sched-1", "c_2"); kind != allow {
		t.Fatalf("another chat: %s", kind)
	}
}

// Other gated tools never autosend: with nobody present they are denied,
// naming the recipient as given (no lookup on a deny), and so is a call the
// hook cannot read. Once a person typed, the user-prompt gate asks.
func TestAutosendOnlyTextMessages(t *testing.T) {
	a := newAuto(t)
	const only = "(only whatsapp_send_message to a chat approved for the task goes out with nobody present). Do not retry"
	cases := []struct {
		tool string
		args map[string]any
		what string
	}{
		{sendFile, map[string]any{"to": "c_1", "path": `C:\x\fattura.pdf`}, "this send to c_1"},
		{history, map[string]any{"chat": "c_1"}, "this history request for c_1"},
		{"mcp__whatsapp__send_message", map[string]any{"recipient": "393331234567", "message": "x"}, "this send to 393331234567"},
	}
	env := a.env(true)
	env.Resolver = noResolver{t}
	for _, c := range cases {
		kind, r := a.call(env, "sched-1", c.tool, c.args)
		if kind != deny || !strings.Contains(r, "is not approved for "+c.what+" "+only) {
			t.Errorf("%s: %s %q", c.tool, kind, r)
		}
	}
	if kind, r := a.callRaw(env, []byte(`{"session_id":"sched-1","tool_name":"`+send+`",`)); kind != deny ||
		!strings.Contains(r, "the hook could not read this call") {
		t.Errorf("unreadable call: %s %q", kind, r)
	}
	sched := PromptOptions{Scheduled: true}
	a.prompt(sched, "sched-1", "task prompt")
	a.prompt(sched, "sched-1", "leggi i messaggi")
	for _, c := range cases {
		if kind, _ := a.call(a.env(true), "sched-1", c.tool, c.args); kind != ask {
			t.Errorf("%s after a person typed: %s", c.tool, kind)
		}
	}
}

// noResolver fails the test when the gate looks a recipient up.
type noResolver struct{ t *testing.T }

func (n noResolver) Resolve(context.Context, string, string) (Recipient, error) {
	n.t.Error("a recipient was looked up on a deny path")
	return Recipient{}, errors.New("no lookup")
}

// A deny for a chat the task is not approved for names the label the
// approval stored when the id is one of its chats, else the id as given.
func TestAutosendDenyLabels(t *testing.T) {
	a := newAuto(t)
	env := a.env(true)
	env.Resolver = noResolver{t}
	a.exec(`INSERT INTO jids(jid, person_id, kind) VALUES ('987@lid', 1, 'lid')`) // p_1 and c_1 now fail to resolve
	if kind, r := a.send(env, "sched-1", "c_1"); kind != deny || !strings.Contains(r, "not approved for this send to Marco Rossi (+393331234567) (") {
		t.Fatalf("approved id: %s %q", kind, r)
	}
	if kind, r := a.send(env, "sched-1", "p_2"); kind != deny || !strings.Contains(r, "not approved for this send to p_2 (") {
		t.Fatalf("other id: %s %q", kind, r)
	}
}

// Desktop saves a run's session file a few seconds after the run starts:
// the gate waits for it.
func TestAutosendWaitsForTheRunFile(t *testing.T) {
	a := newAuto(t)
	env := a.env(true)
	env.RunWait = 5 * time.Second
	go func() {
		time.Sleep(time.Second)
		b, _ := json.Marshal(map[string]any{"cliSessionId": "sched-late", "sessionId": "local_late", "scheduledTaskId": "follow-up"})
		os.WriteFile(filepath.Join(a.org, "local_late.json"), b, 0o600)
	}()
	start := time.Now()
	if kind, r := a.send(env, "sched-late", "c_1"); kind != allow {
		t.Fatalf("%s %q", kind, r)
	}
	if took := time.Since(start); took < 900*time.Millisecond || took > 4*time.Second {
		t.Fatalf("took %s", took)
	}
}

// A file caught mid-write reads as broken, or twice: the gate looks again
// until the wait ends.
func TestAutosendRetriesABrokenRunFile(t *testing.T) {
	a := newAuto(t)
	env := a.env(true)
	env.RunWait = 5 * time.Second
	path := filepath.Join(a.org, "local_mid.json")
	a.write(path, []byte(`{"cliSessionId": "sched-mid", `))
	go func() {
		time.Sleep(time.Second)
		b, _ := json.Marshal(map[string]any{"cliSessionId": "sched-mid", "sessionId": "local_mid", "scheduledTaskId": "follow-up"})
		os.WriteFile(path, b, 0o600)
	}()
	if kind, r := a.send(env, "sched-mid", "c_1"); kind != allow {
		t.Fatalf("%s %q", kind, r)
	}
}

// Without CLAUDE_CODE_HOST_SCHEDULED_RUN=1 the session is the user's: no
// Desktop folder is read, nothing is autosent, and the user-prompt gate
// decides as it always has.
func TestInteractiveSessionsSkipDesktop(t *testing.T) {
	a := newAuto(t)
	noScan(t)
	kind, r := a.send(a.env(false), "sched-1", "c_1")
	if kind != ask || strings.Contains(r, "Autosend:") || strings.Contains(r, "scheduled task") || !strings.Contains(r, "did not ask to send") {
		t.Fatalf("no flag: %s %q", kind, r)
	}
	a.prompt(PromptOptions{}, "sched-1", "rispondi a Marco che arrivo")
	if kind, r := a.send(a.env(false), "sched-1", "c_1"); kind != silent {
		t.Fatalf("flag naming Marco: %s %q", kind, r)
	}
	if kind, _ := a.call(a.env(false), "s1", allowTool, map[string]any{"task": "follow-up", "chats": []string{"c_1"}}); kind != ask {
		t.Fatalf("approval card: %s", kind)
	}
	if lines := a.auditLines(); len(lines) != 0 {
		t.Fatalf("autosent: %q", lines)
	}
}

// Desktop's "continued" flag does not decide: it lags a person's typing by
// seconds, and "Run now" sets it with nobody typing. Only a prompt after the
// first one makes a person present.
func TestScheduledRunPresence(t *testing.T) {
	a := newAuto(t)
	sched := PromptOptions{Scheduled: true}
	// "Run now" of the approved task: continued from the start, only the task prompt.
	a.prompt(sched, "sched-cont", "Every weekday ask Marco (c_1) whether the invoice arrived; post a summary in c_2.")
	if kind, r := a.send(a.env(true), "sched-cont", "c_1"); kind != allow {
		t.Fatalf("Run now, approved chat: %s %q", kind, r)
	}
	if kind, r := a.send(a.env(true), "sched-cont", "p_2"); kind != deny {
		t.Fatalf("Run now, other chat: %s %q", kind, r)
	}
	// A person types in the opened run while the file still says not continued.
	a.prompt(sched, "sched-1", "task prompt")
	a.prompt(sched, "sched-1", "leggi i messaggi")
	noScan(t)
	if kind, r := a.send(a.env(true), "sched-1", "c_1"); kind != ask || !strings.Contains(r, "did not ask to send") {
		t.Fatalf("typed prompt without intent: %s %q", kind, r)
	}
	a.prompt(sched, "sched-1", "rispondi a Lorenzo che arrivo")
	if kind, r := a.send(a.env(true), "sched-1", "p_2"); kind != silent {
		t.Fatalf("typed prompt naming Lorenzo: %s %q", kind, r)
	}
	if lines := a.auditLines(); len(lines) != 1 {
		t.Fatalf("autosends: %q", lines)
	}
	// A typed marker that cannot be read is nobody.
	if personTyped(a.dir, "sched-404") || personTyped("", "sched-1") || personTyped(a.dir, "") {
		t.Fatal("presence without a marker")
	}
}

// The first prompt of a scheduled session is the task's: it never writes a
// send-intent flag, whatever the Desktop file says, even for a task that
// asks to send ("Run now" marks a run continued from its start). Later
// prompts are typed by a person and follow the usual rules.
func TestScheduledFirstPromptNeverFlags(t *testing.T) {
	a := newAuto(t)
	sched := PromptOptions{Scheduled: true}
	noScan(t)
	for _, session := range []string{"sched-1", "sched-cont", "sched-404", "plain-1"} {
		userPrompt(t, a.dir, session, "rispondi a Marco che arrivo", a.now) // an old flag goes too
		a.prompt(sched, session, "rispondi a Marco che arrivo")
		if _, err := os.Stat(flagPath(a.dir, session)); !os.IsNotExist(err) {
			t.Errorf("%s: first prompt left a flag: %v", session, err)
		}
	}
	// Other sessions' prompts prune flags, never the markers.
	userPrompt(t, a.dir, "other", "leggi", a.now.Add(3*time.Hour))
	a.prompt(sched, "sched-cont", "rispondi a Marco che arrivo")
	if _, err := os.Stat(flagPath(a.dir, "sched-cont")); err != nil {
		t.Fatalf("second prompt wrote no flag: %v", err)
	}
	// A marker that cannot be written counts as a first prompt.
	b := newAuto(t)
	b.write(filepath.Join(b.dir, "scheduled"), []byte("a file, not a folder"))
	b.prompt(sched, "sched-cont", "x")
	b.prompt(sched, "sched-cont", "rispondi a Marco che arrivo")
	if _, err := os.Stat(flagPath(b.dir, "sched-cont")); !os.IsNotExist(err) {
		t.Fatalf("flag written without a marker: %v", err)
	}
}

// Two hooks at once at the brake's edge: the log's lock lets one through.
func TestAutosendBrakeLock(t *testing.T) {
	a := newAuto(t)
	now := a.now.Add(time.Minute)
	var lines strings.Builder
	for i := range autosend.MaxPerDay - 1 {
		b, _ := json.Marshal(auditEntry{At: now.Add(-time.Duration(i) * time.Minute).UTC().Format(time.RFC3339), TaskID: "follow-up", JID: marcoLID})
		lines.Write(append(b, '\n'))
	}
	a.write(a.logPath(), []byte(lines.String()))
	in, _ := json.Marshal(map[string]any{"session_id": "sched-1", "tool_name": send, "tool_input": map[string]any{"to": "c_1", "text": "x"},
		"permission_mode": "default"})
	results := make(chan string, 6)
	for range 6 {
		go func() {
			var out bytes.Buffer
			PreTool(bytes.NewReader(in), &out, a.env(true))
			var got map[string]map[string]string
			json.Unmarshal(out.Bytes(), &got)
			results <- got["hookSpecificOutput"]["permissionDecision"]
		}()
	}
	allowed := 0
	for range 6 {
		if <-results == allow {
			allowed++
		}
	}
	if allowed != 1 || len(a.auditLines()) != autosend.MaxPerDay {
		t.Fatalf("%d allowed, %d log lines", allowed, len(a.auditLines()))
	}
	// A lock held by a live hook makes the send wait, then refuse.
	unlock, err := lockFile(a.logPath()+".lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if kind, r := a.send(a.env(true), "sched-1", "c_2"); kind != deny || !strings.Contains(r, "cannot lock the autosend log") {
		t.Fatalf("held lock: %s %q", kind, r)
	}
}

func TestRunWaitFor(t *testing.T) {
	for timeout, want := range map[time.Duration]time.Duration{
		30 * time.Second: 12 * time.Second, 20 * time.Second: 11 * time.Second, 10 * time.Second: time.Second, 5 * time.Second: 0,
	} {
		if got := RunWaitFor(timeout); got != want {
			t.Errorf("RunWaitFor(%s) = %s, want %s", timeout, got, want)
		}
	}
}

// A Desktop lookup that overruns its wait is denied; an approval check that
// overruns its budget is denied and leaves no receipt.
func TestAutosendBudget(t *testing.T) {
	a := newAuto(t)
	oldRun, oldTask := findRun, findTask
	defer func() { findRun, findTask = oldRun, oldTask }()
	findRun = func(ctx context.Context, _ []string, _ string) (desktop.Run, error) {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		return desktop.Run{TaskID: "follow-up", Dir: a.org}, nil
	}
	start := time.Now()
	kind, r := a.send(a.env(true), "sched-1", "c_1")
	if kind != deny || !strings.Contains(r, "did not show this run within 400ms") {
		t.Fatalf("%s %q", kind, r)
	}
	if took := time.Since(start); took > a.env(true).RunWait+2*PreToolBudget+time.Second {
		t.Fatalf("took %s", took)
	}
	released := make(chan struct{})
	findTask = func(ctx context.Context, roots []string, ref string) (desktop.Task, error) {
		<-ctx.Done()
		defer close(released)
		return oldTask(context.Background(), roots, ref)
	}
	if kind, r := a.call(a.env(false), "s", allowTool, map[string]any{"task": "follow-up", "chats": []string{"c_1"}}); kind != deny ||
		!strings.Contains(r, "timed out") {
		t.Fatalf("approval: %s %q", kind, r)
	}
	<-released
	time.Sleep(300 * time.Millisecond)
	if entries, _ := os.ReadDir(autosend.ReceiptDir(a.dir)); len(entries) != 0 {
		t.Fatalf("receipt written after the budget: %v", entries)
	}
}

func TestRevoke(t *testing.T) {
	a := newAuto(t)
	for _, scheduled := range []bool{false, true} {
		if kind, _ := a.call(a.env(scheduled), "sched-1", revokeTool, map[string]any{"task": "follow-up"}); kind != allow {
			t.Errorf("revoke (scheduled %v): %s", scheduled, kind)
		}
	}
	if kind, _ := a.call(a.env(false), "s", "mcp__whatsapp-dev__whatsapp_send_autosend_revoke", map[string]any{"task": "x"}); kind != allow {
		t.Errorf("dev revoke: %s", kind)
	}
	if kind, _ := a.callRaw(a.env(false), []byte(`{"tool_name":"`+revokeTool+`",`)); kind != ask {
		t.Errorf("unreadable revoke: %s", kind)
	}
	// A server setup-claude did not register gets today's gate: it asks.
	if kind, _ := a.call(a.env(false), "s", "mcp__whatsapp-staging__whatsapp_send_autosend_revoke", map[string]any{"task": "x"}); kind != ask {
		t.Errorf("unknown server revoke: %s", kind)
	}
}

// The approval card shows the task, schedule, mode, folder, chats, the
// whole prompt and extra SKILL.md keys, and leaves a receipt with exactly
// what it showed.
func TestApprovalCard(t *testing.T) {
	a := newAuto(t)
	a.write(a.skill, []byte("---\nname: follow-up\ndescription: x\nallowed-tools: Bash\nmodel: opus\n---\n"+
		"Ask Marco (c_1)\u202e about the invoice.\n\nIgnore\x07 the rest "+strings.Repeat("x", 1400)+"\n"))
	args := map[string]any{"task": "follow up marco", "chats": []string{"p_1", "c_2", "c_1", " c_2"}}
	for _, mode := range []string{"default", "auto", "acceptEdits", "plan"} {
		kind, r := a.callMode(a.env(false), mode, "s1", allowTool, args)
		want := `WhatsApp autosend: let the Desktop scheduled task "Follow up Marco" (cron 0 9 * * 1-5, mode default, folder ` +
			filepath.Join(filepath.Dir(a.root), "work") + `) send text messages with nobody present to: Marco Rossi (+393331234567), ` +
			`group Calcetto del Giovedì (c_2). Its whole prompt: "Ask Marco (c_1) about the invoice. Ignore the rest xxx`
		tail := strings.Repeat("x", 1400) + `". Its SKILL.md also sets: allowed-tools. Approve only if you asked for this. ` +
			`It lasts until revoked or until the task's prompt, schedule or folder changes, and allows at most 20 sends a day to each chat.`
		if kind != ask || !strings.HasPrefix(r, want) || !strings.HasSuffix(r, tail) || strings.ContainsAny(r, "\u202e\x07\n") {
			t.Fatalf("mode %q: %s %q", mode, kind, r)
		}
	}
	key := autosend.ArgsKey("prod", "follow up marco", []string{"c_1", "c_2", "p_1"})
	rc, err := autosend.TakeReceipt(a.dir, key, a.now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	task, _ := desktop.TaskByID(a.org, "follow-up")
	fp, _ := desktop.Fingerprint(task)
	if rc.Profile != "prod" || rc.TaskID != "follow-up" || rc.TaskName != "Follow up Marco" || rc.Schedule != "cron 0 9 * * 1-5" ||
		rc.TaskDir != a.org || rc.Fingerprint != fp || len(rc.Chats) != 2 || rc.Chats[0].JID != marcoLID || rc.Chats[0].Ref != "c_1" ||
		rc.Chats[1].JID != calcettoJ || rc.Chats[1].Ref != "c_2" || rc.At != a.now.Add(time.Minute).Unix() {
		t.Fatalf("receipt %+v", rc)
	}
	// The receipt folder survives the flag pruning of the next prompt.
	a.call(a.env(false), "s1", allowTool, args)
	userPrompt(t, a.dir, "s2", "leggi", a.now)
	if _, err := autosend.TakeReceipt(a.dir, key, a.now.Add(2*time.Minute)); err != nil {
		t.Fatalf("receipt after a prompt: %v", err)
	}
	// The dev server's card is for the dev profile.
	a.copyProfile("dev")
	if kind, _ := a.call(a.env(false), "s1", "mcp__whatsapp-dev__whatsapp_send_autosend_allow", args); kind != ask {
		t.Fatalf("dev card: %s", kind)
	}
	if _, err := autosend.TakeReceipt(a.dir, autosend.ArgsKey("dev", "follow up marco", []string{"c_1", "c_2", "p_1"}), a.now.Add(2*time.Minute)); err != nil {
		t.Fatalf("dev receipt: %v", err)
	}
	// A server setup-claude did not register gets today's gate: it asks, with no card and no receipt.
	kind, r := a.call(a.env(false), "s1", "mcp__my-whatsapp__whatsapp_send_autosend_allow", args)
	if kind != ask || strings.Contains(r, "autosend") {
		t.Fatalf("unknown server: %s %q", kind, r)
	}
	if entries, _ := os.ReadDir(autosend.ReceiptDir(a.dir)); len(entries) != 0 {
		t.Fatalf("receipt for an unknown server: %v", entries)
	}
}

func TestApprovalCardDenies(t *testing.T) {
	ok := map[string]any{"task": "follow-up", "chats": []string{"c_1"}}
	for _, c := range []struct {
		name, session, mode string
		scheduled           bool
		args                map[string]any
		setup               func(a *auto)
		env                 func(a *auto, e *PreToolEnv)
		why                 string
	}{
		{name: "no session", session: "", args: ok, why: "could not read this call"},
		{name: "scheduled env", session: "s1", scheduled: true, args: ok, why: "never by a scheduled run"},
		{name: "scheduled env, continued run", session: "sched-cont", scheduled: true, args: ok, why: "never by a scheduled run"},
		{name: "session in bypassPermissions", session: "s1", mode: "bypassPermissions", args: ok, why: "this session's permission mode is bypassPermissions, which may settle"},
		{name: "session in dontAsk", session: "s1", mode: "dontAsk", args: ok, why: "this session's permission mode is dontAsk, which may settle"},
		{name: "session without a mode", session: "s1", mode: "none", args: ok, why: "this session has no permission mode"},
		{name: "task without a mode, bypass in project settings", session: "s1", args: ok,
			why: "sets permissions.defaultMode to bypassPermissions: a run must use",
			setup: func(a *auto) {
				a.dropTaskMode()
				a.projectSettings("settings.json", `{"permissions": {"defaultMode": "bypassPermissions"}}`)
			}},
		{name: "task without a mode, unreadable user settings", session: "s1", args: ok,
			why:   "the default mode from your Claude Code settings cannot be read (read ",
			setup: func(a *auto) { a.dropTaskMode(); a.userSettings(`not json`) }},
		{name: "task in an unknown mode", session: "s1", args: ok, why: "the task's permission mode is yolo",
			setup: func(a *auto) { a.task["permissionMode"] = "yolo"; a.writeTasks() }},
		{name: "task not found", session: "s1", args: map[string]any{"task": "nope", "chats": []string{"c_1"}}, why: "no Claude Desktop scheduled task has the id or title \"nope\""},
		{name: "ambiguous name", session: "s1", args: map[string]any{"task": "twin", "chats": []string{"c_1"}}, why: "names 2 tasks: use the id",
			setup: func(a *auto) {
				a.writeTasks(map[string]any{"id": "a", "displayName": "Twin"}, map[string]any{"id": "b", "displayName": "twin"})
			}},
		{name: "SKILL.md missing", session: "s1", args: ok, why: "cannot read what the task runs", setup: func(a *auto) { os.Remove(a.skill) }},
		{name: "SKILL.md Desktop would not run", session: "s1", args: ok, why: "no front matter",
			setup: func(a *auto) { a.write(a.skill, []byte("--- \nname: x\ndescription: y\n---\nAsk")) }},
		{name: "relative filePath", session: "s1", args: ok, why: "not an absolute path",
			setup: func(a *auto) { a.task["filePath"] = "SKILL.md"; a.writeTasks() }},
		{name: "task in bypassPermissions", session: "s1", args: ok, why: "the task's permission mode is bypassPermissions",
			setup: func(a *auto) { a.task["permissionMode"] = "bypassPermissions"; a.writeTasks() }},
		{name: "task in dontAsk", session: "s1", args: ok, why: "the task's permission mode is dontAsk",
			setup: func(a *auto) { a.task["permissionMode"] = "dontAsk"; a.writeTasks() }},
		{name: "prompt too long", session: "s1", args: ok, why: "the task's prompt is too long to show on a card (1501 chars): shorten it to 1500.",
			setup: func(a *auto) { a.writeSkill(strings.Repeat("é", 1501)) }},
		{name: "no chats", session: "s1", args: map[string]any{"task": "follow-up", "chats": []string{" "}}, why: "name at least one chat"},
		{name: "chat by name", session: "s1", args: map[string]any{"task": "follow-up", "chats": []string{"c_1", "Marco Rossi"}},
			why: "cannot approve Marco Rossi: Marco Rossi is not a c_N or p_N id. Use c_N or p_N ids."},
		{name: "chat by phone", session: "s1", args: map[string]any{"task": "follow-up", "chats": []string{"+39 333 123 4567"}}, why: "Use c_N or p_N ids."},
		{name: "unknown chat", session: "s1", args: map[string]any{"task": "follow-up", "chats": []string{"c_99"}}, why: "cannot approve c_99"},
		{name: "receipt not written", session: "s1", args: ok, why: "cannot record the approval",
			env: func(a *auto, e *PreToolEnv) { e.Dir = filepath.Join(a.skill, "not-a-folder") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAuto(t)
			if c.setup != nil {
				c.setup(a)
			}
			env := a.env(c.scheduled)
			if c.env != nil {
				c.env(a, &env)
			}
			mode := c.mode
			switch mode {
			case "":
				mode = "default"
			case "none":
				mode = ""
			}
			kind, r := a.callMode(env, mode, c.session, allowTool, c.args)
			if kind != deny || !strings.HasPrefix(r, "WhatsApp autosend: ") || !strings.Contains(r, c.why) {
				t.Fatalf("%s %q, want deny with %q", kind, r, c.why)
			}
			if entries, _ := os.ReadDir(autosend.ReceiptDir(a.dir)); len(entries) != 0 {
				t.Fatalf("receipt written: %v", entries)
			}
		})
	}
}

// A second card for the same arguments cannot replace a waiting one with
// other content.
func TestApprovalCardConflict(t *testing.T) {
	a := newAuto(t)
	args := map[string]any{"task": "follow-up", "chats": []string{"c_1"}}
	if kind, _ := a.call(a.env(false), "s1", allowTool, args); kind != ask {
		t.Fatalf("first card: %s", kind)
	}
	if kind, _ := a.call(a.env(false), "s1", allowTool, args); kind != ask {
		t.Fatalf("same card again: %s", kind)
	}
	a.writeSkill("Ask Lorenzo (p_2) for money.")
	if kind, r := a.call(a.env(false), "s1", allowTool, args); kind != deny || !strings.Contains(r, "another approval card for these arguments is waiting") {
		t.Fatalf("changed card: %s %q", kind, r)
	}
}

// A hook binary older than the autosend tools reads an approval call as a
// send with no recipient: it asks, and writes no receipt.
func TestOldGateAsksForApprovals(t *testing.T) {
	a := newAuto(t)
	userPrompt(t, a.dir, "s1", "manda a Marco ogni mattina", a.now)
	for _, tool := range []string{allowTool, revokeTool} {
		in, _ := json.Marshal(map[string]any{"task": "follow-up", "chats": []string{"c_1"}})
		r, _ := askReason(toolInput{SessionID: "s1", ToolName: tool, ToolInput: in}, nil, a.env(false))
		if !strings.Contains(r, "cannot check the recipient an unnamed recipient") {
			t.Errorf("%s: %q", tool, r)
		}
	}
}

// Reasons keep up to 300 characters of an internal error and cut text with
// ASCII dots: some hosts turn other characters into replacement marks.
func TestReasonTextIsASCIICut(t *testing.T) {
	long := strings.Repeat("é", 500)
	if got := displayN(long, 80); len([]rune(got)) != 80 || !strings.HasSuffix(got, "...") {
		t.Fatalf("displayN: %q", got)
	}
	if got := errText(errors.New(long)); len([]rune(got)) != 300 || !strings.HasSuffix(got, "...") {
		t.Fatalf("errText: %d runes", len([]rune(got)))
	}
	if got := display("short"); got != "short" {
		t.Fatalf("display: %q", got)
	}
}

// A task saved without a mode runs in the default mode its settings give
// it: the card shows where the mode comes from, and runs autosend while
// that mode prompts.
func TestTaskModeFromSettings(t *testing.T) {
	a := newAuto(t)
	a.dropTaskMode()
	args := map[string]any{"task": "follow-up", "chats": []string{"c_1", "c_2"}}
	for _, tc := range []struct{ user, project, local, card string }{
		{card: "mode default (from your settings)"},
		{user: "plan", card: "mode plan (from your settings)"},
		{user: "bypassPermissions", project: "acceptEdits", card: "mode acceptEdits (from your settings)"},
		{user: "bypassPermissions", project: "dontAsk", local: "auto", card: "mode auto (from your settings)"},
	} {
		for file, mode := range map[string]string{"user": tc.user, "settings.json": tc.project, "settings.local.json": tc.local} {
			body := `{}`
			if mode != "" {
				body = `{"permissions": {"defaultMode": "` + mode + `"}}`
			}
			if file == "user" {
				a.userSettings(body)
			} else {
				a.projectSettings(file, body)
			}
		}
		if kind, r := a.callMode(a.env(false), "default", "s1", allowTool, args); kind != ask || !strings.Contains(r, ", "+tc.card+", folder ") {
			t.Fatalf("%+v: %s %q", tc, kind, r)
		}
		if kind, r := a.send(a.env(true), "sched-1", "c_1"); kind != allow {
			t.Fatalf("%+v autosend: %s %q", tc, kind, r)
		}
	}
	// The task's own mode wins over every settings file.
	a.task["permissionMode"] = "plan"
	a.writeTasks()
	a.projectSettings("settings.local.json", `{"permissions": {"defaultMode": "bypassPermissions"}}`)
	if kind, r := a.callMode(a.env(false), "default", "s1", allowTool, args); kind != ask || !strings.Contains(r, ", mode plan, folder ") {
		t.Fatalf("task mode: %s %q", kind, r)
	}
}
