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
	"strings"
	"time"
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

// Task is one Desktop scheduled task. Its prompt is not in the record: it
// is the body of the task's SKILL.md (see Skill), which Desktop reads again
// at every run.
type Task struct {
	ID             string `json:"id"` // kebab-case: ^[a-z0-9_-]+$
	DisplayName    string `json:"displayName"`
	FilePath       string `json:"filePath"` // its SKILL.md
	Cron           string `json:"cronExpression"`
	CronHuman      string `json:"cronHumanReadable"` // optional
	FireAt         string `json:"fireAt"`            // ISO 8601, for a one-shot task
	Enabled        *bool  `json:"enabled"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permissionMode"`
	Dir            string `json:"-"` // the account/org folder it was read from
}

// Title is the name the user sees: the display name, else the id.
func (t Task) Title() string {
	if s := strings.TrimSpace(t.DisplayName); s != "" {
		return s
	}
	return t.ID
}

// Schedule describes when the task runs: the human-readable schedule, else
// "cron <expression>", else "once at <fireAt>", else "manual only".
func (t Task) Schedule() string {
	switch {
	case strings.TrimSpace(t.CronHuman) != "":
		return strings.TrimSpace(t.CronHuman)
	case strings.TrimSpace(t.Cron) != "":
		return "cron " + strings.TrimSpace(t.Cron)
	case strings.TrimSpace(t.FireAt) != "":
		return "once at " + strings.TrimSpace(t.FireAt)
	}
	return "manual only"
}

// Run is one local Claude Code session Desktop started. A scheduled run
// names its task; Continued is set once the user opens it and writes in it.
type Run struct {
	SessionID      string `json:"sessionId"`
	CLISessionID   string `json:"cliSessionId"`
	TaskID         string `json:"scheduledTaskId"`
	Continued      bool   `json:"scheduledRunContinued"`
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

// LoadTasks reads dir/scheduled-tasks.json. A missing file is no tasks.
func LoadTasks(dir string) ([]Task, error) {
	b, tooBig, err := readCapped(filepath.Join(dir, TasksFile), maxFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if tooBig {
		return nil, fmt.Errorf("%s is over %d MB", TasksFile, maxFile>>20)
	}
	var f struct {
		ScheduledTasks []Task `json:"scheduledTasks"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("read %s: %w", TasksFile, err)
	}
	for i := range f.ScheduledTasks {
		f.ScheduledTasks[i].Dir = dir
	}
	return f.ScheduledTasks, nil
}

// TaskByID returns the task with id from dir's task list.
func TaskByID(dir, id string) (Task, error) {
	ts, err := LoadTasks(dir)
	if err != nil {
		return Task{}, err
	}
	for _, t := range ts {
		if t.ID == id && id != "" {
			return t, nil
		}
	}
	return Task{}, fmt.Errorf("task %s: %w", id, ErrNotFound)
}

// allTasks reads the task lists of every account/org folder under roots.
func allTasks(ctx context.Context, roots []string) ([]Task, error) {
	var out []Task
	for _, p := range matches(roots, TasksFile) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ts, err := LoadTasks(filepath.Dir(p))
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

// FindTaskByID finds the task with exactly this id in any account/org folder.
func FindTaskByID(ctx context.Context, roots []string, id string) (Task, error) {
	ts, err := allTasks(ctx, roots)
	if err != nil {
		return Task{}, err
	}
	return one(ts, func(t Task) bool { return t.ID == id }, id)
}

// FindTask finds a task by its id in any account/org folder, else by its
// display name, case ignored. Exactly one task must match.
func FindTask(ctx context.Context, roots []string, ref string) (Task, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Task{}, errors.New("no task given")
	}
	ts, err := allTasks(ctx, roots)
	if err != nil {
		return Task{}, err
	}
	if t, err := one(ts, func(t Task) bool { return t.ID == ref }, ref); !errors.Is(err, ErrNotFound) {
		return t, err
	}
	return one(ts, func(t Task) bool { return strings.EqualFold(strings.TrimSpace(t.DisplayName), ref) }, ref)
}

func one(ts []Task, match func(Task) bool, ref string) (Task, error) {
	var found []Task
	for _, t := range ts {
		if match(t) {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 0:
		return Task{}, fmt.Errorf("task %q: %w", ref, ErrNotFound)
	case 1:
		return found[0], nil
	}
	return Task{}, fmt.Errorf("%q names %d tasks: use the id", ref, len(found))
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
// cron expression, fireAt, permission mode, cwd, the SKILL.md's absolute
// path]. Its display name and bookkeeping fields do not count.
func FingerprintOf(t Task, file []byte) (string, error) {
	p, err := SkillPath(t)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(file)
	b, err := json.Marshal([]string{hex.EncodeToString(sum[:]), t.Cron, t.FireAt, t.PermissionMode, t.Cwd, p})
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
