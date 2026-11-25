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

// decide answers one gated call:
//
//  1. Revoking an autosend approval only narrows what may be sent: allow.
//  2. An autosend approval always shows the user a card (ask), or is denied
//     when it cannot be shown truthfully (approvalCard). Never silent.
//  3. In a session Claude Desktop started for a scheduled task
//     (CLAUDE_CODE_HOST_SCHEDULED_RUN=1), a person is present once the
//     UserPromptSubmit hook has seen a prompt after the first, which is the
//     task's SKILL.md (scheduledPrompt). With nobody present, whatever
//     Desktop's file says about the run:
//  4. a whatsapp_send_message, in a call whose own permission mode shows
//     prompts, of a task that is approved, unchanged since, in a mode that
//     shows prompts, to an approved chat, less than autosend.MaxPerDay times
//     a day: allow, once the autosend log holds it;
//  5. anything else: deny, with a reason Claude can act on. Nobody is there
//     to answer an ask, and Desktop can store a task's answer to an ask and
//     replay it, past the chat check and the brake; a deny is never stored.
//  6. Otherwise, a person typed or the session is not a scheduled run: the
//     user-prompt gate decides (askReason). In a scheduled session it asks
//     at most: Claude can write the marker and the flag it trusts.
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
	switch {
	case env.Scheduled && inputErr != nil:
		l = lookup{unattended: true, why: "the hook could not read this call"}
	case env.Scheduled && !personTyped(env.Dir, in.SessionID):
		l = lookupRun(in, server, tool, env)
	}
	if a := l.allow; a != nil {
		l.label = a.label
		if why := logAutosend(env, profile, in.SessionID, a); why != "" {
			l.why = why
		} else {
			return decision{allow, fmt.Sprintf("WhatsApp autosend: task %s approved %s sends to %s.", display(a.taskName),
				time.Unix(a.approvedAt, 0).Format("2006-01-02"), display(a.label))}
		}
	}
	if l.unattended {
		why := l.why
		if why == "" {
			why = "only " + sendTool + " to a chat approved for the task goes out with nobody present"
		}
		// No recipient lookup here: the deny must come within the hook's timeout.
		return decision{deny, "WhatsApp: this scheduled run has nobody present and is not approved for " + callWhat(in, tool, l.label) +
			" (" + displayN(why, 400) + "). Do not retry; tell the user in your final message."}
	}
	r, what := askReason(in, inputErr, env)
	if r == "" && env.Scheduled {
		// The typed marker and the flag sit in a folder Claude can write to:
		// in a scheduled session they never make a send silent.
		r = "WhatsApp: this session runs a Desktop scheduled task. Confirm " + what + "."
	}
	if r != "" {
		return decision{ask, r}
	}
	return decision{}
}

// callWhat names a call for a reason without looking anything up: the
// label an approval stored for its recipient, else the recipient as given.
func callWhat(in toolInput, tool, label string) string {
	var args toolArgs
	_ = json.Unmarshal(in.ToolInput, &args)
	target := firstNonEmpty(label, args.To, args.Recipient)
	if tool == "whatsapp_request_history" {
		return "this history request for " + display(firstNonEmpty(label, args.Chat))
	}
	return "this send to " + display(target)
}

// logAutosend holds the autosend log's lock while it counts the task's
// sends to the chat and appends this one, so two hooks at once cannot both
// pass the brake. It returns why the send cannot go, "" once it is logged.
func logAutosend(env PreToolEnv, profile, session string, a *autoSend) string {
	path, err := auditPath(env, profile)
	if err != nil {
		return errText(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "cannot write the autosend log (" + errText(err) + ")"
	}
	unlock, err := lockFile(path+".lock", auditLockWait)
	if err != nil {
		return "cannot lock the autosend log (" + errText(err) + ")"
	}
	defer unlock()
	n, err := countAutosends(env, profile, a.taskID, a.jid)
	switch {
	case err != nil:
		return "cannot read the autosend log (" + errText(err) + ")"
	case n >= autosend.MaxPerDay:
		return fmt.Sprintf("the task reached %d autosends to %s in 24 hours", autosend.MaxPerDay, display(a.label))
	}
	if err := writeAudit(env, profile, session, a); err != nil {
		return "cannot write the autosend log (" + errText(err) + ")"
	}
	return ""
}

// auditLockWait is the longest the gate waits for the autosend log's lock.
const auditLockWait = 2 * time.Second

// lockFile takes an exclusive operating system lock on the file at path,
// retrying until wait ends. The system drops the lock when unlock closes
// the file or the process dies, so a killed hook leaves no stale lock. The
// file stays: deleting it while held would let another hook lock a new
// file of the same name.
func lockFile(path string, wait time.Duration) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		held, err := tryLock(f)
		switch {
		case err != nil:
			f.Close()
			return nil, err
		case !held:
			return func() { f.Close() }, nil
		case time.Now().After(deadline):
			f.Close()
			return nil, errors.New("another hook holds it")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// RunWaitFor is how long the gate may wait for Desktop to save a scheduled
// run's session file under a PreToolUse registration timeout: whatever
// leaves 3 s to spare after the autosend check and the log's lock, at most
// maxRunWait. Desktop writes the file 3 s after its last change, 9 s at most.
func RunWaitFor(timeout time.Duration) time.Duration {
	return min(max(timeout-3*time.Second-PreToolBudget-auditLockWait, 0), maxRunWait)
}

// maxRunWait is the wait under the registered 30 s timeout.
const maxRunWait = 12 * time.Second

// runPoll is how often the gate looks for a run's session file.
var runPoll = 250 * time.Millisecond

// waitForRun finds the session's run file, looking again every runPoll
// for up to wait: Desktop saves the file some seconds after the run starts,
// and a file caught mid-write reads as broken. The last error stands when
// the wait ends. No session id, or a session two files name, is final: it
// returns at once.
func waitForRun(roots []string, session string, wait time.Duration) (desktop.Run, error) {
	if strings.TrimSpace(session) == "" {
		return desktop.Run{}, errors.New("the hook input has no session id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	type result struct {
		run desktop.Run
		err error
	}
	last := fmt.Errorf("Claude Desktop's session files did not show this run within %s", wait)
	for {
		res, ok := withBudget(ctx, func(ctx context.Context) result {
			run, err := findRun(ctx, roots, session)
			return result{run, err}
		})
		switch {
		case !ok:
			return desktop.Run{}, last
		case res.err == nil:
			return res.run, nil
		case errors.Is(res.err, desktop.ErrAmbiguous):
			return desktop.Run{}, res.err
		case errors.Is(res.err, desktop.ErrNotFound):
			last = fmt.Errorf("Claude Desktop had not saved this run's session file after %s", wait)
		default:
			last = res.err
		}
		select {
		case <-ctx.Done():
			return desktop.Run{}, last
		case <-time.After(runPoll):
		}
	}
}

// errText is an internal error as a reason shows it.
func errText(err error) string { return displayN(err.Error(), 300) }

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
	label      string    // the label an approval stored for the recipient, when known
	allow      *autoSend // an approved autosend, to check and log before allowing
}

// autoSend is one send an approved task may make.
type autoSend struct {
	taskID, taskName string
	approvedAt       int64
	to, jid, label   string
	textChars        int
}

// lookupRun runs step 4 of decide with nobody present: the run's file,
// then within PreToolBudget the autosend check. Other tools need neither.
func lookupRun(in toolInput, server, tool string, env PreToolEnv) lookup {
	if tool != sendTool {
		return lookup{unattended: true}
	}
	// The mode Claude Code reports for this very call: settings the gate does
	// not read, such as managed ones, can override the task's.
	if why := autosend.ModeProblem("this run", in.PermissionMode); why != "" {
		return lookup{unattended: true, why: why}
	}
	run, runErr := waitForRun(env.DesktopRoots, in.SessionID, env.RunWait)
	ctx, cancel := context.WithTimeout(context.Background(), PreToolBudget)
	defer cancel()
	l, ok := withBudget(ctx, func(ctx context.Context) lookup {
		l := lookup{unattended: true}
		if l.why = autosendCheck(ctx, in, server, env, run, runErr, &l); l.why != "" {
			l.allow = nil
		}
		return l
	})
	if !ok {
		return lookup{unattended: true, why: "the autosend check timed out"}
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
		return "cannot find this run in Claude Desktop's files (" + errText(runErr) + ")"
	case run.TaskID == "":
		return "this run names no scheduled task"
	}
	args, err := strictSendArgs(in.ToolInput)
	if err != nil {
		return errText(err)
	}
	to := strings.TrimSpace(args.To)
	if _, kind := parseRef(to); kind == "" {
		return "to must be an approved c_N or p_N id, not " + display(to)
	}
	task, err := desktop.TaskByID(run.Dir, run.TaskID)
	if err != nil {
		return "task " + display(run.TaskID) + ": " + errText(err)
	}
	if why := autosend.TaskModeProblem(desktop.EffectiveMode(task, env.ClaudeDir)); why != "" {
		return why
	}
	db, err := openProfileDB(env.AppDir, profile)
	if err != nil {
		return errText(err)
	}
	defer db.Close()
	rule, err := store.AutosendRuleFor(ctx, db, task.ID)
	if errors.Is(err, store.ErrNoRule) {
		return "task " + display(task.Title()) + " is not approved"
	}
	if err != nil {
		return "cannot read the approvals (" + errText(err) + ")"
	}
	for _, c := range rule.Chats {
		if c.Ref == to {
			l.label = c.Label
		}
	}
	if !desktop.SamePath(rule.TaskDir, run.Dir) {
		return "this run's task is in another Claude Desktop account or organization than the approved one"
	}
	file, _, err := desktop.ReadSkill(task)
	if err != nil {
		return errText(err)
	}
	fp, err := desktop.FingerprintOf(task, file)
	if err != nil {
		return errText(err)
	}
	if fp != rule.Fingerprint {
		return "the task's prompt, schedule or folder changed since it was approved"
	}
	dest, err := destination(ctx, db, to)
	if err != nil {
		return errText(err)
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
func approvalCard(ctx context.Context, in toolInput, inputErr error, profile string, env PreToolEnv) decision {
	const lead = "WhatsApp autosend: "
	no := func(why string) decision { return decision{deny, lead + why} }
	var args autosend.Args
	if inputErr != nil || in.SessionID == "" || json.Unmarshal(in.ToolInput, &args) != nil {
		return no("the hook could not read this call.")
	}
	const scheduledRun = "an approval can only be given in a session with you, never by a scheduled run."
	if env.Scheduled {
		return no(scheduledRun)
	}
	// CLAUDE_CODE_HOST_SCHEDULED_RUN is the check. Desktop's file for the
	// session is a best-effort second one, in case the variable did not
	// reach the hook: a file naming a task refuses. No file found must pass,
	// since an interactive CLI session has none; so this check misses a run
	// whose file Desktop has not written yet. It does not wait for one: that
	// would delay every interactive approval.
	switch run, err := findRun(ctx, env.DesktopRoots, in.SessionID); {
	case err == nil && run.TaskID != "":
		return no(scheduledRun)
	case err != nil && !errors.Is(err, desktop.ErrNotFound):
		return no("cannot tell whether this session is a scheduled run (" + errText(err) + "). Call again.")
	}
	if !autosend.PromptMode(in.PermissionMode) {
		return no(autosend.ModeProblem("this session", in.PermissionMode) + ", which may settle the approval without showing it to you: " +
			"switch to default, acceptEdits, plan or auto mode, then call again.")
	}
	task, err := findTask(ctx, env.DesktopRoots, args.Task)
	switch {
	case errors.Is(err, desktop.ErrNotFound):
		return no("no Claude Desktop scheduled task has the id or title " + strconv.Quote(display(args.Task)) +
			": create it first, then call again with the taskId given to create_scheduled_task.")
	case err != nil:
		return no("cannot find the task (" + errText(err) + ").")
	}
	file, skill, err := desktop.ReadSkill(task)
	if err != nil {
		return no("cannot read what the task runs (" + errText(err) + ").")
	}
	fp, err := desktop.FingerprintOf(task, file)
	if err != nil {
		return no("cannot read what the task runs (" + errText(err) + ").")
	}
	mode, modeErr := desktop.EffectiveMode(task, env.ClaudeDir)
	if why := autosend.TaskModeProblem(mode, modeErr); why != "" {
		return no(why + ": a run must use default, acceptEdits, plan or auto mode, where it cannot settle confirmations " +
			"without you or rewrite the send gate. Give the task one of them first.")
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
		return no(errText(err) + ".")
	}
	defer db.Close()
	var chats []autosend.Chat
	for _, ref := range refs {
		c, err := destination(ctx, db, ref)
		if err != nil {
			return no("cannot approve " + display(ref) + ": " + errText(err) + ". Use c_N or p_N ids.")
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
		return no("cannot record the approval (" + errText(err) + ").")
	}
	labels := make([]string, len(chats))
	for i, c := range chats {
		labels[i] = display(c.Label)
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
		"Its whole prompt: %q.%s Approve only if you asked for this. It lasts until revoked or until the task's prompt, schedule "+
		"or folder changes, and allows at most %d sends a day to each chat.",
		lead, display(task.Title()), display(task.Schedule()), display(autosend.ShowTaskMode(mode)), folder, strings.Join(labels, ", "),
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

// destination is where a send to ref goes, exactly as the server resolves
// it (store.ResolveDestination). Only c_N and p_N ids: names and numbers
// are refused, since another person can take a name or a number.
func destination(ctx context.Context, db *sql.DB, ref string) (autosend.Chat, error) {
	ref = strings.TrimSpace(ref)
	if kind, _ := store.ParseRef(ref); kind == "" {
		return autosend.Chat{}, fmt.Errorf("%s is not a c_N or p_N id", display(ref))
	}
	d, err := store.ResolveDestination(ctx, db, ref)
	if err != nil {
		return autosend.Chat{}, err
	}
	return autosend.Chat{Ref: ref, JID: d.JID, Label: d.Label}, nil
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

// scheduledDir holds, per scheduled session, a marker that its first
// prompt was seen and another once a later prompt was. It is a folder,
// which pruneFlags leaves alone.
func scheduledDir(dir string) string { return filepath.Join(dir, "scheduled") }

// markerTTL is how long a scheduled session's markers are kept.
const markerTTL = 30 * 24 * time.Hour

// typedPath is the marker of a person's prompt in a scheduled session.
func typedPath(dir, session string) string { return flagPath(scheduledDir(dir), session) + ".typed" }

// scheduledPrompt records a prompt of a session Claude Desktop started for
// a scheduled task and reports whether it is the first. The first is the
// task's SKILL.md, also when the user starts the run by hand ("Run now"):
// it is never the user's request. A later prompt is a person typing in the
// opened run: it is marked, and from then on the user-prompt gate decides.
// When it cannot tell, it says first and marks nothing: no flag is written,
// and nobody counts as present.
func scheduledPrompt(dir, session string, now time.Time) (first bool) {
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
	if err == nil {
		f.Close()
		return true
	}
	if !errors.Is(err, fs.ErrExist) {
		return true
	}
	if t, err := os.OpenFile(typedPath(dir, session), os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		if t.Close() == nil {
			return false
		}
	}
	return true
}

// personTyped reports whether a person typed in this scheduled session: the
// UserPromptSubmit hook marked a prompt after the first. A marker it cannot
// read means nobody.
func personTyped(dir, session string) bool {
	if dir == "" || session == "" {
		return false
	}
	st, err := os.Stat(typedPath(dir, session))
	return err == nil && st.Mode().IsRegular()
}
