package hooks

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/autosend"
	"github.com/vukappa4/whatsapp-mcp/internal/desktop"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/useracl"
)

// findRun and findTask are the desktop readers; tests replace them.
var (
	findRun  = desktop.FindRun
	findTask = desktop.FindTask
)

// Decision kinds of the PreToolUse hook. Silent ("") leaves the call to
// Claude Code's normal permission flow.
const (
	silent = ""
	allow  = "allow"
	ask    = "ask"
	deny   = "deny"
)

// decision is what PreTool prints: Claude Code shows an "ask" reason to the
// user and a "deny" reason to Claude.
type decision struct {
	kind   string
	reason string
}

// sendTool is the only tool an approved scheduled task may call unasked.
const sendTool = "whatsapp_send_message"

// maxCardPrompt is the longest task prompt the approval card shows whole.
const maxCardPrompt = 1500

// autosendProfile maps the exact MCP server names setup-claude registers
// (install's mcpServerName) to their profile. Autosend decisions are made
// only for these: any other server named like whatsapp gets the user-prompt
// gate, which asks.
func autosendProfile(server string) (string, bool) {
	switch server {
	case "whatsapp":
		return "prod", true
	case "whatsapp-dev":
		return "dev", true
	}
	return "", false
}

// promptlessModes settle a confirmation without showing it to the user:
// dontAsk denies it, unless a mod approves it, and bypassPermissions skips
// prompts. An approval card needs a person to read it. Auto mode shows a
// hook's "ask" to the user (the classifier does not decide it).
var promptlessModes = map[string]bool{"bypassPermissions": true, "dontAsk": true}

// decide answers one gated call:
//
//  1. Revoking an autosend approval only narrows what may be sent: allow.
//  2. An autosend approval always shows the user a card (ask), or is denied
//     when it cannot be shown truthfully (approvalCard). Never silent.
//  3. Only in a session Claude Desktop started for a scheduled task
//     (CLAUDE_CODE_HOST_SCHEDULED_RUN=1), within PreToolBudget, it reads the
//     Desktop session file: the call is unattended unless the file shows the
//     user opened the run and continued it.
//  4. A whatsapp_send_message in an unattended run whose task is approved,
//     unchanged since, and sends to an approved chat less than
//     autosend.MaxPerDay times a day: allow, once the autosend log holds it.
//  5. Otherwise the user-prompt gate decides (askReason), and an unattended
//     call it would leave silent asks anyway: no prompt of the user's
//     backs it.
//
// Steps 1, 2 and 4 hold only for the exact server names setup-claude
// registers. Nothing here turns an ask of the user-prompt gate into silence.
func decide(in toolInput, inputErr error, env PreToolEnv) decision {
	server, tool, _ := gatedTool(in.ToolName)
	profile, exact := autosendProfile(server)
	switch {
	case exact && inputErr == nil && tool == autosend.ToolRevoke:
		return decision{allow, "WhatsApp autosend: ending an approval only narrows what may be sent."}
	case exact && tool == autosend.ToolAllow:
		ctx, cancel := context.WithTimeout(context.Background(), PreToolBudget)
		defer cancel()
		d, ok := withBudget(ctx, func(ctx context.Context) decision { return approvalCard(ctx, in, inputErr, profile, env) })
		if !ok {
			return decision{deny, "WhatsApp autosend: the approval check timed out. Call again."}
		}
		return d
	}
	var l lookup
	if inputErr == nil && env.Scheduled {
		ctx, cancel := context.WithTimeout(context.Background(), PreToolBudget)
		var ok bool
		l, ok = withBudget(ctx, func(ctx context.Context) lookup { return lookupRun(ctx, in, server, tool, env) })
		cancel()
		if !ok {
			l = lookup{unattended: true, why: "the check of Claude Desktop's files timed out"}
		}
	}
	if a := l.allow; a != nil {
		n, err := countAutosends(env, profile, a.taskID, a.jid)
		switch {
		case err != nil:
			l.why = "cannot read the autosend log (" + err.Error() + ")"
		case n >= autosend.MaxPerDay:
			l.why = fmt.Sprintf("the task reached %d autosends to %s in 24 hours", autosend.MaxPerDay, display(a.label))
		default:
			if err := writeAudit(env, profile, in.SessionID, a); err != nil {
				l.why = "cannot write the autosend log (" + err.Error() + ")"
				break
			}
			return decision{allow, fmt.Sprintf("WhatsApp autosend: task %s approved %s sends to %s.", display(a.taskName),
				time.Unix(a.approvedAt, 0).Format("2006-01-02"), display(a.label))}
		}
	}
	r, what := askReason(in, inputErr, env)
	if l.unattended && r == "" {
		r = "WhatsApp: a scheduled task, not you, asked for this send. Confirm " + what + "."
	}
	if r == "" {
		return decision{}
	}
	if l.why != "" {
		r += " Autosend: " + displayN(l.why, 200) + "."
	}
	return decision{ask, r}
}

// withBudget runs fn and waits for it until ctx ends; ok is false when it
// did not finish in time.
func withBudget[T any](ctx context.Context, fn func(context.Context) T) (v T, ok bool) {
	done := make(chan T, 1)
	go func() { done <- fn(ctx) }()
	select {
	case v := <-done:
		return v, true
	case <-ctx.Done():
		return v, false
	}
}

// lookup is what the Desktop session says about a gated call.
type lookup struct {
	unattended bool
	why        string    // why an unattended send is not autosent
	allow      *autoSend // an approved autosend, to check and log before allowing
}

// autoSend is one send an approved task may make.
type autoSend struct {
	taskID, taskName string
	approvedAt       int64
	to, jid, label   string
	textChars        int
}

// lookupRun runs steps 3 and 4 of decide in a scheduled session.
func lookupRun(ctx context.Context, in toolInput, server, tool string, env PreToolEnv) lookup {
	run, err := findRun(ctx, env.DesktopRoots, in.SessionID)
	if err == nil && run.Continued {
		return lookup{} // the user opened the run and wrote in it: the user-prompt gate decides
	}
	l := lookup{unattended: true}
	if tool != sendTool {
		return l
	}
	if l.why = autosendCheck(ctx, in, server, env, run, err, &l); l.why != "" {
		l.allow = nil
	}
	return l
}

// sendArgKeys are the arguments of whatsapp_send_message, spelled exactly.
var sendArgKeys = map[string]bool{"to": true, "text": true, "reply_to": true, "idempotency_key": true}

// sendArgs are whatsapp_send_message's arguments as an autosend reads them.
type sendArgs struct {
	To             string `json:"to"`
	Text           string `json:"text"`
	ReplyTo        string `json:"reply_to"`
	IdempotencyKey string `json:"idempotency_key"`
}

// strictSendArgs decodes whatsapp_send_message's arguments for an autosend:
// one JSON object whose keys are exactly the tool's, each once, with string
// values. Go's decoder matches keys in any case and keeps the last of a
// repeated one; an argument the gate and the server could read differently
// is refused.
func strictSendArgs(raw json.RawMessage) (sendArgs, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return sendArgs{}, errors.New("the arguments are not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return sendArgs{}, err
		}
		k, _ := t.(string)
		if !sendArgKeys[k] || seen[k] {
			return sendArgs{}, fmt.Errorf("unexpected argument %s", strconv.Quote(displayN(k, 40)))
		}
		seen[k] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return sendArgs{}, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return sendArgs{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return sendArgs{}, errors.New("data after the arguments")
	}
	var a sendArgs
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&a); err != nil {
		return sendArgs{}, err
	}
	return a, nil
}

// autosendCheck sets l.allow when an approved task may send this message,
// and returns why not otherwise.
func autosendCheck(ctx context.Context, in toolInput, server string, env PreToolEnv, run desktop.Run, runErr error, l *lookup) string {
	profile, exact := autosendProfile(server)
	switch {
	case !exact:
		return "the server " + display(server) + " is not one setup-claude registers"
	case runErr != nil:
		return "cannot find this run in Claude Desktop's files (" + runErr.Error() + ")"
	case run.TaskID == "":
		return "this run names no scheduled task"
	case promptlessModes[run.PermissionMode]:
		return "the run's permission mode is " + run.PermissionMode
	}
	args, err := strictSendArgs(in.ToolInput)
	if err != nil {
		return err.Error()
	}
	to := strings.TrimSpace(args.To)
	if _, kind := parseRef(to); kind == "" {
		return "to must be an approved c_N or p_N id, not " + display(to)
	}
	task, err := desktop.TaskByID(run.Dir, run.TaskID)
	if err != nil {
		return "task " + display(run.TaskID) + ": " + err.Error()
	}
	db, err := openProfileDB(env.AppDir, profile)
	if err != nil {
		return err.Error()
	}
	defer db.Close()
	rule, err := store.AutosendRuleFor(ctx, db, task.ID)
	if errors.Is(err, store.ErrNoRule) {
		return "task " + display(task.Title()) + " is not approved"
	}
	if err != nil {
		return "cannot read the approvals (" + err.Error() + ")"
	}
	if !samePath(rule.TaskDir, run.Dir) {
		return "this run's task is in another Claude Desktop account or organization than the approved one"
	}
	file, _, err := desktop.ReadSkill(task)
	if err != nil {
		return err.Error()
	}
	fp, err := desktop.FingerprintOf(task, file)
	if err != nil {
		return err.Error()
	}
	if fp != rule.Fingerprint {
		return "the task's prompt, schedule, permission mode or folder changed since it was approved"
	}
	dest, err := destination(ctx, db, to)
	if err != nil {
		return err.Error()
	}
	if !slices.ContainsFunc(rule.Chats, func(c store.AutosendChat) bool { return c.JID == dest.JID }) {
		return display(dest.Label) + " is not among the chats approved for task " + display(task.Title())
	}
	l.allow = &autoSend{taskID: task.ID, taskName: task.Title(), approvedAt: rule.ApprovedAt, to: to, jid: dest.JID,
		label: dest.Label, textChars: len([]rune(args.Text))}
	return ""
}

// samePath compares two folder paths read from the same roots.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || filepath.Separator == '\\' && strings.EqualFold(a, b)
}

// approvalCard answers whatsapp_send_autosend_allow: an "ask" whose reason is
// the card the user approves, after writing the receipt the daemon needs to
// store the approval, or a "deny" telling Claude why no card can be shown.
func approvalCard(ctx context.Context, in toolInput, inputErr error, profile string, env PreToolEnv) decision {
	const lead = "WhatsApp autosend: "
	no := func(why string) decision { return decision{deny, lead + why} }
	var args autosend.Args
	if inputErr != nil || in.SessionID == "" || json.Unmarshal(in.ToolInput, &args) != nil {
		return no("the hook could not read this call.")
	}
	if env.Scheduled {
		return no("an approval can only be given in a session with you, never by a scheduled run.")
	}
	if promptlessModes[in.PermissionMode] {
		return no("this session's permission mode " + in.PermissionMode + " would settle the approval without showing it to you: " +
			"switch to another mode, then call again.")
	}
	task, err := findTask(ctx, env.DesktopRoots, args.Task)
	switch {
	case errors.Is(err, desktop.ErrNotFound):
		return no("no Claude Desktop scheduled task has the id or title " + strconv.Quote(display(args.Task)) +
			": create it first, then call again with the taskId given to create_scheduled_task.")
	case err != nil:
		return no("cannot find the task (" + display(err.Error()) + ").")
	}
	file, skill, err := desktop.ReadSkill(task)
	if err != nil {
		return no("cannot read what the task runs (" + display(err.Error()) + ").")
	}
	fp, err := desktop.FingerprintOf(task, file)
	if err != nil {
		return no("cannot read what the task runs (" + display(err.Error()) + ").")
	}
	if promptlessModes[task.PermissionMode] {
		return no("the task runs with permission mode " + task.PermissionMode + ", where a run settles confirmations without " +
			"you or rewrites any file, the send gate's included: give it another mode first.")
	}
	if n := len([]rune(skill.Prompt)); n > maxCardPrompt {
		return no(fmt.Sprintf("the task's prompt is too long to show on a card (%d chars): shorten it to %d.", n, maxCardPrompt))
	}
	refs := autosend.Normalize(args.Chats)
	if len(refs) == 0 {
		return no("name at least one chat: use c_N or p_N ids.")
	}
	db, err := openProfileDB(env.AppDir, profile)
	if err != nil {
		return no(err.Error() + ".")
	}
	defer db.Close()
	var chats []autosend.Chat
	for _, ref := range refs {
		c, err := destination(ctx, db, ref)
		if err != nil {
			return no("cannot approve " + display(ref) + ": " + display(err.Error()) + ". Use c_N or p_N ids.")
		}
		if !slices.ContainsFunc(chats, func(x autosend.Chat) bool { return x.JID == c.JID }) {
			chats = append(chats, c)
		}
	}
	if env.Dir == "" {
		return no("the hook has no folder for the approval.")
	}
	// Past the budget the hook has already answered: no receipt may outlive it.
	if ctx.Err() != nil {
		return no("the approval check timed out. Call again.")
	}
	r := autosend.Receipt{At: env.Now.Unix(), Profile: profile, TaskID: task.ID, TaskName: task.Title(), TaskDir: task.Dir,
		Schedule: task.Schedule(), Fingerprint: fp, Chats: chats}
	err = autosend.WriteReceipt(env.Dir, autosend.ArgsKey(profile, args.Task, args.Chats), r)
	switch {
	case errors.Is(err, autosend.ErrReceiptConflict):
		return no(fmt.Sprintf("another approval card for these arguments is waiting with other content: answer it, or wait %d minutes, then call again.",
			int(autosend.ReceiptTTL.Minutes())))
	case err != nil:
		return no("cannot record the approval (" + display(err.Error()) + ").")
	}
	labels := make([]string, len(chats))
	for i, c := range chats {
		labels[i] = display(c.Label)
	}
	mode := task.PermissionMode
	if mode == "" {
		mode = "default"
	}
	folder := "no folder"
	if strings.TrimSpace(task.Cwd) != "" {
		folder = "folder " + displayN(task.Cwd, 200)
	}
	extra := ""
	if len(skill.Extra) > 0 {
		extra = " Its SKILL.md also sets: " + displayN(strings.Join(skill.Extra, ", "), 200) + "."
	}
	return decision{ask, fmt.Sprintf("%slet the Desktop scheduled task %q (%s, mode %s, %s) send text messages with nobody present to: %s. "+
		"Its whole prompt: %q.%s Approve only if you asked for this. It lasts until revoked or until the task's prompt, schedule, "+
		"mode or folder changes, and allows at most %d sends a day to each chat.",
		lead, display(task.Title()), display(task.Schedule()), display(mode), folder, strings.Join(labels, ", "),
		displayN(skill.Prompt, maxCardPrompt+1), extra, autosend.MaxPerDay)}
}

// openProfileDB opens <appDir>/<profile>/data.db read-only, as the resolver does.
func openProfileDB(appDir, profile string) (*sql.DB, error) {
	if appDir == "" {
		return nil, errors.New("no WhatsApp MCP folder")
	}
	path := filepath.Join(appDir, profile, "data.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no %s data", profile)
	}
	db, err := sql.Open("sqlite", store.ReadOnlyDSNWait(path, resolveBusyTimeout))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// destination is where a send to ref goes, as the server resolves it: a
// group chat's JID, or the JID store.SendJID picks for a person (a DM chat
// goes to its person). Only c_N and p_N ids: names and numbers are refused,
// since another person can take a name or a number.
func destination(ctx context.Context, db *sql.DB, ref string) (autosend.Chat, error) {
	ref = strings.TrimSpace(ref)
	id, kind := parseRef(ref)
	var c autosend.Chat
	var err error
	switch kind {
	case "p":
		c, err = personDestination(ctx, db, id)
	case "c":
		var chatKind, name, jid string
		var person sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT kind, person_id, name, jid FROM chats WHERE id = ?`, id).Scan(&chatKind, &person, &name, &jid); err != nil {
			return autosend.Chat{}, fmt.Errorf("chat %s: %w", ref, err)
		}
		switch {
		case chatKind == "dm" && person.Valid:
			c, err = personDestination(ctx, db, person.Int64)
		case chatKind == "group":
			if name == "" {
				name = "unnamed"
			}
			c = autosend.Chat{JID: jid, Label: "group " + name + " (" + ref + ")"}
		default:
			return autosend.Chat{}, fmt.Errorf("chat %s is a %s: only people and groups receive messages", ref, chatKind)
		}
	default:
		return autosend.Chat{}, fmt.Errorf("%s is not a c_N or p_N id", display(ref))
	}
	c.Ref = ref
	return c, err
}

func personDestination(ctx context.Context, db *sql.DB, id int64) (autosend.Chat, error) {
	var r Recipient
	if err := (DBResolver{}).person(ctx, db, id, &r); err != nil {
		return autosend.Chat{}, err
	}
	var phone sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT phone_e164 FROM persons WHERE id = ?`, id).Scan(&phone); err != nil {
		return autosend.Chat{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT jid, kind FROM jids WHERE person_id = ?`, id)
	if err != nil {
		return autosend.Chat{}, err
	}
	var jids []store.JIDRow
	for rows.Next() {
		var j store.JIDRow
		if err := rows.Scan(&j.JID, &j.Kind); err != nil {
			rows.Close()
			return autosend.Chat{}, err
		}
		jids = append(jids, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return autosend.Chat{}, err
	}
	jid, _, err := store.SendJID(jids, phone.String)
	if err != nil {
		return autosend.Chat{}, fmt.Errorf("p_%d %w", id, err)
	}
	label := r.Label
	if label == "" {
		label = "p_" + strconv.FormatInt(id, 10)
	}
	return autosend.Chat{JID: jid, Label: label}, nil
}

// maxAuditLog is the size past which the autosend log moves to autosend.log.1.
const maxAuditLog = 1 << 20

// auditEntry is one line of the autosend log. It never holds the text.
type auditEntry struct {
	At        string `json:"at"`
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id"`
	TaskName  string `json:"task_name"`
	To        string `json:"to"`
	JID       string `json:"jid"`
	Label     string `json:"label"`
	TextChars int    `json:"text_chars"`
}

func auditPath(env PreToolEnv, profile string) (string, error) {
	if env.AppDir == "" {
		return "", errors.New("no WhatsApp MCP folder")
	}
	return filepath.Join(env.AppDir, profile, "logs", autosend.LogName), nil
}

// countAutosends counts the sends the autosend log (and its rotated copy)
// records for the task to jid in the 24 hours before env.Now.
func countAutosends(env PreToolEnv, profile, taskID, jid string) (int, error) {
	path, err := auditPath(env, profile)
	if err != nil {
		return 0, err
	}
	since := env.Now.Add(-24 * time.Hour)
	n := 0
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e auditEntry
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.TaskID != taskID || e.JID != jid {
				continue
			}
			if at, err := time.Parse(time.RFC3339, e.At); err == nil && at.After(since) {
				n++
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return 0, err
		}
	}
	return n, nil
}

// writeAudit appends the send to <AppDir>/<profile>/logs/autosend.log,
// readable by the user only. The send is allowed only once it is logged.
func writeAudit(env PreToolEnv, profile, session string, a *autoSend) error {
	path, err := auditPath(env, profile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxAuditLog {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	b, err := json.Marshal(auditEntry{At: env.Now.UTC().Format(time.RFC3339), SessionID: session, TaskID: a.taskID, TaskName: a.taskName,
		To: a.to, JID: a.jid, Label: a.label, TextChars: a.textChars})
	if err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		if err := useracl.Restrict(path, false); err != nil {
			f.Close()
			return err
		}
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// scheduledDir holds a marker per scheduled session whose first prompt the
// UserPromptSubmit hook saw. It is a folder, which pruneFlags leaves alone.
func scheduledDir(dir string) string { return filepath.Join(dir, "scheduled") }

// markerTTL is how long a scheduled session's marker is kept.
const markerTTL = 30 * 24 * time.Hour

// firstScheduledPrompt reports whether this is the first prompt of a
// session Claude Desktop started for a scheduled task, and records that it
// saw one. That prompt is the task's SKILL.md, also when the user starts the
// run by hand. When it cannot tell, it says first: the caller then writes no
// send-intent flag, and a send asks.
func firstScheduledPrompt(dir, session string, now time.Time) bool {
	md := scheduledDir(dir)
	if err := os.MkdirAll(md, 0o700); err != nil {
		return true
	}
	if useracl.Restrict(dir, true) != nil || useracl.Restrict(md, true) != nil {
		return true
	}
	if entries, err := os.ReadDir(md); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > markerTTL {
				_ = os.Remove(filepath.Join(md, e.Name()))
			}
		}
	}
	f, err := os.OpenFile(flagPath(md, session), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return !errors.Is(err, fs.ErrExist)
	}
	f.Close()
	return true
}
