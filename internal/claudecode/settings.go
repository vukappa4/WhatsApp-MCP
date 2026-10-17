// Package claudecode registers the daemon with Claude Code: the MCP server
// through the claude CLI, and the read-tool allow rules and the send-gate
// hooks in the user's settings.json.
package claudecode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/vukappa4/whatsapp-mcp/internal/hooks"
)

// ReadTools are the tools that never send: they are allowed without a prompt.
var ReadTools = []string{
	"whatsapp_status", "whatsapp_list_chats", "whatsapp_read_chat",
	"whatsapp_search_people", "whatsapp_search_messages", "whatsapp_get_media",
}

// HookTimeout is the timeout, in seconds, of both registered hooks.
const HookTimeout = 10

// ConfigDir is Claude Code's user config dir: $CLAUDE_CONFIG_DIR, else ~/.claude.
func ConfigDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// allowRules are the permission rules allowing server's read tools.
func allowRules(server string) []string {
	out := make([]string, len(ReadTools))
	for i, t := range ReadTools {
		out[i] = "mcp__" + server + "__" + t
	}
	return out
}

// hookGroups are the UserPromptSubmit and PreToolUse entries running exe in
// exec form: Claude Code spawns the binary directly, with no shell. Only the
// PreToolUse gate blocks on failure; a failing UserPromptSubmit hook must
// not reject the user's prompts.
func hookGroups(exe string) map[string]*object {
	exe = filepath.ToSlash(exe)
	return map[string]*object{
		"UserPromptSubmit": obj("hooks", []any{
			obj("type", "command", "command", exe, "args", []any{"hook", "user-prompt"}, "timeout", HookTimeout),
		}),
		"PreToolUse": obj("matcher", hooks.PreToolMatcher, "hooks", []any{
			obj("type", "command", "command", exe, "args", []any{"hook", "pre-tool"}, "timeout", HookTimeout, "onFailure", "block"),
		}),
	}
}

// hookEvents fixes the order in which the events are added to a file.
var hookEvents = []string{"UserPromptSubmit", "PreToolUse"}

// Register writes into the settings file at file the allow rules for
// server's read tools and the two hooks running exe. Hooks of an earlier
// registration (any whatsapp-mcp program) are replaced, so every profile
// shares one pair of hooks: the gate serves them all. The rest of the file
// is kept as it was, key order included. A missing file is created.
func Register(file, exe, server string) error {
	return edit(file, func(root *object) error {
		perms, err := root.child("permissions")
		if err != nil {
			return err
		}
		allow, err := perms.list("allow")
		if err != nil {
			return fmt.Errorf("permissions: %w", err)
		}
		for _, r := range allowRules(server) {
			if !slices.Contains(allow, any(r)) {
				allow = append(allow, r)
			}
		}
		perms.set("allow", allow)
		return setHooks(root, exe)
	})
}

// setHooks replaces the hooks of any whatsapp-mcp program with the two
// hooks running exe.
func setHooks(root *object, exe string) error {
	hs, err := root.child("hooks")
	if err != nil {
		return err
	}
	groups := hookGroups(exe)
	for _, ev := range hookEvents {
		l, err := withoutOurHooks(hs, ev)
		if err != nil {
			return err
		}
		hs.set(ev, append(l, groups[ev]))
	}
	return nil
}

// Unregister removes server's allow rules from the settings file at file.
// The hooks are shared by every profile: with keepExe set (another profile
// still installed) they are pointed at keepExe, otherwise every hook of a
// whatsapp-mcp program is removed. Keys left empty by the removal are
// dropped. A missing file is left missing.
func Unregister(file, server, keepExe string) error {
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return edit(file, func(root *object) error {
		if v, ok := root.get("permissions"); ok {
			perms, ok := v.(*object)
			if !ok {
				return errors.New(`"permissions" is not an object`)
			}
			allow, err := perms.list("allow")
			if err != nil {
				return fmt.Errorf("permissions: %w", err)
			}
			ours := allowRules(server)
			kept := []any{}
			for _, r := range allow {
				if s, ok := r.(string); !ok || !slices.Contains(ours, s) {
					kept = append(kept, r)
				}
			}
			setOrDrop(perms, "allow", kept)
			if len(perms.keys) == 0 {
				root.del("permissions")
			}
		}
		if keepExe != "" {
			return setHooks(root, keepExe)
		}
		if v, ok := root.get("hooks"); ok {
			hs, ok := v.(*object)
			if !ok {
				return errors.New(`"hooks" is not an object`)
			}
			for _, ev := range append([]string(nil), hs.keys...) {
				l, err := withoutOurHooks(hs, ev)
				if err != nil {
					return err
				}
				setOrDrop(hs, ev, l)
			}
			if len(hs.keys) == 0 {
				root.del("hooks")
			}
		}
		return nil
	})
}

// withoutOurHooks is the event's list of matcher groups with every hook
// running whatsapp-mcp taken out, and groups left with no hooks dropped.
func withoutOurHooks(hs *object, ev string) ([]any, error) {
	l, err := hs.list(ev)
	if err != nil {
		return nil, fmt.Errorf("hooks: %w", err)
	}
	out := []any{}
	for _, g := range l {
		group, ok := g.(*object)
		if !ok {
			out = append(out, g)
			continue
		}
		inner, err := group.list("hooks")
		if err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", ev, err)
		}
		kept := []any{}
		for _, h := range inner {
			if !isOurHook(h) {
				kept = append(kept, h)
			}
		}
		if len(kept) == 0 && len(inner) > 0 {
			continue
		}
		if len(kept) != len(inner) {
			group.set("hooks", kept)
		}
		out = append(out, group)
	}
	return out, nil
}

// isOurHook reports whether a hook runs the program whatsapp-mcp(.exe), in
// exec form or as the first word of a shell command line.
func isOurHook(h any) bool {
	o, ok := h.(*object)
	if !ok {
		return false
	}
	v, _ := o.get("command")
	cmd, ok := v.(string)
	if !ok {
		return false
	}
	prog := strings.TrimSpace(cmd)
	if _, exec := o.get("args"); !exec {
		prog = firstWord(prog)
	}
	base := strings.ToLower(path.Base(strings.ReplaceAll(prog, `\`, "/")))
	return base == "whatsapp-mcp" || base == "whatsapp-mcp.exe"
}

// firstWord is the program of a shell command line: up to the closing quote
// when it starts with one, else up to the first space.
func firstWord(s string) string {
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		if i := strings.IndexByte(s[1:], s[0]); i >= 0 {
			return s[1 : i+1]
		}
		return s[1:]
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// edit applies change to the settings file and writes it back through a
// temporary file, so a failure never leaves it half written. A file that is
// not valid JSON is not touched.
func edit(file string, change func(*object) error) error {
	// A symlinked settings file (dotfiles) is written through its target.
	if target, err := filepath.EvalSymlinks(file); err == nil {
		file = target
	}
	b, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	root, err := parseObject(b)
	if err != nil {
		return fmt.Errorf("%s is not valid JSON, not changed: %w", file, err)
	}
	if err := change(root); err != nil {
		return fmt.Errorf("%s: %w; not changed", file, err)
	}
	out, err := encode(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

func setOrDrop(o *object, k string, l []any) {
	if len(l) == 0 {
		o.del(k)
		return
	}
	o.set(k, l)
}
