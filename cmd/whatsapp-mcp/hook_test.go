package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/claudecode"
	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// tempAppDir points config.AppDir, in this process and in the returned
// environment for child processes, into dir/home on every OS.
func tempAppDir(t *testing.T, dir string) []string {
	home := filepath.Join(dir, "home")
	vars := map[string]string{"HOME": home, "LOCALAPPDATA": filepath.Join(home, "AppData", "Local"), "XDG_DATA_HOME": ""}
	env := os.Environ()
	for k, v := range vars {
		t.Setenv(k, v)
		env = append(env, k+"="+v)
	}
	return env
}

// The hooks run as the real binary, built like production, fed the JSON
// Claude Code sends, with the prod data.db of a temporary app dir:
// user-prompt prints nothing; pre-tool prints the "ask" decision on its
// stdout pipe unless the user's own prompt asked to send to the person the
// call names.
func TestHookSubcommandsEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, exeName)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	env := tempAppDir(t, dir)
	root, err := config.AppDir()
	if err != nil {
		t.Fatal(err)
	}
	prod := filepath.Join(root, "prod")
	if err := os.MkdirAll(prod, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(prod, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO persons(id, display_name, name_source, phone_e164, full_name, first_name, saved_contact, updated_at)
		VALUES (17, 'Marco Rossi', 'full_name', '+393331234567', 'Marco Rossi', 'Marco', 1, 0)`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	run := func(sub string, input map[string]any) (string, int) {
		t.Helper()
		b, _ := json.Marshal(input)
		cmd := exec.Command(bin, "hook", sub)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(string(b))
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("hook %s: %v", sub, err)
		}
		return string(out), code
	}
	pre := map[string]any{"session_id": "sess-1", "hook_event_name": "PreToolUse", "tool_name": "mcp__whatsapp__whatsapp_send_message",
		"tool_input": map[string]any{"to": "p_17", "text": "ciao"}, "tool_use_id": "toolu_1", "cwd": dir, "permission_mode": "default"}
	prompt := func(text string) map[string]any {
		return map[string]any{"session_id": "sess-1", "hook_event_name": "UserPromptSubmit", "prompt": text, "cwd": dir, "permission_mode": "default"}
	}
	reason := func(out string) string {
		t.Helper()
		var dec struct {
			HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &dec); err != nil || dec.HookSpecificOutput["permissionDecision"] != "ask" {
			t.Fatalf("not an ask decision: %q", out)
		}
		return dec.HookSpecificOutput["permissionDecisionReason"]
	}

	out, code := run("pre-tool", pre)
	if r := reason(out); code != 0 || !strings.Contains(r, "Confirm this send to Marco Rossi (+393331234567).") {
		t.Fatalf("pre-tool without intent: %q exit %d", r, code)
	}
	if out, code := run("user-prompt", prompt("rispondi a Marco che arrivo")); out != "" || code != 0 {
		t.Fatalf("user-prompt printed %q exit %d: Claude Code would add it to the context", out, code)
	}
	if _, err := os.Stat(filepath.Join(root, "hooks", "sess-1")); err != nil {
		t.Fatalf("flag file: %v", err)
	}
	if out, code := run("pre-tool", pre); out != "" || code != 0 {
		t.Fatalf("pre-tool with intent printed %q exit %d", out, code)
	}
	// Intent to send, but to someone else: ask, naming Marco.
	run("user-prompt", prompt("rispondi a Luca che arrivo"))
	if out, _ := run("pre-tool", pre); !strings.Contains(reason(out), "does not name Marco Rossi") {
		t.Fatalf("pre-tool for another recipient: %q", out)
	}
	if out, _ := run("user-prompt", prompt("cosa mi ha mandato Marco?")); out != "" {
		t.Fatalf("user-prompt printed %q", out)
	}
	if out, _ := run("pre-tool", pre); !strings.Contains(out, `"ask"`) {
		t.Fatalf("pre-tool after a prompt without intent: %q", out)
	}
	// A misregistered gate blocks instead of passing.
	if _, code := run("pre-toll", pre); code != 2 {
		t.Fatalf("unknown hook exit %d, want 2", code)
	}
}

// The hook subcommands are a permanent contract: update never rewrites the
// settings.json setup-claude wrote, so a binary must run the hooks every
// earlier version registered, hook user-prompt and hook pre-tool, and the
// ones this version registers. Both exit 0 on valid input; an unknown
// subcommand exits 2 and would block every prompt or send.
func TestRegisteredHookSubcommandsExist(t *testing.T) {
	dir := t.TempDir()
	tempAppDir(t, dir)
	file := filepath.Join(dir, "settings.json")
	if err := claudecode.Register(file, filepath.Join(dir, exeName), "whatsapp"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Args []string }
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]map[string]any{
		"user-prompt": {"session_id": "sess-1", "hook_event_name": "UserPromptSubmit", "prompt": "ciao", "cwd": dir},
		"pre-tool": {"session_id": "sess-1", "hook_event_name": "PreToolUse", "tool_name": "mcp__whatsapp__whatsapp_send_message",
			"tool_input": map[string]any{"to": "p_1", "text": "ciao"}, "tool_use_id": "toolu_1", "cwd": dir},
	}
	registered := [][]string{{"hook", "user-prompt"}, {"hook", "pre-tool"}}
	for _, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				registered = append(registered, h.Args)
			}
		}
	}
	if len(registered) != 4 {
		t.Fatalf("registered hooks: %v", registered)
	}
	for _, args := range registered {
		if len(args) != 2 || args[0] != "hook" || inputs[args[1]] == nil {
			t.Fatalf("hook args %v: not a known subcommand", args)
		}
		in, _ := json.Marshal(inputs[args[1]])
		var out bytes.Buffer
		if code := hookCmd(args[1:], bytes.NewReader(in), &out); code != 0 {
			t.Errorf("hook %s exit %d: %s", args[1], code, out.String())
		}
	}
}
