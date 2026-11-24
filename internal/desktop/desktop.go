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
	"runtime"
	"slices"
	"strings"
	"time"
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
// <ConfigDir>/scheduled-tasks/<id>/SKILL.md.
func SkillPath(t Task) (string, error) {
	if p := strings.TrimSpace(t.FilePath); p != "" {
		return p, nil
	}
	dir := ConfigDir()
	if dir == "" || t.ID == "" || strings.ContainsAny(t.ID, `/\.`) {
		return "", errors.New("the task names no SKILL.md")
	}
	return filepath.Join(dir, "scheduled-tasks", t.ID, "SKILL.md"), nil
}

// Skill reads the task's SKILL.md: the whole file and its body, the prompt
// a run is given (the text after the YAML front matter between --- lines).
// A file that is missing, unreadable, over 1 MB or with an empty body is an
// error: the gate cannot tell what the task does.
func Skill(t Task) (file []byte, body string, err error) {
	p, err := SkillPath(t)
	if err != nil {
		return nil, "", err
	}
	b, tooBig, err := readCapped(p, maxSkill)
	if err != nil {
		return nil, "", fmt.Errorf("read the task's SKILL.md: %w", err)
	}
	if tooBig {
		return nil, "", fmt.Errorf("the task's SKILL.md is over %d MB", maxSkill>>20)
	}
	body = strings.TrimSpace(skillBody(string(b)))
	if body == "" {
		return nil, "", errors.New("the task's SKILL.md has no prompt")
	}
	return b, body, nil
}

// skillBody strips YAML front matter: a first line "---" up to the next
// line "---".
func skillBody(s string) string {
	s = strings.TrimPrefix(s, "\xEF\xBB\xBF")
	lines := strings.SplitAfter(s, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return s
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[i+1:], "")
		}
	}
	return "" // front matter never closed: no body
}

// Fingerprint identifies what a task runs and when: the hex SHA-256 of the
// JSON array [SHA-256 of its whole SKILL.md, cron expression, fireAt,
// permission mode]. Its display name, folder and bookkeeping fields do not
// count. A SKILL.md Skill cannot read is an error.
func Fingerprint(t Task) (string, error) {
	file, _, err := Skill(t)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(file)
	b, err := json.Marshal([]string{hex.EncodeToString(sum[:]), t.Cron, t.FireAt, t.PermissionMode})
	if err != nil {
		return "", err
	}
	sum = sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
