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
	if err != nil || r.TaskID != "follow-up" || r.SessionID != "local_a" || r.Dir != f.org {
		t.Fatalf("match: %+v %v", r, err)
	}
	if r, err := FindRun(ctx, []string{f.root}, "cli-2"); err != nil || r.TaskID != "" {
		t.Fatalf("no task id: %+v %v", r, err)
	}
	if r, err := FindRun(ctx, []string{f.root}, "cli-3"); err != nil || r.SessionID != "local_c" {
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
	// A file that is not a task list blocks only lookups that find nothing elsewhere.
	f.write(filepath.Join(other, TasksFile), `{"scheduledTasks": [`, time.Time{})
	if got, err := FindTask(ctx, roots, "follow-up"); err != nil || got.ID != "follow-up" {
		t.Errorf("unique match beside a broken file: %+v %v", got, err)
	}
	if _, err := FindTask(ctx, roots, "remind"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("no match beside a broken file: %v", err)
	}
	if _, err := TaskByID(other, "remind"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("TaskByID in a broken file: %v", err)
	}
}

// Each task decodes on its own: a record that does not decode hides only
// the lookups it might answer.
func TestUndecodableTasks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.write(filepath.Join(f.org, TasksFile), `{"scheduledTasks": [
		{"id": "good", "displayName": "Good", "enabled": true, "filePath": "x", "createdAt": 1},
		{"id": "bad-enabled", "displayName": "Bad", "enabled": "yes", "filePath": "x", "createdAt": 1},
		{"id": 7, "displayName": "Numbered", "enabled": true},
		{"id": "odd-name", "displayName": 12, "enabled": true, "cronExpression": 5},
		"not an object"
	], "recordedSkips": {}}`, time.Time{})
	roots := []string{f.root}
	l, err := AllTasks(ctx, roots)
	if err != nil || len(l.Tasks) != 1 || len(l.Unreadable) != 4 || len(l.Broken) != 0 {
		t.Fatalf("list: %+v %v", l, err)
	}
	for ref, ok := range map[string]bool{
		"good": false, // an id: the numbered record and the non-object one might have it
		"Good": false, // a title: odd-name's and the non-object record's titles are unknown
	} {
		if _, err := FindTask(ctx, roots, ref); (err == nil) != ok {
			t.Errorf("FindTask(%q): %v", ref, err)
		}
	}
	// Without the records whose id or title is unknown, the good task is found.
	f.write(filepath.Join(f.org, TasksFile), `{"scheduledTasks": [
		{"id": "good", "displayName": "Good", "enabled": true, "filePath": "x", "createdAt": 1},
		{"id": "bad-enabled", "displayName": "Bad", "enabled": "yes", "filePath": "x", "createdAt": 1},
		{"id": "no-title", "enabled": "yes"}
	]}`, time.Time{})
	for ref, want := range map[string]string{"good": "good", "GOOD": "good"} {
		if got, err := FindTask(ctx, roots, ref); err != nil || got.ID != want {
			t.Errorf("FindTask(%q) = %+v %v", ref, got, err)
		}
	}
	for _, ref := range []string{"bad-enabled", "Bad"} {
		if _, err := FindTask(ctx, roots, ref); err == nil || !strings.Contains(err.Error(), "cannot be read might be") {
			t.Errorf("FindTask(%q): %v", ref, err)
		}
	}
	if _, err := FindTask(ctx, roots, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := TaskByID(f.org, "bad-enabled"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("TaskByID of an unreadable task: %v", err)
	}
}

// realRecord is the shape Claude Desktop writes, from a real one-shot task
// after it ran (ids and paths replaced).
const realRecord = `{
 "scheduledTasks": [
  {
   "id": "wamcp-test",
   "displayName": "wamcp-test",
   "fireAt": 1795551540000,
   "enabled": false,
   "filePath": "SKILLPATH",
   "createdAt": 1795551137428,
   "cwd": "C:\\Users\\me\\AppData\\Roaming\\Claude\\scratch-workspaces\\acct\\org\\scratch-2026-11-24-3a3b81",
   "permissionMode": "auto",
   "permissionModeCarried": true,
   "madeByClaude": true,
   "notifySessionId": "local_00000000-0000-0000-0000-000000000000",
   "promptWrittenBy": ["unknown"],
   "promptFileHash": "679f80a6f9b74924440c4a86e254aaed.10a006c3db13e2ba734e6f1218b75b110c17e2d6a05b8ed41621b7577afdf26c",
   "lastRunAt": "2026-11-24T20:19:31.515Z",
   "lastScheduledFor": "2026-11-24T20:19:00.000Z",
   "changedByClaudeAt": 1795551143679
  }
 ],
 "recordedSkips": {},
 "sundayAliasBoundaryStamped": true,
 "dayFieldsOrBoundaryStamped": true
}`

func TestRealRecordShape(t *testing.T) {
	f := newFixture(t)
	skill := f.skill("wamcp-test", "Send c_5 a test message.\n")
	b, _ := json.Marshal(skill)
	f.write(filepath.Join(f.org, TasksFile), strings.Replace(realRecord, `"SKILLPATH"`, string(b), 1), time.Time{})
	got, err := FindTask(context.Background(), []string{f.root}, "wamcp-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "wamcp-test" || got.Title() != "wamcp-test" || got.FireAt.Canonical() != "ms:1795551540000" || got.Enabled == nil || *got.Enabled ||
		got.PermissionMode != "auto" || got.FilePath != skill || !strings.HasSuffix(got.Cwd, `scratch-2026-11-24-3a3b81`) || got.Cron != "" {
		t.Fatalf("task %+v", got)
	}
	if s := got.Schedule(); s != "once at 24 Nov 2026 21:19" {
		t.Fatalf("schedule %q", s)
	}
	if _, err := Fingerprint(got); err != nil {
		t.Fatal(err)
	}
}

func TestFireAt(t *testing.T) {
	for _, tc := range []struct {
		json, canonical, shown string
		err                    bool
	}{
		{`1795551540000`, "ms:1795551540000", "24 Nov 2026 21:19", false},
		{`1.79555154e12`, "ms:1795551540000", "24 Nov 2026 21:19", false},
		{`"2026-11-24T20:19:00Z"`, "ms:1795551540000", "24 Nov 2026 21:19", false},
		{`"2026-11-24T21:19:00+01:00"`, "ms:1795551540000", "24 Nov 2026 21:19", false},
		{`"tomorrow"`, "raw:tomorrow", "tomorrow", false},
		{`"1795551540000"`, "raw:1795551540000", "1795551540000", false}, // never the number's fingerprint
		{`null`, "", "", false},
		{`""`, "", "", true},
		{`1.5`, "", "", true},
		{`true`, "", "", true},
	} {
		var x struct {
			F FireAt `json:"fireAt"`
		}
		err := json.Unmarshal([]byte(`{"fireAt":`+tc.json+`}`), &x)
		if (err != nil) != tc.err || err == nil && (x.F.Canonical() != tc.canonical || x.F.String() != tc.shown) {
			t.Errorf("%s: %q %q %v", tc.json, x.F.Canonical(), x.F.String(), err)
		}
	}
	// Summer time in Rome.
	if s := FireAtMs(time.Date(2026, 7, 1, 7, 0, 0, 0, time.UTC).UnixMilli()).String(); s != "1 Jul 2026 09:00" {
		t.Errorf("summer: %q", s)
	}
}

func TestTaskTitleAndSchedule(t *testing.T) {
	for _, tc := range []struct {
		task         Task
		title, sched string
	}{
		{Task{ID: "a", DisplayName: "Morning", CronHuman: "Every day at 9:00", Cron: "0 9 * * *"}, "Morning", "cron 0 9 * * * (Every day at 9:00)"},
		{Task{ID: "a", CronHuman: "Every day at 9:00"}, "a", "manual only"},
		{Task{ID: "a", Cron: "0 9 * * *"}, "a", "cron 0 9 * * *"},
		{Task{ID: "a", FireAt: FireAtMs(1795551540000)}, "a", "once at 24 Nov 2026 21:19"},
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
	file, sk, err := ReadSkill(base)
	if err != nil || sk.Prompt != "Write to c_5: are we still on?" || len(sk.Extra) != 0 {
		t.Fatalf("skill %+v %v", sk, err)
	}
	fp, err := Fingerprint(base)
	if err != nil || len(fp) != 64 {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
	if of, err := FingerprintOf(base, file); err != nil || of != fp {
		t.Fatalf("FingerprintOf the same bytes: %q %v", of, err)
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
	x.DisplayName, x.Dir, x.CronHuman, x.PermissionMode = "Other", "elsewhere", "Weekdays at 9", "acceptEdits"
	same("display name, dir, human schedule, permission mode", x, true)
	for name, edit := range map[string]func(*Task){
		"cron":   func(t *Task) { t.Cron = "0 10 * * 1-5" },
		"fireAt": func(t *Task) { t.FireAt = FireAtMs(1795551540000) },
		"cwd":    func(t *Task) { t.Cwd = `D:\y` },
	} {
		x := base
		edit(&x)
		same(name, x, false)
	}
	other := f.skill("other", "Write to c_5: are we still on?\n")
	x = base
	x.FilePath = other
	same("same file at another path", x, false)
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

	// Fail closed: missing, unreadable, oversized SKILL.md, a relative filePath.
	big := filepath.Join(f.root, "big.md")
	f.write(big, strings.Repeat("x", maxSkill+1), time.Time{})
	for name, task := range map[string]Task{
		"missing":  {ID: "x", FilePath: filepath.Join(f.root, "nope", "SKILL.md")},
		"folder":   {ID: "x", FilePath: f.root},
		"relative": {ID: "x", FilePath: filepath.Join("scheduled-tasks", "x", "SKILL.md")},
		"spaced":   {ID: "x", FilePath: " " + path},
		"oversize": {ID: "x", FilePath: big},
	} {
		if _, err := Fingerprint(task); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// ParseSkill splits a SKILL.md like Claude Desktop's parser, and refuses
// what Desktop would not run or might read differently.
func TestParseSkill(t *testing.T) {
	for _, tc := range []struct {
		name, file, prompt string
		extra              []string
		err                string
	}{
		{"desktop's own format", "---\nname: follow-up\ndescription: Follow up\n---\n\nAsk c_5.\n", "Ask c_5.", nil, ""},
		{"CRLF", "---\r\nname: x\r\ndescription: d\r\n---\r\nAsk c_5.\r\n\r\n", "Ask c_5.", nil, ""},
		{"model and extra keys", "---\nname: x\ndescription: d\nmodel: opus\nallowed-tools: Bash\nhooks: x\n---\nAsk", "Ask", []string{"allowed-tools", "hooks"}, ""},
		{"empty description before a key", "---\nname: x\ndescription:\nmodel: y\n---\nAsk", "Ask", nil, ""},
		{"empty description last", "---\nname: x\ndescription: \t\n---\nAsk", "Ask", nil, ""},
		{"body keeps leading spaces and inner ---", "---\nname: x\ndescription: d\n---\n  Ask\n---\nmore  \n", "  Ask\n---\nmore", nil, ""},
		{"trailing js whitespace", "---\nname: x\ndescription: d\n---\nAsk\u00a0\ufeff\u2028 ", "Ask", nil, ""},
		{"no front matter", "Just do it", "", nil, "no front matter"},
		{"BOM", "\ufeff---\nname: x\ndescription: d\n---\nAsk", "", nil, "no front matter"},
		{"opening with spaces", "--- \nname: x\ndescription: d\n---\nAsk", "", nil, "no front matter"},
		{"unclosed", "---\nname: x\ndescription: d\nAsk", "", nil, "no front matter"},
		{"closing with spaces then real", "---\nname: x\ndescription: d\n--- \ny: z\n---\nAsk", "", nil, "--- line with spaces"},
		{"no name", "---\ndescription: d\n---\nAsk", "", nil, "no name"},
		{"name without space", "---\nname:x\ndescription: d\n---\nAsk", "", nil, "no name"},
		{"no description", "---\nname: x\n---\nAsk", "", nil, "no description"},
		{"empty description before a non-key", "---\nname: x\ndescription:\n  indented\n---\nAsk", "", nil, "no description"},
		{"lone CR", "---\nname: x\rdescription: d\n---\nAsk", "", nil, "line break"},
		{"line separator", "---\nname: x\u2028description: d\n---\nAsk", "", nil, "line break"},
		{"empty body", "---\nname: x\ndescription: d\n---\n \n\t\n", "", nil, "no prompt"},
		{"not UTF-8", "---\nname: x\ndescription: d\n---\nAsk \xff", "", nil, "not UTF-8"},
	} {
		got, err := ParseSkill([]byte(tc.file))
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: %+v %v, want error %q", tc.name, got, err, tc.err)
			}
			continue
		}
		if err != nil || got.Prompt != tc.prompt || strings.Join(got.Extra, ",") != strings.Join(tc.extra, ",") {
			t.Errorf("%s: %+v %v, want %q %v", tc.name, got, err, tc.prompt, tc.extra)
		}
	}
}

// A task without a filePath is refused, even with a SKILL.md where Claude
// Code's config folder would hold one: the gate and the daemon may see
// different config folders.
func TestSkillNeedsFilePath(t *testing.T) {
	f := newFixture(t)
	cfg := filepath.Join(f.root, "..", "config")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	f.skill("follow-up", "Write to c_5")
	for _, p := range []string{"", "  "} {
		if _, _, err := ReadSkill(Task{ID: "follow-up", FilePath: p}); err == nil || !strings.Contains(err.Error(), "no filePath") {
			t.Fatalf("filePath %q: %v", p, err)
		}
		if _, err := FingerprintOf(Task{ID: "follow-up", FilePath: p}, nil); err == nil {
			t.Fatalf("fingerprint with filePath %q", p)
		}
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
		l := LoadTasks(filepath.Dir(p))
		for _, err := range l.Broken {
			t.Errorf("%s: %v", p, err)
		}
		for _, u := range l.Unreadable {
			t.Errorf("%s: a task does not decode: %v", p, u.Err)
		}
		for _, task := range l.Tasks {
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
			t.Logf("scheduled run task=%s: FindRun %s (%v, task %s)", r.TaskID, time.Since(start), err, got.TaskID)
		}
	}
}

// Records that spell a field the gate reads in another case, or hold it
// twice, do not decode: Desktop's JavaScript would read another value.
func TestExactKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	roots := []string{f.root}
	f.write(filepath.Join(f.org, TasksFile), `{"scheduledTasks": [
		{"id": "good", "displayName": "Good", "enabled": true, "filePath": "x", "createdAt": 1, "unknownField": 1},
		{"id": "cased", "displayName": "Cased", "enabled": true, "CronExpression": "* * * * *"},
		{"id": "twice", "displayName": "Twice", "enabled": true, "cwd": "a", "cwd": "b"}
	]}`, time.Time{})
	if got, err := FindTask(ctx, roots, "good"); err != nil || got.ID != "good" {
		t.Fatalf("good: %+v %v", got, err)
	}
	for _, ref := range []string{"cased", "twice", "Cased", "Twice"} {
		if _, err := FindTask(ctx, roots, ref); err == nil || !strings.Contains(err.Error(), "cannot be read") {
			t.Errorf("%s: %v", ref, err)
		}
	}
	for name, top := range map[string]string{
		"cased top": `{"ScheduledTasks": [{"id": "good", "enabled": true, "filePath": "x"}]}`,
		"top twice": `{"scheduledTasks": [], "scheduledTasks": [{"id": "good", "enabled": true, "filePath": "x"}]}`,
	} {
		f.write(filepath.Join(f.org, TasksFile), top, time.Time{})
		if l := LoadTasks(f.org); len(l.Broken) != 1 || len(l.Tasks) != 0 {
			t.Errorf("%s: %+v", name, l)
		}
	}
	// Run files: a matching file with a cased or doubled field is an error.
	f.run("cased", map[string]any{"cliSessionId": "cli-1", "ScheduledTaskId": "x"}, time.Now())
	if _, err := FindRun(ctx, roots, "cli-1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("cased run field: %v", err)
	}
	f.write(filepath.Join(f.org, "local_twice.json"), `{"cliSessionId": "cli-2", "scheduledTaskId": "a", "scheduledTaskId": "follow-up"}`, time.Time{})
	if _, err := FindRun(ctx, roots, "cli-2"); err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("doubled run field: %v", err)
	}
}

// An unreadable record's id and display name are read each on its own, and
// null counts as absent.
func TestUnreadableNullFields(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		id         string // "" for unknown
		titleKnown bool
		title      string
	}{
		{`{"id": "a", "displayName": "A", "enabled": "x"}`, "a", true, "A"},
		{`{"id": null, "displayName": null, "enabled": "x"}`, "", true, ""},
		{`{"id": "a", "displayName": 3, "enabled": "x"}`, "a", false, ""},
		{`{"id": 3, "displayName": "A", "enabled": "x"}`, "", true, "A"},
	} {
		u := unreadable("d", json.RawMessage(tc.raw), errors.New("x"))
		id := ""
		if u.ID != nil {
			id = *u.ID
		}
		title := ""
		if u.DisplayName != nil {
			title = *u.DisplayName
		}
		if id != tc.id || u.TitleKnown != tc.titleKnown || title != tc.title {
			t.Errorf("%s: id %q title %q known %v", tc.raw, id, title, u.TitleKnown)
		}
	}
}

// A task without a mode runs in the default mode its settings give it:
// local over project over user, "default" when none sets it, and an error
// for any settings file that exists but cannot be read.
func TestEffectiveMode(t *testing.T) {
	base := t.TempDir()
	user := filepath.Join(base, "claude-config")
	cwd := filepath.Join(base, "work")
	local, project, userFile := filepath.Join(cwd, ".claude", "settings.local.json"), filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(user, "settings.json")
	write := func(file, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mode := func(m string) string { return `{"permissions": {"allow": [], "defaultMode": "` + m + `"}}` }
	task := Task{ID: "t", Cwd: cwd}
	check := func(name, want, source string) {
		t.Helper()
		got, err := EffectiveMode(task, user)
		if err != nil || got.Name != want || !got.FromSettings || got.Source != source {
			t.Errorf("%s: %+v %v, want %s from %q", name, got, err, want, source)
		}
	}
	check("absent everywhere", "default", "")
	write(userFile, `{"model": "x"}`)
	check("user file without the key", "default", "")
	write(userFile, mode("bypassPermissions"))
	check("user", "bypassPermissions", userFile)
	write(project, mode("plan"))
	check("project over user", "plan", project)
	write(local, mode("acceptEdits"))
	check("local over project", "acceptEdits", local)
	write(local, `{"permissions": {"DefaultMode": "auto", "defaultMode": null}, "Permissions": {"defaultMode": "auto"}}`)
	check("case variants and null ignored", "plan", project)
	write(local, `{"permissions": {"defaultMode": ""}}`)
	check("empty string ignored", "plan", project)

	// The task's own mode wins without reading anything.
	if got, err := EffectiveMode(Task{PermissionMode: "auto"}, ""); err != nil || got != (Mode{Name: "auto"}) {
		t.Errorf("task mode: %+v %v", got, err)
	}

	// Any settings file that exists but cannot be read refuses, even below one that sets the mode.
	for name, setup := range map[string]func(){
		"broken user file":    func() { write(userFile, `{"permissions": `) },
		"non-string mode":     func() { write(project, `{"permissions": {"defaultMode": 3}}`) },
		"permissions not obj": func() { write(project, `{"permissions": []}`) },
		"empty local file":    func() { write(local, ``) },
		"folder in its place": func() { os.MkdirAll(filepath.Join(user, "settings.json"), 0o700) },
	} {
		os.RemoveAll(base)
		write(local, mode("default"))
		setup()
		if got, err := EffectiveMode(task, user); err == nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
	os.RemoveAll(base)
	for name, tc := range map[string]struct {
		task Task
		user string
	}{
		"no folder":       {Task{ID: "t"}, user},
		"relative folder": {Task{ID: "t", Cwd: "work"}, user},
		"no config dir":   {task, ""},
	} {
		if got, err := EffectiveMode(tc.task, tc.user); err == nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
}
