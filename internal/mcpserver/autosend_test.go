package mcpserver

import (
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

// autosendEnv is a send environment (profile dev) with a hooks dir and a
// Claude Desktop folder holding the task "follow-up".
type autosendEnv struct {
	*sendEnv
	hooks, org, skill string
}

func newAutosendEnv(t *testing.T) *autosendEnv {
	t.Helper()
	base := t.TempDir()
	a := &autosendEnv{hooks: filepath.Join(base, "hooks"), org: filepath.Join(base, "sessions", "acct", "org"),
		skill: filepath.Join(base, "claude", "scheduled-tasks", "follow-up", "SKILL.md")}
	a.sendEnv = newSendEnvWith(t, func(e *sendEnv) {
		e.srv.HooksDir = a.hooks
		e.srv.DesktopRoots = []string{filepath.Join(base, "sessions")}
	})
	a.writeFile(t, a.skill, "---\nname: follow-up\ndescription: Follow up\n---\nAsk c_1 about the invoice.\n")
	a.writeTasks(t, "0 9 * * 1-5")
	return a
}

func (a *autosendEnv) writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (a *autosendEnv) writeTasks(t *testing.T, cron string) {
	b, _ := json.Marshal(map[string]any{"scheduledTasks": []map[string]any{{"id": "follow-up", "displayName": "Follow up",
		"filePath": a.skill, "cronExpression": cron, "permissionMode": "default"}}})
	a.writeFile(t, filepath.Join(a.org, desktop.TasksFile), string(b))
}

func (a *autosendEnv) fingerprint(t *testing.T) string {
	t.Helper()
	task, err := desktop.TaskByID(a.org, "follow-up")
	if err != nil {
		t.Fatal(err)
	}
	fp, err := desktop.Fingerprint(task)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// jid is where a send to ref goes in the fixture.
func (a *autosendEnv) jid(t *testing.T, ref string) string {
	t.Helper()
	jid, why, err := a.srv.refJID(context.Background(), ref)
	if err != nil || why != "" {
		t.Fatalf("%s: %s %v", ref, why, err)
	}
	return jid
}

// receipt writes what the send gate writes when it shows the card: p_1 and c_2.
func (a *autosendEnv) receipt(t *testing.T, profile string, at time.Time, task string, chats ...string) {
	t.Helper()
	r := autosend.Receipt{At: at.Unix(), Profile: profile, TaskID: "follow-up", TaskName: "Follow up", TaskDir: a.org, Schedule: "cron 0 9 * * 1-5",
		Fingerprint: a.fingerprint(t), Chats: []autosend.Chat{{Ref: "p_1", JID: a.jid(t, "p_1"), Label: "Marco Rossi (+39 333 111 1111)"},
			{Ref: "c_2", JID: a.jid(t, "c_2"), Label: "group Calcetto (c_2)"}}}
	if err := autosend.WriteReceipt(a.hooks, autosend.ArgsKey(profile, task, chats), r); err != nil {
		t.Fatal(err)
	}
}

func TestAutosendToolAnnotations(t *testing.T) {
	e := newSendEnv(t)
	tools, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tl := range tools.Tools {
		a := tl.Annotations
		switch tl.Name {
		case toolAutosendAllow:
			if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint || a.IdempotentHint {
				t.Errorf("%s annotations = %+v", tl.Name, a)
			}
		case toolAutosendRevoke:
			if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint || !a.IdempotentHint {
				t.Errorf("%s annotations = %+v", tl.Name, a)
			}
		default:
			continue
		}
		seen++
		// An older gate reads these as a send with no recipient: no argument may look like one.
		b, _ := json.Marshal(tl.InputSchema)
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		json.Unmarshal(b, &schema)
		for _, p := range []string{"to", "recipient", "chat", "path", "media_path"} {
			if _, ok := schema.Properties[p]; ok {
				t.Errorf("%s has an argument %s", tl.Name, p)
			}
		}
		if len(schema.Properties) == 0 {
			t.Errorf("%s: no properties in %s", tl.Name, b)
		}
	}
	if seen != 2 {
		t.Fatalf("autosend tools listed: %d", seen)
	}
}

func TestAutosendAllowNeedsTheGatesReceipt(t *testing.T) {
	a := newAutosendEnv(t)
	now := a.srv.now()
	args := map[string]any{"task": "follow-up", "chats": []string{"p_1", "c_2"}}
	text, isErr := call(t, a.cs, toolAutosendAllow, args)
	if !isErr || !strings.Contains(text, `Not approved: the send gate did not ask the user for exactly these arguments (task "follow-up", chats c_2, p_1).`) ||
		!strings.Contains(text, "update it (prod), restart Claude Code, and call again") {
		t.Fatalf("no receipt:\n%s", text)
	}
	// Other arguments, another profile's key, an expired or future receipt: not approved.
	a.receipt(t, "dev", now, "follow-up", "p_1")
	a.receipt(t, "prod", now, "follow-up", "p_1", "c_2")
	for name, at := range map[string]time.Time{"expired": now.Add(-autosend.ReceiptTTL - time.Minute), "future": now.Add(5 * time.Minute)} {
		a.receipt(t, "dev", at, "follow-up", "p_1", "c_2")
		if text, isErr := call(t, a.cs, toolAutosendAllow, args); !isErr || !strings.Contains(text, "Not approved") {
			t.Fatalf("%s receipt:\n%s", name, text)
		}
	}
	// A receipt under this profile's key that names another profile: refused.
	r := autosend.Receipt{At: now.Unix(), Profile: "prod", TaskID: "follow-up", Fingerprint: "x", Chats: []autosend.Chat{{Ref: "p_1", JID: "1@lid", Label: "x"}}}
	if err := autosend.WriteReceipt(a.hooks, autosend.ArgsKey("dev", "follow-up", []string{"c_2", "p_1"}), r); err != nil {
		t.Fatal(err)
	}
	if text, isErr := call(t, a.cs, toolAutosendAllow, args); !isErr || !strings.Contains(text, "approved this for the prod profile") {
		t.Fatalf("wrong profile:\n%s", text)
	}
	if rs, _ := store.AutosendRules(context.Background(), a.db); len(rs) != 0 {
		t.Fatalf("rules stored: %+v", rs)
	}

	// The receipt for exactly these arguments stores the rule, once.
	a.receipt(t, "dev", now.Add(-time.Minute), " follow-up ", "c_2", "p_1", "p_1")
	text, isErr = call(t, a.cs, toolAutosendAllow, args)
	if isErr || !strings.Contains(text, "Approved: this Desktop scheduled task may send text messages with nobody present:") ||
		!strings.Contains(text, `to "Marco Rossi (+39 333 111 1111)"`) || !strings.Contains(text, "to set to one of: c_2, p_1.") {
		t.Fatalf("approved:\n%s", text)
	}
	rule, err := store.AutosendRuleFor(context.Background(), a.db, "follow-up")
	if err != nil || rule.Fingerprint != a.fingerprint(t) || rule.ApprovedAt != now.Unix() || len(rule.Chats) != 2 || rule.TaskDir != a.org ||
		rule.Chats[0].JID != a.jid(t, "p_1") || rule.Chats[0].Ref != "p_1" || rule.TaskName != "Follow up" {
		t.Fatalf("rule %+v %v", rule, err)
	}
	if text, isErr := call(t, a.cs, toolAutosendAllow, args); !isErr || !strings.Contains(text, "Not approved") {
		t.Fatalf("receipt used twice:\n%s", text)
	}
}

func TestAutosendRevokeAndStatus(t *testing.T) {
	a := newAutosendEnv(t)
	ctx := context.Background()
	now := a.srv.now()
	text, _ := call(t, a.cs, toolStatus, nil)
	if !strings.Contains(text, "Autosend: no Desktop scheduled task may send without asking") {
		t.Fatalf("status without rules:\n%s", text)
	}
	put := func(id, name, fp string) {
		t.Helper()
		if err := store.PutAutosendRule(ctx, a.db, store.AutosendRule{TaskID: id, TaskDir: a.org, TaskName: name, Schedule: "cron 0 9 * * 1-5", Fingerprint: fp,
			ApprovedAt: now.Add(-time.Hour).Unix(), Chats: []store.AutosendChat{{JID: a.jid(t, "p_1"), Ref: "p_1", Label: "~Evil\" (+39 333 111 1111)"},
				{JID: "1203@g.us", Ref: "c_2", Label: "group Calcetto (c_2)"}}}); err != nil {
			t.Fatal(err)
		}
	}
	put("follow-up", "Follow up", a.fingerprint(t))
	put("gone", "Gone task", "x")
	a.writeFile(t, filepath.Join(a.srv.DataDir, "logs", autosend.LogName),
		`{"at":"2026-10-13T08:00:00Z","task_name":"Follow up","label":"Marco Rossi","text_chars":12}`+"\nnot json\n")
	state := func() string {
		t.Helper()
		text, isErr := call(t, a.cs, toolStatus, nil)
		if isErr {
			t.Fatal(text)
		}
		return text
	}
	text = state()
	for _, want := range []string{"Autosend (2 approved Desktop scheduled tasks", `task "Follow up" ("cron 0 9 * * 1-5"), id "follow-up"`,
		`active; to "~Evil\" (+39 333 111 1111)", "group Calcetto (c_2)" (c_2: chat changed: approve again)`, `task "Gone task"`, "inactive: task not found",
		`by task "Follow up" to "Marco Rossi", 12 characters`, "<untrusted_wa n="} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}
	a.db.Exec(`UPDATE autosend_rules SET task_dir = 'elsewhere' WHERE task_id = 'follow-up'`)
	if text := state(); !strings.Contains(text, "inactive: the task is in another Claude Desktop account or organization") {
		t.Errorf("other folder:\n%s", text)
	}
	a.db.Exec(`UPDATE autosend_rules SET task_dir = ? WHERE task_id = 'follow-up'`, a.org)
	// The permission mode is not in the fingerprint: a promptless one blocks the rule while it lasts.
	b, _ := json.Marshal(map[string]any{"scheduledTasks": []map[string]any{{"id": "follow-up", "displayName": "Follow up",
		"filePath": a.skill, "cronExpression": "0 9 * * 1-5", "permissionMode": "dontAsk"}}})
	a.writeFile(t, filepath.Join(a.org, desktop.TasksFile), string(b))
	if text := state(); !strings.Contains(text, "inactive: the task's permission mode is dontAsk") {
		t.Errorf("dontAsk task:\n%s", text)
	}
	a.writeTasks(t, "*/5 * * * *")
	if text := state(); !strings.Contains(text, "inactive: the task's prompt, schedule or folder changed since approval") {
		t.Errorf("changed task:\n%s", text)
	}
	os.Remove(a.skill)
	if text := state(); !strings.Contains(text, "unknown: read the task's SKILL.md") {
		t.Errorf("unreadable SKILL.md:\n%s", text)
	}

	if text, isErr := call(t, a.cs, toolAutosendRevoke, map[string]any{"task": "nope"}); !isErr || !strings.Contains(text, `No autosend approval for task "nope"`) {
		t.Fatalf("revoke missing:\n%s", text)
	}
	if text, isErr := call(t, a.cs, toolAutosendRevoke, map[string]any{"task": "follow-up"}); isErr || !strings.Contains(text, "Revoked") {
		t.Fatalf("revoke by id:\n%s", text)
	}
	if text, isErr := call(t, a.cs, toolAutosendRevoke, map[string]any{"task": "gone TASK"}); isErr || !strings.Contains(text, "Revoked") {
		t.Fatalf("revoke by name:\n%s", text)
	}
	if rs, _ := store.AutosendRules(ctx, a.db); len(rs) != 0 {
		t.Fatalf("rules left: %+v", rs)
	}
}

// A receipt that the daemon's own data or Desktop's files contradict since
// the card was shown stores nothing.
func TestAutosendAllowRechecksTheReceipt(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, a *autosendEnv){
		"task schedule": func(t *testing.T, a *autosendEnv) { a.writeTasks(t, "*/5 * * * *") },
		"task prompt": func(t *testing.T, a *autosendEnv) {
			a.writeFile(t, a.skill, "---\nname: follow-up\ndescription: x\n---\nAsk p_2 for money.\n")
		},
		"task gone": func(t *testing.T, a *autosendEnv) { os.Remove(filepath.Join(a.org, desktop.TasksFile)) },
		"person's account": func(t *testing.T, a *autosendEnv) {
			if _, err := a.db.Exec(`DELETE FROM jids WHERE person_id = 1 AND kind = 'lid'`); err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.Exec(`INSERT INTO jids(jid, person_id, kind) VALUES ('424242@lid', 1, 'lid')`); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAutosendEnv(t)
			a.receipt(t, "dev", a.srv.now(), "follow-up", "p_1", "c_2")
			change(t, a)
			text, isErr := call(t, a.cs, toolAutosendAllow, map[string]any{"task": "follow-up", "chats": []string{"p_1", "c_2"}})
			if !isErr || !strings.Contains(text, "since the card was shown. Call again to show the user a new card.") {
				t.Fatalf("%s", text)
			}
			if rs, _ := store.AutosendRules(context.Background(), a.db); len(rs) != 0 {
				t.Fatalf("rules stored: %+v", rs)
			}
		})
	}
}
