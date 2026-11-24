package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture is a Desktop sessions root with one account/org folder.
type fixture struct {
	t    *testing.T
	root string
	org  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "claude-code-sessions")
	f := &fixture{t: t, root: root, org: filepath.Join(root, "acct-1", "org-1")}
	if err := os.MkdirAll(f.org, 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(path string, v any, mod time.Time) {
	f.t.Helper()
	var b []byte
	switch x := v.(type) {
	case string:
		b = []byte(x)
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		f.t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) run(name string, fields map[string]any, mod time.Time) {
	f.t.Helper()
	f.write(filepath.Join(f.org, "local_"+name+".json"), fields, mod)
}

// skill writes a SKILL.md with front matter and returns its path.
func (f *fixture) skill(id, body string) string {
	f.t.Helper()
	p := filepath.Join(f.root, "..", "config", "scheduled-tasks", id, "SKILL.md")
	f.write(p, "---\nname: "+id+"\ndescription: a test task\n---\n"+body, time.Time{})
	return p
}

func (f *fixture) tasks(dir string, ts ...map[string]any) {
	f.t.Helper()
	f.write(filepath.Join(dir, TasksFile), map[string]any{"scheduledTasks": ts}, time.Time{})
}

func TestFindRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	f.run("a", map[string]any{"sessionId": "local_a", "cliSessionId": "cli-1", "scheduledTaskId": "follow-up",
		"scheduledRunContinued": false, "permissionMode": "default", "title": "x"}, now)
	f.run("b", map[string]any{"sessionId": "local_b", "cliSessionId": "cli-2"}, now.Add(-time.Hour))
	f.run("c", map[string]any{"sessionId": "local_c", "cliSessionId": "cli-3", "scheduledTaskId": "follow-up", "scheduledRunContinued": true}, now)
	f.write(filepath.Join(f.org, "local_corrupt.json"), `{"cliSessionId": "cli-9", `, time.Time{})
	f.write(filepath.Join(f.org, "local_other.json"), `not json at all`, time.Time{})

	r, err := FindRun(ctx, []string{f.root}, "cli-1")
	if err != nil || r.TaskID != "follow-up" || r.Continued || r.SessionID != "local_a" || r.Dir != f.org || r.PermissionMode != "default" {
		t.Fatalf("match: %+v %v", r, err)
	}
	if r, err := FindRun(ctx, []string{f.root}, "cli-2"); err != nil || r.TaskID != "" {
		t.Fatalf("no task id: %+v %v", r, err)
	}
	if r, err := FindRun(ctx, []string{f.root}, "cli-3"); err != nil || !r.Continued {
		t.Fatalf("continued: %+v %v", r, err)
	}
	if _, err := FindRun(ctx, []string{f.root}, "cli-404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("none: %v", err)
	}
	if _, err := FindRun(ctx, []string{f.root}, ""); err == nil {
		t.Fatal("empty id accepted")
	}
	// A corrupt file holding the id cannot be ruled out: error.
	if _, err := FindRun(ctx, []string{f.root}, "cli-9"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt: %v", err)
	}
	// An id that is a prefix of another is no match.
	if _, err := FindRun(ctx, []string{f.root}, "cli"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("prefix: %v", err)
	}
	// Two files name the session: error.
	f.run("d", map[string]any{"cliSessionId": "cli-2"}, now)
	if _, err := FindRun(ctx, []string{f.root}, "cli-2"); err == nil || !strings.Contains(err.Error(), "2 Desktop session files") {
		t.Fatalf("two: %v", err)
	}
	// Over 4 MB: error when the id is in it, skipped otherwise.
	big := `{"cliSessionId": "cli-big", "pad": "` + strings.Repeat("x", maxFile) + `"}`
	f.write(filepath.Join(f.org, "local_big.json"), big, time.Time{})
	if _, err := FindRun(ctx, []string{f.root}, "cli-big"); err == nil || !strings.Contains(err.Error(), "over 4 MB") {
		t.Fatalf("oversize: %v", err)
	}
	if _, err := FindRun(ctx, []string{f.root}, "cli-1"); err != nil {
		t.Fatalf("oversize file without the id: %v", err)
	}
	// A cancelled context stops the scan.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := FindRun(cctx, []string{f.root}, "cli-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	// Missing roots are no runs.
	if _, err := FindRun(ctx, []string{filepath.Join(f.root, "nope"), ""}, "cli-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing root: %v", err)
	}
}

func TestFindTask(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	other := filepath.Join(f.root, "acct-2", "org-2")
	f.tasks(f.org,
		map[string]any{"id": "follow-up", "displayName": "Follow up Marco", "cronExpression": "0 9 * * 1-5", "enabled": true,
			"permissionMode": "default", "model": "x", "place": map[string]any{"k": 1}},
		map[string]any{"id": "weekly", "displayName": "Twin"},
	)
	f.tasks(other,
		map[string]any{"id": "remind", "displayName": "twin"},
		map[string]any{"id": "follow-up-2", "displayName": "weekly"},
	)
	roots := []string{f.root}
	for ref, want := range map[string]string{
		"follow-up": "follow-up", "  follow-up ": "follow-up", "follow up marco": "follow-up", "FOLLOW UP MARCO": "follow-up",
		"remind": "remind", "weekly": "weekly", // an id wins over another task's display name
	} {
		got, err := FindTask(ctx, roots, ref)
		if err != nil || got.ID != want {
			t.Errorf("FindTask(%q) = %q %v, want %q", ref, got.ID, err, want)
		}
	}
	got, _ := FindTask(ctx, roots, "remind")
	if got.Dir != other {
		t.Errorf("dir = %s", got.Dir)
	}
	if _, err := FindTask(ctx, roots, "twin"); err == nil || !strings.Contains(err.Error(), "names 2 tasks: use the id") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := FindTask(ctx, roots, "Follow"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := FindTask(ctx, roots, " "); err == nil {
		t.Error("empty ref accepted")
	}
	if got, err := FindTaskByID(ctx, roots, "follow-up"); err != nil || got.Title() != "Follow up Marco" || got.Schedule() != "cron 0 9 * * 1-5" {
		t.Errorf("by id: %+v %v", got, err)
	}
	if _, err := FindTaskByID(ctx, roots, "Follow up Marco"); !errors.Is(err, ErrNotFound) {
		t.Errorf("by id matched a name: %v", err)
	}
	if got, err := TaskByID(other, "remind"); err != nil || got.Title() != "twin" {
		t.Errorf("TaskByID: %+v %v", got, err)
	}
	if _, err := TaskByID(other, "follow-up"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TaskByID in another folder: %v", err)
	}
	f.write(filepath.Join(other, TasksFile), `{"scheduledTasks": [`, time.Time{})
	if _, err := FindTask(ctx, roots, "follow-up"); err == nil {
		t.Error("corrupt task list ignored")
	}
}

func TestTaskTitleAndSchedule(t *testing.T) {
	for _, tc := range []struct {
		task         Task
		title, sched string
	}{
		{Task{ID: "a", DisplayName: "Morning", CronHuman: "Every day at 9:00", Cron: "0 9 * * *"}, "Morning", "Every day at 9:00"},
		{Task{ID: "a", Cron: "0 9 * * *"}, "a", "cron 0 9 * * *"},
		{Task{ID: "a", FireAt: "2026-12-01T09:00:00Z"}, "a", "once at 2026-12-01T09:00:00Z"},
		{Task{ID: "a"}, "a", "manual only"},
	} {
		if tc.task.Title() != tc.title || tc.task.Schedule() != tc.sched {
			t.Errorf("%+v: %q %q", tc.task, tc.task.Title(), tc.task.Schedule())
		}
	}
}

func TestSkillAndFingerprint(t *testing.T) {
	f := newFixture(t)
	path := f.skill("follow-up", "Write to c_5: are we still on?\n")
	base := Task{ID: "follow-up", DisplayName: "Follow up", FilePath: path, Cron: "0 9 * * 1-5", PermissionMode: "default", Cwd: `C:\x`}
	_, body, err := Skill(base)
	if err != nil || body != "Write to c_5: are we still on?" {
		t.Fatalf("body %q %v", body, err)
	}
	fp, err := Fingerprint(base)
	if err != nil || len(fp) != 64 {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
	same := func(name string, task Task, want bool) {
		t.Helper()
		got, err := Fingerprint(task)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if (got == fp) != want {
			t.Errorf("%s: fingerprint unchanged=%v, want %v", name, got == fp, want)
		}
	}
	x := base
	x.DisplayName, x.Cwd, x.Dir, x.CronHuman = "Other", `D:\y`, "elsewhere", "Weekdays at 9"
	same("display name, cwd, dir, human schedule", x, true)
	x = base
	x.Cron = "0 10 * * 1-5"
	same("cron", x, false)
	x = base
	x.FireAt = "2026-12-01T09:00:00Z"
	same("fireAt", x, false)
	x = base
	x.PermissionMode = "acceptEdits"
	same("mode", x, false)
	f.write(path, "---\nname: follow-up\ndescription: a test task\n---\nWrite to c_6: are we still on?\n", time.Time{})
	same("prompt", base, false)
	f.write(path, "---\nname: follow-up\ndescription: changed\n---\nWrite to c_5: are we still on?\n", time.Time{})
	same("front matter", base, false)

	// Bookkeeping fields Desktop changes do not count.
	var a, b Task
	json.Unmarshal([]byte(`{"id":"t","filePath":`+quoteJSON(path)+`,"cronExpression":"0 9 * * *","createdAt":1}`), &a)
	json.Unmarshal([]byte(`{"id":"t","filePath":`+quoteJSON(path)+`,"cronExpression":"0 9 * * *","createdAt":2,"changedByClaudeAt":3,"enabled":false}`), &b)
	fa, _ := Fingerprint(a)
	fb, _ := Fingerprint(b)
	if fa != fb || fa == "" {
		t.Errorf("bookkeeping changed the fingerprint")
	}

	// Fail closed: missing, unreadable, oversized or empty SKILL.md.
	for name, task := range map[string]Task{
		"missing": {ID: "x", FilePath: filepath.Join(f.root, "nope", "SKILL.md")},
		"folder":  {ID: "x", FilePath: f.root},
	} {
		if _, err := Fingerprint(task); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	empty := f.skill("empty", "\n  \n")
	if _, err := Fingerprint(Task{ID: "empty", FilePath: empty}); err == nil || !strings.Contains(err.Error(), "no prompt") {
		t.Errorf("empty body: %v", err)
	}
	open := filepath.Join(f.root, "open.md")
	f.write(open, "---\nname: x\nWrite to c_5", time.Time{})
	if _, err := Fingerprint(Task{ID: "x", FilePath: open}); err == nil {
		t.Error("unclosed front matter accepted")
	}
	big := filepath.Join(f.root, "big.md")
	f.write(big, strings.Repeat("x", maxSkill+1), time.Time{})
	if _, err := Fingerprint(Task{ID: "x", FilePath: big}); err == nil || !strings.Contains(err.Error(), "over 1 MB") {
		t.Errorf("oversize: %v", err)
	}
	// No front matter: the whole file is the prompt.
	plain := filepath.Join(f.root, "plain.md")
	f.write(plain, "Just do it", time.Time{})
	if _, body, err := Skill(Task{ID: "x", FilePath: plain}); err != nil || body != "Just do it" {
		t.Errorf("plain: %q %v", body, err)
	}
}

// Without a filePath the SKILL.md is the default one in Claude Code's config folder.
func TestSkillDefaultPath(t *testing.T) {
	f := newFixture(t)
	cfg := filepath.Join(f.root, "..", "config")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	f.skill("follow-up", "Write to c_5")
	if _, body, err := Skill(Task{ID: "follow-up"}); err != nil || body != "Write to c_5" {
		t.Fatalf("default path: %q %v", body, err)
	}
	if _, _, err := Skill(Task{ID: "../follow-up"}); err == nil {
		t.Fatal("id with a path accepted")
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Read-only parse of the real Desktop folders, printing ids, names and
// schedules only:
//
//	WHATSAPP_MCP_E2E_DESKTOP=1 go test ./internal/desktop -run TestRealDesktop -v
func TestRealDesktop(t *testing.T) {
	if os.Getenv("WHATSAPP_MCP_E2E_DESKTOP") != "1" {
		t.Skip("set WHATSAPP_MCP_E2E_DESKTOP=1 to read the real Claude Desktop folders")
	}
	roots := Roots()
	t.Logf("roots: %v", roots)
	ctx := context.Background()
	for _, p := range matches(roots, TasksFile) {
		ts, err := LoadTasks(filepath.Dir(p))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		for _, task := range ts {
			_, ferr := Fingerprint(task)
			t.Logf("task id=%s title=%q schedule=%q mode=%q skill=%v", task.ID, task.Title(), task.Schedule(), task.PermissionMode, ferr == nil)
		}
	}
	runs := matches(roots, "local_*.json")
	t.Logf("%d session files", len(runs))
	start := time.Now()
	_, err := FindRun(ctx, roots, "no-such-session-id")
	t.Logf("FindRun over all files: %s (%v)", time.Since(start), err)
	n := 0
	for _, p := range runs {
		b, tooBig, err := readCapped(p, maxFile)
		var r Run
		if err != nil || tooBig || json.Unmarshal(b, &r) != nil || r.TaskID == "" {
			continue
		}
		if n++; n <= 5 {
			start := time.Now()
			got, err := FindRun(ctx, roots, r.CLISessionID)
			t.Logf("scheduled run task=%s continued=%v: FindRun %s (%v, task %s)", r.TaskID, r.Continued, time.Since(start), err, got.TaskID)
		}
	}
}
