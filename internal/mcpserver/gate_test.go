package mcpserver

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/claudecode"
	"github.com/vukappa4/whatsapp-mcp/internal/hooks"
)

// firstMatcher is the PreToolUse matcher the first release registered.
// update replaces the binary but never the settings.json that setup-claude
// wrote, so settings files keep it.
const firstMatcher = `mcp__[A-Za-z0-9_-]*[Ww][Hh][Aa][Tt][Ss][Aa][Pp][Pp][A-Za-z0-9_-]*__(whatsapp_send_.*|whatsapp_request_history|send_message|send_file|send_audio_message)`

// Every tool is either a read tool, read-only and allowed by name
// (claudecode.ReadTools), or a tool that acts, gated by the current matcher
// and by the first release's: a new send tool is named whatsapp_send_*, or
// it runs ungated for every user who does not run setup-claude again.
func TestEveryActingToolIsGatedByEveryMatcher(t *testing.T) {
	e := newSendEnv(t)
	tools, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	matchers := map[string]*regexp.Regexp{
		"first":   regexp.MustCompile(`^(?:` + firstMatcher + `)$`),
		"current": regexp.MustCompile(`^(?:` + hooks.PreToolMatcher + `)$`),
	}
	for _, tl := range tools.Tools {
		read := tl.Annotations != nil && tl.Annotations.ReadOnlyHint
		if allowed := slices.Contains(claudecode.ReadTools, tl.Name); allowed != read {
			t.Errorf("%s: read-only %v but allowed without asking %v", tl.Name, read, allowed)
		}
		for _, server := range []string{"whatsapp", "whatsapp-dev"} {
			name := "mcp__" + server + "__" + tl.Name
			for which, m := range matchers {
				if gated := m.MatchString(name); gated == read {
					t.Errorf("%s: read-only %v but gated by the %s matcher %v", name, read, which, gated)
				}
			}
		}
	}
}
