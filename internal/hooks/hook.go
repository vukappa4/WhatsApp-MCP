package hooks

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// decodeInput reads the hook's JSON input, skipping a UTF-8 byte order mark:
// Windows PowerShell 5.1 prepends one when it pipes text with $OutputEncoding UTF-8.
// raw is the input as read, for a lenient look at a session id.
func decodeInput(r io.Reader, v any) (raw []byte, err error) {
	raw, err = io.ReadAll(io.LimitReader(bufio.NewReader(r), 4<<20))
	if err != nil {
		return raw, err
	}
	return raw, json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF")), v)
}

// FlagTTL is how long a send intent stays valid without a new prompt.
const FlagTTL = 2 * time.Hour

// PreToolBudget bounds the PreToolUse hook: past it the hook asks. The
// registered hook timeout (10 s) is well above it.
const PreToolBudget = 4 * time.Second

// PreToolMatcher is the PreToolUse matcher registered for the gate: the send
// tools and the history request of every MCP server whose name contains
// "whatsapp" in any case. Matchers are case-sensitive regular expressions,
// so the name is spelled letter by letter in both cases.
const PreToolMatcher = `mcp__[A-Za-z0-9_-]*[Ww][Hh][Aa][Tt][Ss][Aa][Pp][Pp][A-Za-z0-9_-]*__(whatsapp_send_.*|whatsapp_request_history|send_message|send_file|send_audio_message)`

// legacyTools are the send tools of the old Python server, gated too.
var legacyTools = map[string]bool{"send_message": true, "send_file": true, "send_audio_message": true}

// gatedTool splits an MCP tool name (mcp__<server>__<tool>) and reports
// whether it sends: any server whose name contains "whatsapp" (prod, dev or
// the legacy bridge), tools whatsapp_send_*, whatsapp_request_history and
// the legacy send tools.
func gatedTool(name string) (server, tool string, gated bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	i := strings.LastIndex(rest, "__")
	if !ok || i < 0 || !strings.Contains(strings.ToLower(rest[:i]), "whatsapp") {
		return "", "", false
	}
	server, tool = rest[:i], rest[i+2:]
	return server, tool, strings.HasPrefix(tool, "whatsapp_send_") || tool == "whatsapp_request_history" || legacyTools[tool]
}

// DefaultDir is <AppDir>/hooks, shared by every profile.
func DefaultDir() (string, error) {
	root, err := config.AppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "hooks"), nil
}

// promptInput is the part of the UserPromptSubmit input the hook reads.
type promptInput struct {
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
}

// toolInput is the part of the PreToolUse input the hook reads.
type toolInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// flagPath maps a session id to its flag file; ids that are not plain
// file names are hashed.
func flagPath(dir, session string) string {
	if !safeID.MatchString(session) {
		sum := sha256.Sum256([]byte(session))
		session = "h" + hex.EncodeToString(sum[:16])
	}
	return filepath.Join(dir, session)
}

// flag is the content of a session's flag file: when the user's prompt asked
// to send, and that prompt folded to words, so the PreToolUse hook can check
// that the recipient and the file were named in it.
type flag struct {
	At     int64  `json:"at"`
	Prompt string `json:"prompt"`
}

// maxPromptWords caps the prompt stored in a flag.
const maxPromptWords = 20000

var reSessionID = regexp.MustCompile(`"session_id"\s*:\s*"([^"\\]{1,200})"`)

// UserPrompt handles UserPromptSubmit: it writes the session's flag (the
// time and the folded prompt, readable by the user only) when the prompt
// asks to send, and removes it otherwise. Any failure removes the flag when
// the session is known, even from input that is not valid JSON: an old
// intent never survives a prompt. It never writes to stdout: Claude Code
// adds a UserPromptSubmit hook's stdout to the context.
func UserPrompt(stdin io.Reader, dir string, now time.Time) error {
	var in promptInput
	raw, err := decodeInput(stdin, &in)
	if err != nil {
		if m := reSessionID.FindSubmatch(raw); m != nil {
			_ = os.Remove(flagPath(dir, string(m[1])))
		}
		return fmt.Errorf("read hook input: %w", err)
	}
	if in.SessionID == "" {
		return errors.New("hook input has no session_id")
	}
	path := flagPath(dir, in.SessionID)
	err = writeFlag(dir, path, in.Prompt, now)
	if err != nil {
		_ = os.Remove(path) // never keep an old intent
	}
	return err
}

func writeFlag(dir, path, prompt string, now time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := restrictToUser(dir, true); err != nil {
		return fmt.Errorf("restrict %s to the user: %w", dir, err)
	}
	pruneFlags(dir, now)
	if !SendIntent(prompt) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	ws := words(prompt, true)
	if len(ws) > maxPromptWords {
		ws = ws[:maxPromptWords]
	}
	b, err := json.Marshal(flag{At: now.Unix(), Prompt: strings.Join(ws, " ")})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := restrictToUser(tmp, false); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("restrict %s to the user: %w", tmp, err)
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// pruneFlags removes flags of sessions that have expired.
func pruneFlags(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if _, ok := readFlag(filepath.Join(dir, e.Name()), now); ok {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// readFlag returns the flag when the file exists, parses and was written
// within FlagTTL; anything else (missing, corrupt, old, from the future) is
// no flag.
func readFlag(path string, now time.Time) (flag, bool) {
	var f flag
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 1<<20 || json.Unmarshal(b, &f) != nil || f.At == 0 {
		return flag{}, false
	}
	age := now.Sub(time.Unix(f.At, 0))
	return f, age >= -time.Minute && age <= FlagTTL
}

// preToolOutput is the PreToolUse decision JSON.
type preToolOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// PreToolEnv is what the PreToolUse hook reads besides its input.
type PreToolEnv struct {
	Dir      string    // flag directory
	Now      time.Time // the current time
	Resolver Resolver  // maps a tool's recipient to the names it is known by
}

// PreTool handles PreToolUse for the gated tools. It stays silent (Claude
// Code's normal permission flow decides) only when the user's last prompt
// asked to send, named the recipient and, for a file, mentioned it;
// otherwise it prints an "ask" decision naming what to confirm. Input it
// cannot read, a missing or stale flag, a recipient it cannot resolve and a
// check that overruns PreToolBudget all ask: the gate fails closed.
func PreTool(stdin io.Reader, stdout io.Writer, env PreToolEnv) error {
	var in toolInput
	_, err := decodeInput(stdin, &in)
	if _, _, gated := gatedTool(in.ToolName); err == nil && !gated {
		return nil
	}
	reason := askReason(in, err, env)
	if reason == "" {
		return nil
	}
	var out preToolOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = "ask"
	out.HookSpecificOutput.PermissionDecisionReason = reason
	return json.NewEncoder(stdout).Encode(out)
}

// toolArgs are the arguments the gate reads from any gated tool.
type toolArgs struct {
	To        string `json:"to"`
	Recipient string `json:"recipient"` // legacy tools
	Chat      string `json:"chat"`      // whatsapp_request_history
	Path      string `json:"path"`
	MediaPath string `json:"media_path"` // legacy tools
}

// askReason returns why the call needs the user's confirmation, "" when it
// does not.
func askReason(in toolInput, inputErr error, env PreToolEnv) string {
	var args toolArgs
	_ = json.Unmarshal(in.ToolInput, &args)
	server, tool, _ := gatedTool(in.ToolName)
	target := args.To
	if target == "" {
		target = args.Recipient
	}
	if tool == "whatsapp_request_history" {
		target = args.Chat
	}
	what := "this send to " + display(target)
	if tool == "whatsapp_request_history" {
		what = "this history request for " + display(target)
	}
	const lead = "WhatsApp: "
	if inputErr != nil || in.SessionID == "" || env.Dir == "" {
		return lead + "the hook could not read this call. Confirm " + what + "."
	}
	r, rerr := resolve(env.Resolver, ProfileOf(server), target)
	if rerr == nil && r.Label != "" {
		what = strings.Replace(what, display(target), display(r.Label), 1)
	}
	fl, ok := readFlag(flagPath(env.Dir, in.SessionID), env.Now)
	if !ok {
		return lead + "your last message did not ask to send anything. Confirm " + what + "."
	}
	if !flagMatchesTranscript(fl, in.TranscriptPath) {
		return lead + "the send gate did not check your last message (its hook did not run). Confirm " + what + "."
	}
	if rerr != nil {
		return lead + "cannot check the recipient " + display(target) + " (" + display(rerr.Error()) + "). Confirm " + what + "."
	}
	prompt := strings.Fields(fl.Prompt)
	if !r.NamedIn(prompt) {
		return lead + "your last message does not name " + display(r.Label) +
			" (nicknames are not known to the check). Confirm " + what + "."
	}
	if path := firstNonEmpty(args.Path, args.MediaPath); tool == "whatsapp_send_file" || tool == "send_file" || tool == "send_audio_message" {
		if !FileNamedIn(path, prompt) {
			return lead + "your last message does not mention a file. Confirm sending " + display(filepath.Base(path)) +
				" in " + what + "."
		}
	}
	return ""
}

// resolve runs the resolver within PreToolBudget.
func resolve(res Resolver, profile, target string) (Recipient, error) {
	if res == nil {
		return Recipient{}, errors.New("no resolver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), PreToolBudget)
	defer cancel()
	type result struct {
		r   Recipient
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := res.Resolve(ctx, profile, target)
		done <- result{r, err}
	}()
	select {
	case x := <-done:
		return x.r, x.err
	case <-ctx.Done():
		return Recipient{}, errors.New("lookup timed out")
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// display cleans a model-written or sender-chosen value for the permission prompt.
func display(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	if s == "" {
		return "an unnamed recipient"
	}
	return s
}
