package claudecode

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var errNoClaude = errors.New("the claude command is not on PATH: install Claude Code, or open a new terminal after installing it")

// AddMCPServer registers the daemon at url as the user-scope MCP server
// name through the claude CLI. A server already registered under name is
// replaced only once the new entry is known to be needed, so a failure
// never leaves the name unregistered without saying so. The CLI writes
// Claude Code's own config, in $CLAUDE_CONFIG_DIR when set.
func AddMCPServer(name, url, token string) error {
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errNoClaude
	}
	add := []string{"mcp", "add", "--scope", "user", "--transport", "http", name, url, "--header", "Authorization: Bearer " + token}
	out, err := runClaude(claude, token, add...)
	if err == nil || !strings.Contains(out, "already exists") {
		return err
	}
	if _, err := runClaude(claude, token, "mcp", "remove", "--scope", "user", name); err != nil {
		return err
	}
	if _, err := runClaude(claude, token, add...); err != nil {
		return fmt.Errorf("%w; the server %q is not registered now: run setup-claude again", err, name)
	}
	return nil
}

// RemoveMCPServer removes the user-scope MCP server name and reports
// whether it was registered.
func RemoveMCPServer(name string) (bool, error) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		return false, errNoClaude
	}
	out, err := runClaude(claude, "", "mcp", "remove", "--scope", "user", name)
	if err != nil && strings.Contains(out, "No MCP server named") {
		return false, nil
	}
	return err == nil, err
}

// runClaude runs the claude CLI and returns its output; neither the output
// nor an error ever shows the token.
func runClaude(claude, token string, args ...string) (string, error) {
	out, err := exec.Command(claude, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if token != "" {
		text = strings.ReplaceAll(text, token, "<token>")
	}
	if err != nil {
		return text, fmt.Errorf("claude %s %s: %v: %s", args[0], args[1], err, text)
	}
	return text, nil
}
