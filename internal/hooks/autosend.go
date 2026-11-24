package hooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// findRun is desktop.FindRun; tests replace it.
var findRun = desktop.FindRun

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

// decide answers one gated call:
//
//  1. Revoking an autosend approval only narrows what may be sent: allow.
//  2. An autosend approval always shows the user a card (ask), or is denied
//     when it cannot be shown truthfully (approvalCard). Never silent.
//  3. Within PreToolBudget, it looks up the Desktop session: the call is
//     unattended when Claude Desktop says this session is a scheduled run
//     (CLAUDE_CODE_HOST_SCHEDULED_RUN=1) or its session file names a task,
//     unless the user opened and continued the run.
//  4. A whatsapp_send_message in an unattended scheduled run whose task is
//     approved, unchanged since, and sends to an approved chat: allow, once
//     the autosend log holds the send.
//  5. Otherwise the user-prompt gate decides (askReason), and an unattended
//     call it would leave silent asks anyway: no prompt of the user's
//     backs it.
//
// Nothing here turns an ask of the user-prompt gate into silence.
func decide(in toolInput, inputErr error, env PreToolEnv) decision {
	server, tool, _ := gatedTool(in.ToolName)
	switch {
	case inputErr == nil && tool == autosend.ToolRevoke:
		return decision{allow, "WhatsApp autosend: ending an approval only narrows what may be sent."}
	case tool == autosend.ToolAllow:
		ctx, cancel := context.WithTimeout(context.Background(), PreToolBudget)
		defer cancel()
		d, ok := withBudget(ctx, func(ctx context.Context) decision { return approvalCard(ctx, in, inputErr, server, env) })
		if !ok {
			return decision{deny, "WhatsApp autosend: the approval check timed out. Call again."}
		}
		return d
	}
	var l lookup
	if inputErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), PreToolBudget)
		var ok bool
		l, ok = withBudget(ctx, func(ctx context.Context) lookup { return lookupRun(ctx, in, server, tool, env) })
		cancel()
		if !ok {
			l = lookup{unattended: true, why: "the check of Claude Desktop's files timed out"}
		}
	}
	if l.allow != nil {
		if err := writeAudit(env, ProfileOf(server), in.SessionID, l.allow); err != nil {
			l.why = "cannot write the autosend log (" + err.Error() + ")"
		} else {
			return decision{allow, fmt.Sprintf("WhatsApp autosend: task %s approved %s sends to %s.", display(l.allow.taskName),
				time.Unix(l.allow.approvedAt, 0).Format("2006-01-02"), display(l.allow.label))}
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
	allow      *autoSend // an approved autosend, to log before allowing
}

// autoSend is one send an approved task may make.
type autoSend struct {
	taskID, taskName string
	approvedAt       int64
	to, jid, label   string
	textChars        int
}

// lookupRun runs steps 3 and 4 of decide.
func lookupRun(ctx context.Context, in toolInput, server, tool string, env PreToolEnv) lookup {
	run, err := findRun(ctx, env.DesktopRoots, in.SessionID)
	var l lookup
	switch {
	case err == nil && run.Continued:
		return l // the user opened the run and wrote in it: the user-prompt gate decides
	case env.Scheduled:
		l.unattended = true
	case err == nil && run.TaskID != "":
		l.unattended = true
	}
	if !l.unattended || tool != sendTool {
		return l
	}
	if l.why = autosendCheck(ctx, in, server, env, run, err, &l); l.why != "" {
		l.allow = nil
	}
	return l
}

// autosendCheck sets l.allow when an approved task may send this message,
// and returns why not otherwise.
func autosendCheck(ctx context.Context, in toolInput, server string, env PreToolEnv, run desktop.Run, runErr error, l *lookup) string {
	switch {
	case !env.Scheduled:
		return "Claude Code does not say this session is a Desktop scheduled run"
	case runErr != nil:
		return "cannot find this run in Claude Desktop's files (" + runErr.Error() + ")"
	case run.TaskID == "":
		return "this run names no scheduled task"
	case run.PermissionMode == "bypassPermissions":
		return "the run bypasses permissions"
	}
	var args struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(in.ToolInput, &args)
	to := strings.TrimSpace(args.To)
	if _, kind := parseRef(to); kind == "" {
		return "to must be an approved c_N or p_N id, not " + display(to)
	}
	task, err := desktop.TaskByID(run.Dir, run.TaskID)
	if err != nil {
		return "task " + display(run.TaskID) + ": " + err.Error()
	}
	db, err := openProfileDB(env.AppDir, ProfileOf(server))
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
	fp, err := desktop.Fingerprint(task)
	if err != nil {
		return err.Error()
	}
	if fp != rule.Fingerprint {
		return "the task's prompt, schedule or permission mode changed since it was approved"
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

// approvalCard answers whatsapp_send_autosend_allow: an "ask" whose reason is
// the card the user approves, after writing the receipt the daemon needs to
// store the approval, or a "deny" telling Claude why no card can be shown.
func approvalCard(ctx context.Context, in toolInput, inputErr error, server string, env PreToolEnv) decision {
	const lead = "WhatsApp autosend: "
	no := func(why string) decision { return decision{deny, lead + why} }
	var args autosend.Args
	if inputErr != nil || in.SessionID == "" || json.Unmarshal(in.ToolInput, &args) != nil {
		return no("the hook could not read this call.")
	}
	never := no("an approval can only be given in a session with you, never by a scheduled run.")
	if env.Scheduled {
		return never
	}
	if run, err := findRun(ctx, env.DesktopRoots, in.SessionID); err == nil && run.TaskID != "" && !run.Continued {
		return never
	}
	task, err := desktop.FindTask(ctx, env.DesktopRoots, args.Task)
	switch {
	case errors.Is(err, desktop.ErrNotFound):
		return no("no Claude Desktop scheduled task has the id or name " + strconv.Quote(display(args.Task)) +
			": create it first, then call again with the taskId given to create_scheduled_task.")
	case err != nil:
		return no("cannot find the task (" + display(err.Error()) + ").")
	}
	fp, err := desktop.Fingerprint(task)
	if err != nil {
		return no("cannot read what the task runs (" + display(err.Error()) + ").")
	}
	_, prompt, _ := desktop.Skill(task)
	if task.PermissionMode == "bypassPermissions" {
		return no("the task runs with permission mode bypassPermissions, which lets a run rewrite any file, the send gate's " +
			"included: give it another mode first.")
	}
	refs := autosend.Normalize(args.Chats)
	if len(refs) == 0 {
		return no("name at least one chat: use c_N or p_N ids.")
	}
	profile := ProfileOf(server)
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
	r := autosend.Receipt{At: env.Now.Unix(), Profile: profile, TaskID: task.ID, TaskName: task.Title(), Schedule: task.Schedule(),
		Fingerprint: fp, Chats: chats}
	if err := autosend.WriteReceipt(env.Dir, autosend.ArgsKey(profile, args.Task, args.Chats), r); err != nil {
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
	return decision{ask, fmt.Sprintf("%slet the Desktop scheduled task %q (%s, mode %s) send text messages with nobody present to: %s. "+
		"Its prompt: %q. Approve only if you asked for this. It lasts until revoked or until the task's prompt, schedule or mode changes.",
		lead, display(task.Title()), display(task.Schedule()), display(mode), strings.Join(labels, ", "), displayN(prompt, 240))}
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
	switch kind {
	case "p":
		return personDestination(ctx, db, id)
	case "c":
		var chatKind, name, jid string
		var person sql.NullInt64
		err := db.QueryRowContext(ctx, `SELECT kind, person_id, name, jid FROM chats WHERE id = ?`, id).Scan(&chatKind, &person, &name, &jid)
		if err != nil {
			return autosend.Chat{}, fmt.Errorf("chat %s: %w", ref, err)
		}
		switch {
		case chatKind == "dm" && person.Valid:
			return personDestination(ctx, db, person.Int64)
		case chatKind == "group":
			if name == "" {
				name = "unnamed"
			}
			return autosend.Chat{JID: jid, Label: "group " + name + " (" + ref + ")"}, nil
		}
		return autosend.Chat{}, fmt.Errorf("chat %s is a %s: only people and groups receive messages", ref, chatKind)
	}
	return autosend.Chat{}, fmt.Errorf("%s is not a c_N or p_N id", display(ref))
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

// writeAudit appends the send to <AppDir>/<profile>/logs/autosend.log,
// readable by the user only. The send is allowed only once it is logged.
func writeAudit(env PreToolEnv, profile, session string, a *autoSend) error {
	if env.AppDir == "" {
		return errors.New("no WhatsApp MCP folder")
	}
	dir := filepath.Join(env.AppDir, profile, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, autosend.LogName)
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

// unattendedPrompt reports whether a prompt in a session Claude Desktop
// started for a scheduled task comes from the task: always, unless the
// session file shows the user opened the run and continued it.
func unattendedPrompt(roots []string, session string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), PreToolBudget)
	defer cancel()
	continued, ok := withBudget(ctx, func(ctx context.Context) bool {
		run, err := findRun(ctx, roots, session)
		return err == nil && run.Continued
	})
	return !ok || !continued
}
