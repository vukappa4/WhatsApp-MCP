package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/hooks"
)

const exe = `C:\Users\me\AppData\Local\WhatsAppMCP\bin\whatsapp-mcp.exe`

type hook struct {
	Type, Command string
	Args          []string
	Timeout       int
	OnFailure     string `json:"onFailure"`
}

type settings struct {
	Permissions struct{ Allow []string } `json:"permissions"`
	Hooks       map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []hook `json:"hooks"`
	} `json:"hooks"`
}

func load(t *testing.T, file string) (settings, string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var s settings
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	return s, string(b)
}

// A fresh registration holds both hooks on the binary in exec form (no shell
// between Claude Code and the binary), blocks a send when the gate fails to
// run, gates the send tools and allows exactly the read tools.
func TestRegisterFresh(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sub", "settings.json")
	if err := Register(file, exe, "whatsapp"); err != nil {
		t.Fatal(err)
	}
	s, _ := load(t, file)
	up, pre := s.Hooks["UserPromptSubmit"][0].Hooks[0], s.Hooks["PreToolUse"][0].Hooks[0]
	for _, h := range []hook{up, pre} {
		if h.Type != "command" || h.Command != filepath.ToSlash(exe) || h.Timeout <= int(hooks.PreToolBudget.Seconds())+1 {
			t.Errorf("hook %+v", h)
		}
	}
	if strings.Join(up.Args, " ") != "hook user-prompt" || up.OnFailure == "block" {
		t.Errorf("user-prompt hook %+v: a failure must not block the user's prompts", up)
	}
	if strings.Join(pre.Args, " ") != "hook pre-tool" || pre.OnFailure != "block" {
		t.Errorf("pre-tool hook %+v: a failure must block the send", pre)
	}
	if s.Hooks["PreToolUse"][0].Matcher != hooks.PreToolMatcher {
		t.Errorf("matcher %q", s.Hooks["PreToolUse"][0].Matcher)
	}
	m := regexp.MustCompile(`^(?:` + hooks.PreToolMatcher + `)$`)
	if len(s.Permissions.Allow) != len(ReadTools) {
		t.Errorf("allow rules = %v", s.Permissions.Allow)
	}
	for _, a := range s.Permissions.Allow {
		if m.MatchString(a) || !strings.HasPrefix(a, "mcp__whatsapp__whatsapp_") {
			t.Errorf("allow rule %s", a)
		}
	}
}

// Registering into an existing file keeps everything else, key order
// included, replaces an earlier registration's hooks (shell form too) and
// does not duplicate allow rules; unregistering restores the rest.
func TestRegisterExistingAndUnregister(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	orig := "\xEF\xBB\xBF" + `{
  "model": "opus",
  "permissions": {
    "allow": [
      "Bash(git status)",
      "mcp__whatsapp__whatsapp_status"
    ],
    "deny": []
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "echo <&> ok"
          }
        ]
      },
      {
        "matcher": "mcp__whatsapp__whatsapp_send_.*",
        "hooks": [
          {
            "type": "command",
            "command": "\"C:/Old Path/whatsapp-mcp.exe\" hook pre-tool"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "C:\\Tools\\WhatsApp-MCP.EXE",
            "args": ["hook", "user-prompt"]
          },
          {
            "type": "command",
            "command": "notify.exe"
          }
        ]
      }
    ]
  },
  "zeta": 1.50
}
`
	if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Register(file, exe, "whatsapp"); err != nil {
			t.Fatal(err)
		}
	}
	s, text := load(t, file)
	if strings.Count(text, "whatsapp-mcp.exe") != 2 || strings.Contains(text, "Old Path") {
		t.Errorf("earlier hooks not replaced:\n%s", text)
	}
	if len(s.Permissions.Allow) != 1+len(ReadTools) || s.Permissions.Allow[0] != "Bash(git status)" {
		t.Errorf("allow = %v", s.Permissions.Allow)
	}
	pre := s.Hooks["PreToolUse"]
	if len(pre) != 2 || pre[0].Matcher != "Bash" || pre[1].Matcher != hooks.PreToolMatcher {
		t.Errorf("PreToolUse = %+v", pre)
	}
	if st := s.Hooks["Stop"]; len(st) != 1 || len(st[0].Hooks) != 2 {
		t.Errorf("Stop = %+v: Register touches only its own events", st)
	}
	for _, k := range []string{`"model"`, `"permissions"`, `"hooks"`, `"zeta": 1.50`} {
		if !strings.Contains(text, k) {
			t.Errorf("missing %s", k)
		}
	}
	if !(strings.Index(text, `"model"`) < strings.Index(text, `"permissions"`) &&
		strings.Index(text, `"permissions"`) < strings.Index(text, `"hooks"`) &&
		strings.Index(text, `"hooks"`) < strings.Index(text, `"zeta"`)) {
		t.Errorf("key order changed:\n%s", text)
	}
	if !strings.Contains(text, `"echo <&> ok"`) || !strings.Contains(text, `"deny": []`) {
		t.Errorf("other values changed:\n%s", text)
	}

	if err := Unregister(file, "whatsapp", ""); err != nil {
		t.Fatal(err)
	}
	s, text = load(t, file)
	if strings.Contains(strings.ToLower(text), "whatsapp") {
		t.Errorf("registration left behind:\n%s", text)
	}
	if len(s.Permissions.Allow) != 1 || len(s.Hooks["PreToolUse"]) != 1 || len(s.Hooks["Stop"]) != 1 ||
		s.Hooks["Stop"][0].Hooks[0].Command != "notify.exe" {
		t.Errorf("unrelated settings removed:\n%s", text)
	}
}

// Unregistering what Register alone wrote leaves an empty object; a missing
// file stays missing.
func TestUnregisterLeavesNothing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := Unregister(file, "whatsapp", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("missing file created: %v", err)
	}
	if err := Register(file, exe, "whatsapp-dev"); err != nil {
		t.Fatal(err)
	}
	if err := Unregister(file, "whatsapp-dev", ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != "{}\n" {
		t.Errorf("left %q", b)
	}
}

// A file that is not JSON, or has an unexpected shape, is never rewritten.
func TestRegisterRefusesBadFiles(t *testing.T) {
	for _, bad := range []string{`{"model": "opus",}`, `[]`, `{"hooks": []}`, `{"permissions": {"allow": "x"}}`, `{} {}`} {
		file := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(file, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Register(file, exe, "whatsapp"); err == nil {
			t.Errorf("%s: accepted", bad)
		}
		if b, _ := os.ReadFile(file); string(b) != bad {
			t.Errorf("%s: rewritten as %s", bad, b)
		}
	}
}

func TestIsOurHook(t *testing.T) {
	for h, ours := range map[*object]bool{
		obj("command", "C:/x/whatsapp-mcp.exe", "args", []any{"hook", "pre-tool"}): true,
		obj("command", `C:\X\WhatsApp-MCP.EXE`, "args", []any{}):                   true,
		obj("command", "/usr/local/bin/whatsapp-mcp", "args", []any{"hook"}):       true,
		obj("command", `"C:/Old Path/whatsapp-mcp.exe" hook pre-tool`):             true,
		obj("command", "C:/x/whatsapp-mcp.exe hook pre-tool --log a/b"):            true,
		obj("command", "C:/x/whatsapp-mcp-notify.ps1"):                             false,
		obj("command", "notify.exe --about C:/x/whatsapp-mcp.exe"):                 false,
		obj("command", "C:/x/whatsapp-mcp-notify.exe", "args", []any{"hook"}):      false,
		obj("type", "prompt"): false,
	} {
		if isOurHook(h) != ours {
			t.Errorf("isOurHook(%v) = %v", h.vals, !ours)
		}
	}
}

// Removing one profile while another stays installed keeps the gate,
// pointed at the remaining profile's binary.
func TestUnregisterKeepsHooksForAnotherProfile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	dev := `C:\Users\me\AppData\Local\WhatsAppMCP\bin-dev\whatsapp-mcp.exe`
	if err := Register(file, exe, "whatsapp"); err != nil {
		t.Fatal(err)
	}
	if err := Register(file, dev, "whatsapp-dev"); err != nil {
		t.Fatal(err)
	}
	if err := Unregister(file, "whatsapp-dev", exe); err != nil {
		t.Fatal(err)
	}
	s, text := load(t, file)
	if strings.Contains(text, "bin-dev") || strings.Contains(text, "whatsapp-dev") {
		t.Errorf("dev left behind:\n%s", text)
	}
	if len(s.Permissions.Allow) != len(ReadTools) || len(s.Hooks["PreToolUse"]) != 1 ||
		s.Hooks["PreToolUse"][0].Hooks[0].Command != filepath.ToSlash(exe) || len(s.Hooks["UserPromptSubmit"]) != 1 {
		t.Errorf("prod registration damaged:\n%s", text)
	}
}

// Rewriting an existing settings file keeps its permission bits: the
// temporary file it is written through starts as 0600.
func TestEditKeepsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(file, []byte(`{"model": "opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Register(file, exe, "whatsapp"); err != nil {
		t.Fatal(err)
	}
	if err := Unregister(file, "whatsapp", ""); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode after rewrite = %o, want 644", got)
	}
}

// Check reads a registration and names what differs from what this version
// registers, without changing the file. A file never registered, or
// registered for another profile only, has nothing to report.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	check := func(exe, server string) []string {
		t.Helper()
		before, _ := os.ReadFile(file)
		drift, err := Check(file, exe, server)
		if err != nil {
			t.Fatal(err)
		}
		if after, _ := os.ReadFile(file); string(after) != string(before) {
			t.Fatal("Check changed the file")
		}
		return drift
	}
	if d := check(exe, "whatsapp"); d != nil {
		t.Fatalf("missing file: %v", d)
	}
	if err := os.WriteFile(file, []byte(`{"permissions": {"allow": ["Bash(ls)"]}, "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "lint"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := check(exe, "whatsapp"); d != nil {
		t.Fatalf("never registered: %v", d)
	}
	if err := Register(file, exe, "whatsapp"); err != nil {
		t.Fatal(err)
	}
	if d := check(exe, "whatsapp"); d != nil {
		t.Fatalf("fresh registration: %v", d)
	}
	if d := check(exe, "whatsapp-dev"); d != nil {
		t.Fatalf("dev never registered here: %v", d)
	}
	if d := check(`C:\other\whatsapp-mcp.exe`, "whatsapp"); len(d) != 2 {
		t.Fatalf("hooks on another binary: %v", d)
	}

	// The same registration with its keys in another order is the same.
	b, _ := os.ReadFile(file)
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	sorted, _ := json.Marshal(v) // encoding/json sorts the keys
	if err := os.WriteFile(file, sorted, 0o600); err != nil {
		t.Fatal(err)
	}
	if d := check(exe, "whatsapp"); d != nil {
		t.Fatalf("reordered keys: %v", d)
	}

	// An earlier version: a narrower matcher, a missing read tool rule.
	older := strings.Replace(string(sorted), "whatsapp_send_.*|", "whatsapp_send_message|", 1)
	older = strings.Replace(older, `,"mcp__whatsapp__whatsapp_get_media"`, "", 1)
	if older == string(sorted) || strings.Contains(older, "get_media") {
		t.Fatalf("test edit did not apply: %s", older)
	}
	if err := os.WriteFile(file, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	d := check(exe, "whatsapp")
	if len(d) != 2 || !strings.Contains(d[0], "PreToolUse") || d[1] != "no allow rule for mcp__whatsapp__whatsapp_get_media" {
		t.Fatalf("older registration: %q", d)
	}

	if err := os.WriteFile(file, []byte(`{"hooks": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(file, exe, "whatsapp"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
