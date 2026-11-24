package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/autosend"
	"github.com/vukappa4/whatsapp-mcp/internal/desktop"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

const (
	toolAutosendAllow  = autosend.ToolAllow
	toolAutosendRevoke = autosend.ToolRevoke
)

type autosendAllowIn struct {
	Task  string   `json:"task" jsonschema:"the taskId given to create_scheduled_task, or the task's exact name"`
	Chats []string `json:"chats" jsonschema:"c_N or p_N ids the task may send to"`
}

type autosendRevokeIn struct {
	Task string `json:"task" jsonschema:"the task's id or name"`
}

func (s *Server) addAutosendTools(srv *mcp.Server) {
	f, t := false, true
	addTool(srv, &mcp.Tool{
		Name: toolAutosendAllow,
		Description: "Let one Claude Desktop scheduled task send WhatsApp text messages to a fixed set of chats with nobody present. " +
			"Call it only in a session with the user, right after creating the task, when the user asked for it. task: the taskId " +
			"given to create_scheduled_task. chats: c_N or p_N ids; write the same ids in the task's prompt, since its runs must " +
			"call whatsapp_send_message with to set to one of them. The user confirms a card showing the task, schedule, prompt and " +
			"chats; a scheduled run can never approve itself. Any other send, file or chat still asks. Changing the task's prompt, " +
			"schedule or permission mode ends the approval; approving again replaces the chats. Never bulk or unsolicited: " +
			"WhatsApp bans for it.",
		InputSchema: schemaFor[autosendAllowIn](nil),
		Annotations: &mcp.ToolAnnotations{Title: "Approve WhatsApp autosend", DestructiveHint: &f, OpenWorldHint: &f},
	}, s.autosendAllow)

	addTool(srv, &mcp.Tool{
		Name:        toolAutosendRevoke,
		Description: "End a scheduled task's autosend approval (task id or name); no confirmation needed. whatsapp_status lists approvals.",
		InputSchema: schemaFor[autosendRevokeIn](nil),
		Annotations: &mcp.ToolAnnotations{Title: "Revoke WhatsApp autosend", DestructiveHint: &t, IdempotentHint: true, OpenWorldHint: &f},
	}, s.autosendRevoke)
}

func (s *Server) autosendAllow(ctx context.Context, _ *mcp.CallToolRequest, in autosendAllowIn) (*mcp.CallToolResult, error) {
	ids := autosend.Normalize(in.Chats)
	if strings.TrimSpace(in.Task) == "" || len(ids) == 0 {
		return errResult("Not approved: task and at least one chat (c_N or p_N) are required.")
	}
	notApproved := fmt.Sprintf("Not approved: the send gate did not ask the user for exactly these arguments (task %q, chats %s). "+
		"If the user approved a card, the program the send gate runs is older than this daemon: update it (prod), restart "+
		"Claude Code, and call again.", strings.TrimSpace(in.Task), strings.Join(ids, ", "))
	if s.HooksDir == "" {
		return errResult("%s", notApproved)
	}
	r, err := autosend.TakeReceipt(s.HooksDir, autosend.ArgsKey(s.Profile, in.Task, in.Chats), s.now())
	switch {
	case errors.Is(err, autosend.ErrNoReceipt):
		return errResult("%s", notApproved)
	case err != nil:
		return errResult("Not approved: cannot read the send gate's receipt: %v", err)
	case r.Profile != s.Profile:
		return errResult("Not approved: the user approved this for the %s profile, and this server is %s.", r.Profile, s.Profile)
	case r.TaskID == "" || len(r.Chats) == 0:
		return errResult("%s", notApproved)
	}
	rule := store.AutosendRule{TaskID: r.TaskID, TaskName: r.TaskName, Schedule: r.Schedule, Fingerprint: r.Fingerprint, ApprovedAt: r.At}
	for _, c := range r.Chats {
		rule.Chats = append(rule.Chats, store.AutosendChat{JID: c.JID, Label: c.Label})
	}
	if err := store.PutAutosendRule(ctx, s.DB, rule); err != nil {
		return nil, err
	}
	w := newWrapper(s.now())
	bw := w.inner()
	lines := []string{fmt.Sprintf("task %s (%s), id %s", bw.wrap(r.TaskName), bw.wrap(r.Schedule), bw.wrap(r.TaskID))}
	for _, c := range r.Chats {
		lines = append(lines, "to "+bw.wrap(c.Label))
	}
	return textResult(fmt.Sprintf("Approved: this Desktop scheduled task may send text messages with nobody present:\n%s\n"+
		"Its runs must call %s with to set to one of: %s. Changing the task's prompt, schedule or permission mode ends the "+
		"approval. %s ends it now.", w.block(lines), toolSendMessage, strings.Join(ids, ", "), toolAutosendRevoke)), nil
}

func (s *Server) autosendRevoke(ctx context.Context, _ *mcp.CallToolRequest, in autosendRevokeIn) (*mcp.CallToolResult, error) {
	if strings.TrimSpace(in.Task) == "" {
		return errResult("task is required: the scheduled task's id or name")
	}
	r, err := store.DeleteAutosendRule(ctx, s.DB, in.Task)
	switch {
	case errors.Is(err, store.ErrNoRule):
		return errResult("No autosend approval for task %q: whatsapp_status lists approvals.", strings.TrimSpace(in.Task))
	case err != nil && strings.Contains(err.Error(), "use the task id"):
		return errResult("%v", err)
	case err != nil:
		return nil, err
	}
	w := newWrapper(s.now())
	return textResult(fmt.Sprintf("Revoked: the Desktop scheduled task %s (id %s) can no longer send without asking.",
		w.wrap(r.TaskName), w.wrap(r.TaskID))), nil
}

// autosendLogLines is how many of the newest autosend log lines whatsapp_status shows.
const autosendLogLines = 5

// autosendStatus renders the approvals, each with its state, and the newest
// lines of the autosend log, in one untrusted block: labels hold names
// other people chose.
func (s *Server) autosendStatus(ctx context.Context, now time.Time) (string, error) {
	rules, err := store.AutosendRules(ctx, s.DB)
	if err != nil {
		return "", err
	}
	logLines := tailLines(filepath.Join(s.DataDir, "logs", autosend.LogName), autosendLogLines)
	if len(rules) == 0 && len(logLines) == 0 {
		return "Autosend: no Desktop scheduled task may send without asking", nil
	}
	w := newWrapper(now)
	bw := w.inner()
	var lines []string
	for _, r := range rules {
		labels := make([]string, len(r.Chats))
		for i, c := range r.Chats {
			labels[i] = bw.wrap(c.Label)
		}
		lines = append(lines, fmt.Sprintf("task %s (%s), id %s, approved %s, %s; to %s", bw.wrap(r.TaskName), bw.wrap(r.Schedule),
			bw.wrap(r.TaskID), fmtTS(r.ApprovedAt, now), s.autosendState(ctx, r), strings.Join(labels, ", ")))
	}
	for _, l := range logLines {
		var e struct {
			At        string `json:"at"`
			TaskName  string `json:"task_name"`
			Label     string `json:"label"`
			TextChars int    `json:"text_chars"`
		}
		if json.Unmarshal([]byte(l), &e) != nil {
			continue
		}
		at := e.At
		if t, err := time.Parse(time.RFC3339, e.At); err == nil {
			at = fmtTS(t.Unix(), now)
		}
		lines = append(lines, fmt.Sprintf("sent %s by task %s to %s, %d characters", at, bw.wrap(e.TaskName), bw.wrap(e.Label), e.TextChars))
	}
	return fmt.Sprintf("Autosend (%d approved Desktop scheduled tasks; then the newest sends of the log %s):\n%s",
		len(rules), filepath.Join(s.DataDir, "logs", autosend.LogName), w.block(lines)), nil
}

// autosendState tells whether a rule still lets its task send: the task
// must exist with the fingerprint it was approved with.
func (s *Server) autosendState(ctx context.Context, r store.AutosendRule) string {
	task, err := desktop.FindTaskByID(ctx, s.DesktopRoots, r.TaskID)
	if errors.Is(err, desktop.ErrNotFound) {
		return "inactive: task not found"
	}
	if err != nil {
		return "unknown: " + err.Error()
	}
	fp, err := desktop.Fingerprint(task)
	switch {
	case err != nil:
		return "unknown: " + err.Error()
	case fp != r.Fingerprint:
		return "inactive: the task's prompt, schedule or mode changed since approval"
	}
	return "active"
}

// tailLines returns the last n lines of the file at path, nil when it
// cannot be read. It reads at most the last 64 KB.
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(st.Size()-64<<10, 0)
	b := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		return nil
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // cut mid-line
	}
	var out []string
	for _, l := range lines[max(len(lines)-n, 0):] {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			out = append(out, string(l))
		}
	}
	return out
}
