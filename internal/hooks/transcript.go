package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// transcriptTail is how much of the end of a session transcript is read
// to find the last prompt.
const transcriptTail = 8 << 20

// transcriptSlack is how much later than its flag a prompt may be recorded
// in the transcript and still be the prompt the flag was written for.
const transcriptSlack = 5 * time.Second

// typedPrompt is the last prompt the user typed, as the session transcript
// records it.
type typedPrompt struct {
	Text string
	At   time.Time
}

// lastUserPrompt finds the newest prompt typed by the user in a Claude Code
// session transcript (JSONL): a "user" entry whose content is text, not a
// tool result, not meta and not a subagent's. ok is false when there is
// none or the file cannot be read.
func lastUserPrompt(path string) (typedPrompt, bool) {
	if path == "" {
		return typedPrompt{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return typedPrompt{}, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return typedPrompt{}, false
	}
	off := max(st.Size()-transcriptTail, 0)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return typedPrompt{}, false
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // cut mid-line
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if p, ok := promptEntry(lines[i]); ok {
			return p, true
		}
	}
	return typedPrompt{}, false
}

// promptEntry reads one transcript line as a typed user prompt.
func promptEntry(line []byte) (typedPrompt, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || !bytes.Contains(line, []byte(`"user"`)) {
		return typedPrompt{}, false
	}
	var e struct {
		Type        string `json:"type"`
		IsMeta      bool   `json:"isMeta"`
		IsSidechain bool   `json:"isSidechain"`
		Timestamp   string `json:"timestamp"`
		Message     struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "user" || e.IsMeta || e.IsSidechain || e.Message.Role != "user" {
		return typedPrompt{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return typedPrompt{}, false
	}
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		// "!" shell commands and their output are recorded as user entries
		// but are no prompt (no UserPromptSubmit hook runs for them).
		if t := strings.TrimSpace(s); strings.HasPrefix(t, "<bash-") || strings.HasPrefix(t, "<local-command") {
			return typedPrompt{}, false
		}
		return typedPrompt{Text: s, At: at}, s != ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		return typedPrompt{}, false
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			return typedPrompt{}, false // a tool's answer, not the user
		case "text":
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return typedPrompt{}, false
	}
	return typedPrompt{Text: strings.Join(parts, "\n"), At: at}, true
}

// flagMatchesTranscript reports whether the flag belongs to the user's
// latest prompt. A transcript prompt newer than the flag with other words
// means the UserPromptSubmit hook did not run for it (could not start,
// timed out): the flag is an earlier turn's. A transcript that cannot be
// read, or whose newest prompt is not newer than the flag (written after
// the hook ran), gives no reason to doubt it.
func flagMatchesTranscript(fl flag, transcriptPath string) bool {
	p, ok := lastUserPrompt(transcriptPath)
	if !ok || !p.At.After(time.Unix(fl.At, 0).Add(transcriptSlack)) {
		return true
	}
	ws := words(p.Text, true)
	if len(ws) > maxPromptWords {
		ws = ws[:maxPromptWords]
	}
	return strings.Join(ws, " ") == fl.Prompt
}
