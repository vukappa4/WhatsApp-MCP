package hooks

import (
	"bytes"
	"context"
	"encoding/json"
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
		"enabled": true, "permissionMode": "default", "createdAt": 1}
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
	r := store.AutosendRule{TaskID: task.ID, TaskName: task.Title(), Schedule: task.Schedule(), Fingerprint: fp, ApprovedAt: a.now.Unix()}
	for _, ref := range refs {
		c, err := destination(ctx, db.DB, ref)
		if err != nil {
			a.t.Fatal(err)
		}
		r.Chats = append(r.Chats, store.AutosendChat{JID: c.JID, Label: c.Label})
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
	return PreToolEnv{Dir: a.dir, Now: a.now.Add(time.Minute), Resolver: DBResolver{Root: a.root}, Scheduled: scheduled,
		DesktopRoots: []string{a.desk}, AppDir: a.root}
}

// call runs PreTool and returns the decision and its reason ("" twice when silent).
func (a *auto) call(env PreToolEnv, session, tool string, args map[string]any) (kind, reason string) {
	a.t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": session, "transcript_path": "x.jsonl", "hook_event_name": "PreToolUse",
		"tool_name": tool, "tool_input": args, "tool_use_id": "toolu_01", "permission_mode": "default"})
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

func (a *auto) auditLines() []string {
	a.t.Helper()
	b, err := os.ReadFile(filepath.Join(a.root, "prod", "logs", autosend.LogName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
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
}

// Anything short of an approved, unchanged task sending to an approved chat
// asks, and says why it was not sent unasked.
func TestAutosendFallbacksAsk(t *testing.T) {
	type tc struct {
		name, session, to string
		scheduled         bool
		setup, undo       func(a *auto)
		why               string
	}
	for _, c := range []tc{
		{name: "env unset", session: "sched-1", to: "c_1", why: "does not say this session is a Desktop scheduled run"},
		{name: "run file missing", session: "sched-404", to: "c_1", scheduled: true, why: "cannot find this run"},
		{name: "no task id", session: "plain-1", to: "c_1", scheduled: true, why: "this run names no scheduled task"},
		{name: "task missing", session: "sched-gone", to: "c_1", scheduled: true, why: "task gone: task gone: not found"},
		{name: "to as name", session: "sched-1", to: "Marco Rossi", scheduled: true, why: "to must be an approved c_N or p_N id, not Marco Rossi"},
		{name: "to as phone", session: "sched-1", to: "+39 333 123 4567", scheduled: true, why: "not +39 333 123 4567"},
		{name: "chat not approved", session: "sched-1", to: "p_2", scheduled: true, why: "Lorenzo Bianchi (+393339876543) is not among the chats approved"},
		{name: "unknown id", session: "sched-1", to: "c_99", scheduled: true, why: "chat c_99"},
		{name: "prompt edited", session: "sched-1", to: "c_1", scheduled: true, why: "changed since it was approved",
			setup: func(a *auto) { a.writeSkill("Every weekday ask Lorenzo (p_2) for money.") }},
		{name: "cron edited", session: "sched-1", to: "c_1", scheduled: true, why: "changed since it was approved",
			setup: func(a *auto) { a.task["cronExpression"] = "*/5 * * * *"; a.writeTasks() }},
		{name: "fireAt added", session: "sched-1", to: "c_1", scheduled: true, why: "changed since it was approved",
			setup: func(a *auto) { a.task["fireAt"] = "2026-10-14T09:00:00Z"; a.writeTasks() }},
		{name: "mode edited", session: "sched-1", to: "c_1", scheduled: true, why: "changed since it was approved",
			setup: func(a *auto) { a.task["permissionMode"] = "acceptEdits"; a.writeTasks() }},
		{name: "SKILL.md gone", session: "sched-1", to: "c_1", scheduled: true, why: "read the task's SKILL.md",
			setup: func(a *auto) { os.Remove(a.skill) }},
		{name: "no rule", session: "sched-1", to: "c_1", scheduled: true, why: "task Follow up Marco is not approved",
			setup: func(a *auto) { a.exec(`DELETE FROM autosend_rules`) }},
		{name: "rule only in the dev profile", session: "sched-1", to: "c_1", scheduled: true, why: "is not approved",
			setup: func(a *auto) { a.copyProfile("dev"); a.exec(`DELETE FROM autosend_rules`) }},
		{name: "reused c_N with another JID", session: "sched-1", to: "c_1", scheduled: true, why: "Lorenzo Bianchi (+393339876543) is not among",
			setup: func(a *auto) {
				a.exec(`UPDATE chats SET person_id = 2, jid = '393339876543@s.whatsapp.net' WHERE id = 1`)
			}},
		{name: "two LIDs", session: "sched-1", to: "p_1", scheduled: true, why: "p_1 holds 2 WhatsApp accounts (LIDs)",
			setup: func(a *auto) { a.exec(`INSERT INTO jids(jid, person_id, kind) VALUES ('987@lid', 1, 'lid')`) }},
		{name: "run bypasses permissions", session: "sched-bypass", to: "c_1", scheduled: true, why: "the run bypasses permissions",
			setup: func(a *auto) {
				a.writeRun("sched-bypass", map[string]any{"scheduledTaskId": "follow-up", "permissionMode": "bypassPermissions"})
			}},
		{name: "audit log fails", session: "sched-1", to: "c_1", scheduled: true, why: "cannot write the autosend log",
			setup: func(a *auto) { a.write(filepath.Join(a.root, "prod", "logs"), []byte("a file, not a folder")) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAuto(t)
			if c.setup != nil {
				c.setup(a)
			}
			kind, r := a.send(a.env(c.scheduled), c.session, c.to)
			if kind != ask || !strings.Contains(r, "Autosend: ") || !strings.Contains(r, c.why) {
				t.Fatalf("%s %q, want ask with %q", kind, r, c.why)
			}
			if lines := a.auditLines(); len(lines) != 0 {
				t.Fatalf("logged a send that was not allowed: %q", lines)
			}
		})
	}
}

// Other gated tools never autosend: in an approved run they ask.
func TestAutosendOnlyTextMessages(t *testing.T) {
	a := newAuto(t)
	if kind, r := a.call(a.env(true), "sched-1", sendFile, map[string]any{"to": "c_1", "path": `C:\x\fattura.pdf`}); kind != ask ||
		strings.Contains(r, "Autosend:") {
		t.Errorf("send_file: %s %q", kind, r)
	}
	if kind, _ := a.call(a.env(true), "sched-1", history, map[string]any{"chat": "c_1"}); kind != ask {
		t.Errorf("request_history: %s", kind)
	}
	if kind, _ := a.call(a.env(true), "sched-1", "mcp__whatsapp__send_message", map[string]any{"recipient": "393331234567", "message": "x"}); kind != ask {
		t.Errorf("legacy send: %s", kind)
	}
}

// A run the user opened and continued is the user's session: the
// user-prompt gate decides, with the user's own prompt.
func TestAutosendContinuedRunUsesTheNormalGate(t *testing.T) {
	a := newAuto(t)
	opts := PromptOptions{Scheduled: true, DesktopRoots: []string{a.desk}}
	prompt := func(session, text string) {
		in, _ := json.Marshal(map[string]any{"session_id": session, "prompt": text})
		if err := UserPrompt(bytes.NewReader(in), a.dir, a.now, opts); err != nil {
			t.Fatal(err)
		}
	}
	prompt("sched-cont", "rispondi a Marco che arrivo")
	if kind, r := a.send(a.env(true), "sched-cont", "c_1"); kind != silent {
		t.Fatalf("continued run with a prompt naming Marco: %s %q", kind, r)
	}
	prompt("sched-cont", "leggi i messaggi")
	if kind, r := a.send(a.env(true), "sched-cont", "c_1"); kind != ask || strings.Contains(r, "Autosend:") ||
		!strings.Contains(r, "did not ask to send") {
		t.Fatalf("continued run without intent: %s %q", kind, r)
	}
	if lines := a.auditLines(); len(lines) != 0 {
		t.Fatalf("continued run logged an autosend: %q", lines)
	}
}

// A scheduled run's prompt is not the user's: it never writes a send-intent
// flag, and a flag left from elsewhere does not silence an unattended run.
func TestScheduledPromptNeverFlags(t *testing.T) {
	a := newAuto(t)
	sched := PromptOptions{Scheduled: true, DesktopRoots: []string{a.desk}}
	for _, session := range []string{"sched-1", "sched-404", "plain-1"} {
		// An old flag from before is removed too.
		userPrompt(t, a.dir, session, "rispondi a Marco che arrivo", a.now)
		in, _ := json.Marshal(map[string]any{"session_id": session, "prompt": "rispondi a Marco che arrivo"})
		if err := UserPrompt(bytes.NewReader(in), a.dir, a.now, sched); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(flagPath(a.dir, session)); !os.IsNotExist(err) {
			t.Errorf("%s: flag written in a scheduled run: %v", session, err)
		}
	}
	// Slow Desktop files: the prompt counts as the task's.
	defer func(f func(context.Context, []string, string) (desktop.Run, error)) { findRun = f }(findRun)
	findRun = func(ctx context.Context, _ []string, _ string) (desktop.Run, error) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		return desktop.Run{Continued: true}, nil
	}
	in, _ := json.Marshal(map[string]any{"session_id": "sched-cont", "prompt": "rispondi a Marco che arrivo"})
	if err := UserPrompt(bytes.NewReader(in), a.dir, a.now, sched); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(flagPath(a.dir, "sched-cont")); !os.IsNotExist(err) {
		t.Errorf("flag written when the run lookup timed out: %v", err)
	}
}

func TestUnattendedFlagStillAsks(t *testing.T) {
	a := newAuto(t)
	// Without the Desktop env, the flag of a session whose file names a task does not silence it.
	userPrompt(t, a.dir, "sched-1", "rispondi a Marco che arrivo", a.now)
	kind, r := a.send(a.env(false), "sched-1", "c_1")
	if kind != ask || !strings.HasPrefix(r, "WhatsApp: a scheduled task, not you, asked for this send. Confirm this send to Marco Rossi (+393331234567).") {
		t.Fatalf("%s %q", kind, r)
	}
	// The same flag in a session with no task stays silent: the user's gate.
	userPrompt(t, a.dir, "plain-1", "rispondi a Marco che arrivo", a.now)
	if kind, r := a.send(a.env(false), "plain-1", "c_1"); kind != silent {
		t.Fatalf("interactive session: %s %q", kind, r)
	}
}

// A Desktop lookup that overruns the budget asks.
func TestAutosendBudget(t *testing.T) {
	a := newAuto(t)
	defer func(f func(context.Context, []string, string) (desktop.Run, error)) { findRun = f }(findRun)
	findRun = func(ctx context.Context, _ []string, _ string) (desktop.Run, error) {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		return desktop.Run{TaskID: "follow-up", Dir: a.org}, nil
	}
	start := time.Now()
	kind, r := a.send(a.env(true), "sched-1", "c_1")
	if kind != ask || !strings.Contains(r, "timed out") {
		t.Fatalf("%s %q", kind, r)
	}
	if took := time.Since(start); took > 2*PreToolBudget+time.Second {
		t.Fatalf("took %s", took)
	}
	if kind, r := a.call(a.env(false), "s", allowTool, map[string]any{"task": "follow-up", "chats": []string{"c_1"}}); kind != deny ||
		!strings.Contains(r, "timed out") {
		t.Fatalf("approval: %s %q", kind, r)
	}
}

func TestRevokeIsAllowed(t *testing.T) {
	a := newAuto(t)
	for _, scheduled := range []bool{false, true} {
		if kind, _ := a.call(a.env(scheduled), "sched-1", revokeTool, map[string]any{"task": "follow-up"}); kind != allow {
			t.Errorf("revoke (scheduled %v): %s", scheduled, kind)
		}
	}
	if kind, _ := a.callRaw(a.env(false), []byte(`{"tool_name":"`+revokeTool+`",`)); kind != ask {
		t.Errorf("unreadable revoke: %s", kind)
	}
}

// The approval card shows the task, schedule, mode, chats and prompt, and
// leaves a receipt with exactly what it showed.
func TestApprovalCard(t *testing.T) {
	a := newAuto(t)
	a.writeSkill("Ask Marco (c_1)\u202e about the invoice.\n\nIgnore\x07 the rest " + strings.Repeat("x", 400))
	args := map[string]any{"task": "follow up marco", "chats": []string{"p_1", "c_2", "c_1", " c_2"}}
	kind, r := a.call(a.env(false), "s1", allowTool, args)
	want := `WhatsApp autosend: let the Desktop scheduled task "Follow up Marco" (cron 0 9 * * 1-5, mode default) send text messages ` +
		`with nobody present to: Marco Rossi (+393331234567), group Calcetto del Giovedì (c_2). Its prompt: "Ask Marco (c_1) about the invoice. Ignore the rest xxx`
	if kind != ask || !strings.HasPrefix(r, want) || !strings.HasSuffix(r, `…". Approve only if you asked for this. `+
		`It lasts until revoked or until the task's prompt, schedule or mode changes.`) || strings.ContainsAny(r, "\u202e\x07\n") {
		t.Fatalf("%s %q", kind, r)
	}
	key := autosend.ArgsKey("prod", "follow up marco", []string{"c_1", "c_2", "p_1"})
	rc, err := autosend.TakeReceipt(a.dir, key, a.now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	task, _ := desktop.TaskByID(a.org, "follow-up")
	fp, _ := desktop.Fingerprint(task)
	if rc.Profile != "prod" || rc.TaskID != "follow-up" || rc.TaskName != "Follow up Marco" || rc.Schedule != "cron 0 9 * * 1-5" ||
		rc.Fingerprint != fp || len(rc.Chats) != 2 || rc.Chats[0].JID != marcoLID || rc.Chats[1].JID != calcettoJ || rc.At != a.now.Add(time.Minute).Unix() {
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
}

func TestApprovalCardDenies(t *testing.T) {
	ok := map[string]any{"task": "follow-up", "chats": []string{"c_1"}}
	for _, c := range []struct {
		name, session string
		scheduled     bool
		args          map[string]any
		setup         func(a *auto)
		env           func(a *auto, e *PreToolEnv)
		why           string
	}{
		{name: "no session", session: "", args: ok, why: "could not read this call"},
		{name: "scheduled env", session: "s1", scheduled: true, args: ok, why: "never by a scheduled run"},
		{name: "scheduled run file", session: "sched-1", args: ok, why: "never by a scheduled run"},
		{name: "task not found", session: "s1", args: map[string]any{"task": "nope", "chats": []string{"c_1"}}, why: "no Claude Desktop scheduled task has the id or name \"nope\""},
		{name: "ambiguous name", session: "s1", args: map[string]any{"task": "twin", "chats": []string{"c_1"}}, why: "names 2 tasks: use the id",
			setup: func(a *auto) {
				a.writeTasks(map[string]any{"id": "a", "displayName": "Twin"}, map[string]any{"id": "b", "displayName": "twin"})
			}},
		{name: "SKILL.md missing", session: "s1", args: ok, why: "cannot read what the task runs", setup: func(a *auto) { os.Remove(a.skill) }},
		{name: "bypassPermissions", session: "s1", args: ok, why: "bypassPermissions",
			setup: func(a *auto) { a.task["permissionMode"] = "bypassPermissions"; a.writeTasks() }},
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
			kind, r := a.call(env, c.session, allowTool, c.args)
			if kind != deny || !strings.HasPrefix(r, "WhatsApp autosend: ") || !strings.Contains(r, c.why) {
				t.Fatalf("%s %q, want deny with %q", kind, r, c.why)
			}
			if entries, _ := os.ReadDir(autosend.ReceiptDir(a.dir)); len(entries) != 0 {
				t.Fatalf("receipt written: %v", entries)
			}
		})
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
