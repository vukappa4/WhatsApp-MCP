// Package desktop reads the files Claude Desktop keeps for its local Claude
// Code sessions and scheduled tasks. It only reads, and only what the send
// gate needs: which scheduled task a session runs, and what that task is.
//
// Layout, under each root:
//
//	<root>/<accountUuid>/<orgUuid>/scheduled-tasks.json   {"scheduledTasks": [Task...]}
//	<root>/<accountUuid>/<orgUuid>/local_<id>.json        one Run per session
//	<filePath>, by default <ConfigDir>/scheduled-tasks/<id>/SKILL.md: the prompt
package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Europe/Rome without relying on the OS zone database
	"unicode"
	"unicode/utf8"
)

// maxFile caps every Desktop JSON file read.
const maxFile = 4 << 20

// maxSkill caps a task's SKILL.md.
const maxSkill = 1 << 20

// TasksFile is the scheduled task list in each account/org folder.
const TasksFile = "scheduled-tasks.json"

// ErrNotFound means no file names the session or task.
var ErrNotFound = errors.New("not found")

// Roots are the folders holding Claude Desktop's local Claude Code
// sessions: %APPDATA%\Claude\claude-code-sessions on Windows and
// ~/Library/Application Support/Claude/claude-code-sessions on macOS. Other
// systems have no Claude Desktop scheduled tasks: nil.
func Roots() []string {
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("APPDATA"); d != "" {
			return []string{filepath.Join(d, "Claude", "claude-code-sessions")}
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return []string{filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")}
		}
	}
	return nil
}

// Task is one Desktop scheduled task, as Desktop's own record check types
// it: id, filePath and the optional displayName, cronExpression, cwd and
// permissionMode are strings, fireAt is a number (epoch ms), enabled a
// boolean. Its prompt is not in the record: it is the body of the task's
// SKILL.md (see ReadSkill), which Desktop reads again at every run.
type Task struct {
	ID             string    `json:"id"` // kebab-case: ^[a-z0-9_-]+$
	DisplayName    string    `json:"displayName"`
	FilePath       string    `json:"filePath"` // its SKILL.md
	Cron           string    `json:"cronExpression"`
	CronHuman      looseText `json:"cronHumanReadable"` // not in Desktop's record check: read only when it is a string
	FireAt         FireAt    `json:"fireAt"`            // a one-shot task's time
	Enabled        *bool     `json:"enabled"`
	Cwd            string    `json:"cwd"`
	PermissionMode string    `json:"permissionMode"`
	Dir            string    `json:"-"` // the account/org folder it was read from
}

// looseText is a string field that is empty when it holds anything else.
type looseText string

func (l *looseText) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = looseText(s)
	}
	return nil
}

// FireAt is a one-shot task's time. Desktop writes it as epoch
// milliseconds; a string is read too, as RFC 3339 when it parses.
type FireAt struct {
	ms  int64  // epoch ms, when known
	raw string // a string that is not RFC 3339
	set bool
}

// FireAtMs is a FireAt of epoch milliseconds.
func FireAtMs(ms int64) FireAt { return FireAt{ms: ms, set: true} }

// UnmarshalJSON reads a number of milliseconds, a string or null.
func (f *FireAt) UnmarshalJSON(b []byte) error {
	*f = FireAt{}
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s == "" {
			return errors.New("fireAt is an empty string")
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			*f = FireAt{ms: t.UnixMilli(), set: true}
		} else {
			*f = FireAt{raw: s, set: true}
		}
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("fireAt is neither a number nor a string")
	}
	if ms, err := n.Int64(); err == nil {
		*f = FireAt{ms: ms, set: true}
		return nil
	}
	x, err := n.Float64()
	if err != nil || x != float64(int64(x)) {
		return fmt.Errorf("fireAt %s is not a whole number of milliseconds", n)
	}
	*f = FireAt{ms: int64(x), set: true}
	return nil
}

// Set reports whether the task has a fireAt.
func (f FireAt) Set() bool { return f.set }

// Canonical is the fireAt the fingerprint holds: "ms:<epoch ms>" for a
// number or an RFC 3339 string, "raw:<string>" for any other string, ""
// when unset. The prefix keeps a number and a string from ever sharing it.
func (f FireAt) Canonical() string {
	switch {
	case !f.set:
		return ""
	case f.raw != "":
		return "raw:" + f.raw
	}
	return "ms:" + strconv.FormatInt(f.ms, 10)
}

// rome is where the user lives: schedules are shown in its time.
var rome = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// String renders the time in Europe/Rome, "24 Nov 2026 21:19".
func (f FireAt) String() string {
	if f.raw != "" || !f.set {
		return f.raw
	}
	return time.UnixMilli(f.ms).In(rome).Format("2 Jan 2006 15:04")
}

// Title is the name the user sees: the display name, else the id.
func (t Task) Title() string {
	if s := strings.TrimSpace(t.DisplayName); s != "" {
		return s
	}
	return t.ID
}

// Schedule describes when the task runs: "cron <expression>", with the
// human-readable form in parentheses when the record has one, else "once at
// <fireAt>", else "manual only". The expression is what Desktop runs: a
// description written beside it is never shown alone.
func (t Task) Schedule() string {
	cron := strings.TrimSpace(t.Cron)
	switch {
	case cron != "" && strings.TrimSpace(string(t.CronHuman)) != "":
		return "cron " + cron + " (" + strings.TrimSpace(string(t.CronHuman)) + ")"
	case cron != "":
		return "cron " + cron
	case t.FireAt.Set():
		return "once at " + t.FireAt.String()
	}
	return "manual only"
}

// Run is one local Claude Code session Desktop started. A scheduled run
// names its task. The gate never reads Desktop's scheduledRunContinued: it
// lags a person's typing by seconds, and "Run now" sets it with nobody
// typing. Whether a person is present is the UserPromptSubmit hook's call.
type Run struct {
	SessionID      string `json:"sessionId"`
	CLISessionID   string `json:"cliSessionId"`
	TaskID         string `json:"scheduledTaskId"`
	PermissionMode string `json:"permissionMode"`
	Dir            string `json:"-"` // the account/org folder it was read from
}

// readCapped reads at most limit bytes of path; tooBig is set when the
// file holds more, and b then holds the first limit+1 bytes.
func readCapped(path string, limit int64) (b []byte, tooBig bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err = io.ReadAll(io.LimitReader(f, limit+1))
	return b, int64(len(b)) > limit, err
}

// matches are the files called name in the account/org folders under roots.
func matches(roots []string, name string) []string {
	var out []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		m, _ := filepath.Glob(filepath.Join(r, "*", "*", name))
		out = append(out, m...)
	}
	return out
}

// FindRun finds the run whose cliSessionId is id, the session id Claude
// Code gives its hooks. It reads the newest files first, skips files over
// 4 MB that do not hold the id in their first 4 MB and files that do not
// hold it at all, and checks ctx between files. Exactly one run must match:
// none is ErrNotFound, two or more, or a matching file it cannot read, is an
// error.
func FindRun(ctx context.Context, roots []string, id string) (Run, error) {
	if strings.TrimSpace(id) == "" {
		return Run{}, errors.New("no session id")
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, p := range matches(roots, "local_*.json") {
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		files = append(files, file{p, st.ModTime()})
	}
	slices.SortFunc(files, func(a, b file) int { return b.mod.Compare(a.mod) })
	needle, _ := json.Marshal(id) // the id as JSON writes it, quotes included
	var found []Run
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return Run{}, err
		}
		b, tooBig, err := readCapped(f.path, maxFile)
		if err != nil || !bytes.Contains(b, needle) {
			continue
		}
		if tooBig {
			return Run{}, fmt.Errorf("%s is over %d MB", filepath.Base(f.path), maxFile>>20)
		}
		var r Run
		if err := exactKeys(b, runKeys); err != nil {
			return Run{}, fmt.Errorf("read %s: %w", filepath.Base(f.path), err)
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return Run{}, fmt.Errorf("read %s: %w", filepath.Base(f.path), err)
		}
		if r.CLISessionID != id {
			continue
		}
		r.Dir = filepath.Dir(f.path)
		found = append(found, r)
	}
	switch len(found) {
	case 0:
		return Run{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	case 1:
		return found[0], nil
	}
	return Run{}, fmt.Errorf("session %s is named by %d Desktop session files", id, len(found))
}

// Unreadable is a task record that does not decode as a Task. Its id and
// display name are kept when they are strings, so a lookup knows whether
// it might be the task wanted.
type Unreadable struct {
	Dir         string
	ID          *string
	DisplayName *string // nil also when the record has none
	TitleKnown  bool    // the record has no displayName, or a string one
	Err         error
}

// List is what the task files under some roots hold: the tasks that
// decode, the records that do not, and the files that cannot be read at all.
type List struct {
	Tasks      []Task
	Unreadable []Unreadable
	Broken     []error // whole files that cannot be read
}

// LoadTasks reads dir/scheduled-tasks.json, decoding each task on its own:
// a record that does not decode is kept in Unreadable. A missing file is no
// tasks; a file that is not a task list is in Broken.
func LoadTasks(dir string) List {
	path := filepath.Join(dir, TasksFile)
	b, tooBig, err := readCapped(path, maxFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return List{}
	case err != nil:
		return List{Broken: []error{err}}
	case tooBig:
		return List{Broken: []error{fmt.Errorf("%s is over %d MB", path, maxFile>>20)}}
	}
	var f struct {
		ScheduledTasks []json.RawMessage `json:"scheduledTasks"`
	}
	if err := exactKeys(b, []string{"scheduledTasks"}); err != nil {
		return List{Broken: []error{fmt.Errorf("read %s: %w", path, err)}}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return List{Broken: []error{fmt.Errorf("read %s: %w", path, err)}}
	}
	var l List
	for _, raw := range f.ScheduledTasks {
		var t Task
		err := exactKeys(raw, taskKeys)
		if err == nil {
			err = json.Unmarshal(raw, &t)
		}
		if err != nil {
			l.Unreadable = append(l.Unreadable, unreadable(dir, raw, err))
			continue
		}
		t.Dir = dir
		l.Tasks = append(l.Tasks, t)
	}
	return l
}

func unreadable(dir string, raw json.RawMessage, err error) Unreadable {
	u := Unreadable{Dir: dir, Err: err}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return u
	}
	present := func(k string) (json.RawMessage, bool) {
		v, ok := m[k]
		if !ok || string(bytes.TrimSpace(v)) == "null" {
			return nil, false // null is absent, as in Desktop's record check
		}
		return v, true
	}
	if v, ok := present("id"); ok {
		var id string
		if json.Unmarshal(v, &id) == nil {
			u.ID = &id
		}
	}
	if v, ok := present("displayName"); !ok {
		u.TitleKnown = true
	} else {
		var name string
		if json.Unmarshal(v, &name) == nil {
			u.DisplayName, u.TitleKnown = &name, true
		}
	}
	return u
}

// taskKeys and runKeys are the record fields the gate reads. A record that
// spells one in another case, or holds one twice, is refused: Go's decoder
// would match it where Desktop's JavaScript reads another value.
var (
	taskKeys = []string{"id", "displayName", "filePath", "cronExpression", "cronHumanReadable", "fireAt", "enabled", "cwd", "permissionMode"}
	runKeys  = []string{"sessionId", "cliSessionId", "scheduledTaskId", "permissionMode"}
)

// exactKeys checks the keys of the JSON object raw: each of fields at most
// once and spelled exactly. Other keys are free.
func exactKeys(raw []byte, fields []string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		k, _ := t.(string)
		for _, f := range fields {
			switch {
			case k == f && seen[f]:
				return fmt.Errorf("field %q appears twice", f)
			case k == f:
				seen[f] = true
			case strings.EqualFold(k, f):
				return fmt.Errorf("field %q is spelled %q", f, k)
			}
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// mightBeID reports whether an unreadable record could have this id.
func (u Unreadable) mightBeID(id string) bool { return u.ID == nil || *u.ID == id }

// mightBeTitle reports whether an unreadable record could have this
// display name, case ignored.
func (u Unreadable) mightBeTitle(ref string) bool {
	return !u.TitleKnown || u.DisplayName != nil && strings.EqualFold(strings.TrimSpace(*u.DisplayName), ref)
}

// find picks the one task match selects. It fails closed: a record that
// does not decode but might match makes it an error even beside a match,
// and a file that cannot be read at all does when nothing else matches.
func (l List) find(match func(Task) bool, mightBe func(Unreadable) bool, ref string) (Task, error) {
	var found []Task
	for _, t := range l.Tasks {
		if match(t) {
			found = append(found, t)
		}
	}
	for _, u := range l.Unreadable {
		if mightBe(u) {
			return Task{}, fmt.Errorf("a task in %s that cannot be read might be %q (%v)", filepath.Join(u.Dir, TasksFile), ref, u.Err)
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return Task{}, fmt.Errorf("%q names %d tasks: use the id", ref, len(found))
	case len(l.Broken) > 0:
		return Task{}, fmt.Errorf("task %q not found, and %v", ref, l.Broken[0])
	}
	return Task{}, fmt.Errorf("task %q: %w", ref, ErrNotFound)
}

// TaskByID returns the task with id from dir's task list.
func TaskByID(dir, id string) (Task, error) {
	if id == "" {
		return Task{}, errors.New("no task id")
	}
	return LoadTasks(dir).find(func(t Task) bool { return t.ID == id }, func(u Unreadable) bool { return u.mightBeID(id) }, id)
}

// AllTasks reads the task lists of every account/org folder under roots.
func AllTasks(ctx context.Context, roots []string) (List, error) {
	var out List
	for _, p := range matches(roots, TasksFile) {
		if err := ctx.Err(); err != nil {
			return List{}, err
		}
		l := LoadTasks(filepath.Dir(p))
		out.Tasks = append(out.Tasks, l.Tasks...)
		out.Unreadable = append(out.Unreadable, l.Unreadable...)
		out.Broken = append(out.Broken, l.Broken...)
	}
	return out, nil
}

// FindTaskByID finds the task with exactly this id in any account/org folder.
func FindTaskByID(ctx context.Context, roots []string, id string) (Task, error) {
	l, err := AllTasks(ctx, roots)
	if err != nil {
		return Task{}, err
	}
	return l.find(func(t Task) bool { return t.ID == id }, func(u Unreadable) bool { return u.mightBeID(id) }, id)
}

// FindTask finds a task by its id in any account/org folder, else by its
// display name, case ignored. Exactly one task must match.
func FindTask(ctx context.Context, roots []string, ref string) (Task, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Task{}, errors.New("no task given")
	}
	l, err := AllTasks(ctx, roots)
	if err != nil {
		return Task{}, err
	}
	t, err := l.find(func(t Task) bool { return t.ID == ref }, func(u Unreadable) bool { return u.mightBeID(ref) }, ref)
	if !errors.Is(err, ErrNotFound) {
		return t, err
	}
	return l.find(func(t Task) bool { return strings.EqualFold(strings.TrimSpace(t.DisplayName), ref) },
		func(u Unreadable) bool { return u.mightBeTitle(ref) }, ref)
}

// ConfigDir is Claude Code's config folder: CLAUDE_CONFIG_DIR, else ~/.claude.
func ConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// SkillPath is the task's SKILL.md: its filePath, else
// <ConfigDir>/scheduled-tasks/<id>/SKILL.md. A filePath that is not an
// absolute path is an error: which file it names would depend on the
// reader's working folder.
func SkillPath(t Task) (string, error) {
	if p := t.FilePath; strings.TrimSpace(p) != "" {
		if p != strings.TrimSpace(p) || !filepath.IsAbs(p) {
			return "", fmt.Errorf("the task's filePath %q is not an absolute path", p)
		}
		return filepath.Clean(p), nil
	}
	dir := ConfigDir()
	if dir == "" || t.ID == "" || strings.ContainsAny(t.ID, `/\.`) {
		return "", errors.New("the task names no SKILL.md")
	}
	p := filepath.Join(dir, "scheduled-tasks", t.ID, "SKILL.md")
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("the task's SKILL.md %q is not an absolute path", p)
	}
	return p, nil
}

// SkillFile is a parsed SKILL.md.
type SkillFile struct {
	Prompt string   // the body: what a run is told
	Extra  []string // front matter keys other than name, description and model
}

var (
	// skillRe, nameRe and descRe are Claude Desktop's own expressions for a
	// task file (its I3 parser): "---\n", the front matter up to the first
	// "\n---\n", any further newlines, then the body.
	skillRe     = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n+(.*)\z`)
	nameRe      = regexp.MustCompile(`(?m)^name: (.+)$`)
	descRe      = regexp.MustCompile(`^description: (.+)$`)
	descEmptyRe = regexp.MustCompile(`^description:[ \t]*$`)
	keyRe       = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*):`)
)

// ParseSkill reads a SKILL.md as Claude Desktop does: a file it would not
// run (no front matter, no name or description, an empty body) is an
// error, and so is anything this reader might read differently from it: a
// file that is not UTF-8, a carriage return or line separator left in the
// front matter, or a line that is a delimiter but for spaces.
func ParseSkill(file []byte) (SkillFile, error) {
	if !utf8.Valid(file) {
		return SkillFile{}, errors.New("the task's SKILL.md is not UTF-8")
	}
	s := strings.ReplaceAll(string(file), "\r\n", "\n")
	m := skillRe.FindStringSubmatch(s)
	if m == nil {
		return SkillFile{}, errors.New("the task's SKILL.md has no front matter between --- lines")
	}
	fm, body := m[1], m[2]
	if strings.ContainsAny(fm, "\r  ") {
		return SkillFile{}, errors.New("the task's SKILL.md front matter holds a line break other than \n")
	}
	lines := strings.Split(fm, "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == "---" {
			return SkillFile{}, errors.New("the task's SKILL.md front matter holds a --- line with spaces")
		}
	}
	if !nameRe.MatchString(fm) {
		return SkillFile{}, errors.New("the task's SKILL.md front matter has no name")
	}
	desc := false
	for i, l := range lines {
		if descRe.MatchString(l) || descEmptyRe.MatchString(l) && (i == len(lines)-1 || keyRe.MatchString(lines[i+1])) {
			desc = true
			break
		}
	}
	if !desc {
		return SkillFile{}, errors.New("the task's SKILL.md front matter has no description")
	}
	var f SkillFile
	for _, l := range lines {
		if k := keyRe.FindStringSubmatch(l); k != nil && k[1] != "name" && k[1] != "description" && k[1] != "model" &&
			!slices.Contains(f.Extra, k[1]) {
			f.Extra = append(f.Extra, k[1])
		}
	}
	f.Prompt = strings.TrimRightFunc(body, jsSpace)
	if f.Prompt == "" {
		return SkillFile{}, errors.New("the task's SKILL.md has no prompt")
	}
	return f, nil
}

// jsSpace is JavaScript's whitespace, which String.prototype.trimEnd removes.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0xFEFF, 0x2028, 0x2029:
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// ReadSkill reads and parses the task's SKILL.md once: callers fingerprint
// and show the same bytes. A file that is missing, unreadable or over 1 MB
// is an error, and so is one ParseSkill refuses.
func ReadSkill(t Task) (file []byte, f SkillFile, err error) {
	p, err := SkillPath(t)
	if err != nil {
		return nil, SkillFile{}, err
	}
	b, tooBig, err := readCapped(p, maxSkill)
	if err != nil {
		return nil, SkillFile{}, fmt.Errorf("read the task's SKILL.md: %w", err)
	}
	if tooBig {
		return nil, SkillFile{}, fmt.Errorf("the task's SKILL.md is over %d MB", maxSkill>>20)
	}
	if f, err = ParseSkill(b); err != nil {
		return nil, SkillFile{}, err
	}
	return b, f, nil
}

// FingerprintOf identifies what a task runs, where and when, given the bytes
// of its SKILL.md: the hex SHA-256 of the JSON array [SHA-256 of the file,
// cron expression, fireAt (FireAt.Canonical), cwd, the SKILL.md's absolute
// path]. Its display name, permission mode and bookkeeping fields do not
// count: the gate checks the mode at every send instead.
func FingerprintOf(t Task, file []byte) (string, error) {
	p, err := SkillPath(t)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(file)
	b, err := json.Marshal([]string{hex.EncodeToString(sum[:]), t.Cron, t.FireAt.Canonical(), t.Cwd, p})
	if err != nil {
		return "", err
	}
	sum = sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Fingerprint reads the task's SKILL.md and returns FingerprintOf it.
func Fingerprint(t Task) (string, error) {
	file, _, err := ReadSkill(t)
	if err != nil {
		return "", err
	}
	return FingerprintOf(t, file)
}
