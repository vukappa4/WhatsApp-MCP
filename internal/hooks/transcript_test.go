package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func userLine(text string, at time.Time) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": text}})
	return string(b) + "\n"
}

func assistantLine(pad int) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
		"content": []map[string]any{{"type": "text", "text": strings.Repeat("y", pad)}}}})
	return string(b) + "\n"
}

func writeTranscript(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func withChunk(t *testing.T, n int64) {
	t.Helper()
	old := transcriptChunk
	transcriptChunk = n
	t.Cleanup(func() { transcriptChunk = old })
}

// A large transcript: the newest prompt near its end is found, whatever
// chunk boundaries it straddles, and an older prompt or a tool result
// after it does not count.
func TestLastUserPromptLargeTranscript(t *testing.T) {
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	var b strings.Builder
	b.WriteString(userLine("old prompt", at))
	for b.Len() < 6<<20 {
		b.WriteString(assistantLine(4000))
	}
	b.WriteString(userLine("rispondi a Marco che arrivo", at.Add(time.Minute)))
	tool, _ := json.Marshal(map[string]any{"type": "user", "timestamp": at.Add(2 * time.Minute).Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": "ok"}}}})
	b.WriteString(string(tool) + "\n")
	b.WriteString(assistantLine(100))
	path := writeTranscript(t, b.String())
	// Chunk sizes that put a boundary inside the prompt line, on a newline, and around it.
	for _, n := range []int64{64 << 10, 4096, 1000, 97, 7, 1} {
		withChunk(t, n)
		p, ok := lastUserPrompt(path)
		if !ok || p.Text != "rispondi a Marco che arrivo" || !p.At.Equal(at.Add(time.Minute)) {
			t.Fatalf("chunk %d: %+v %v", n, p, ok)
		}
	}
}

// A prompt line split across two chunks is read whole.
func TestLastUserPromptStraddlesChunks(t *testing.T) {
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	tail := assistantLine(50)
	prompt := userLine("manda il file a Giulia", at)
	path := writeTranscript(t, assistantLine(5000)+prompt+tail)
	// The first chunk read ends halfway through the prompt line.
	withChunk(t, int64(len(tail)+len(prompt)/2))
	if p, ok := lastUserPrompt(path); !ok || p.Text != "manda il file a Giulia" {
		t.Fatalf("%+v %v", p, ok)
	}
	// A transcript of one line without a final newline.
	withChunk(t, 10)
	if p, ok := lastUserPrompt(writeTranscript(t, strings.TrimSuffix(prompt, "\n"))); !ok || p.Text != "manda il file a Giulia" {
		t.Fatalf("one line: %+v %v", p, ok)
	}
}

// Only the last transcriptTail bytes count: a prompt before them, or one
// whose line the limit cuts, is not found.
func TestLastUserPromptTailLimit(t *testing.T) {
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	prompt := userLine("old prompt", at)
	filler := assistantLine(transcriptTail)
	if p, ok := lastUserPrompt(writeTranscript(t, prompt+filler)); ok {
		t.Fatalf("prompt before the tail: %+v", p)
	}
	// The limit falls one byte into the prompt line: cut mid-line.
	pad := strings.Repeat("\n", transcriptTail-len(prompt)+1)
	if p, ok := lastUserPrompt(writeTranscript(t, prompt+pad)); ok {
		t.Fatalf("cut prompt: %+v", p)
	}
	// The line the limit cuts is dropped even when only a byte of it is
	// outside: the next line, the prompt, is found.
	if p, ok := lastUserPrompt(writeTranscript(t, "xx\n"+prompt+pad[3:])); !ok || p.Text != "old prompt" {
		t.Fatalf("prompt at the limit: %+v %v", p, ok)
	}
}
