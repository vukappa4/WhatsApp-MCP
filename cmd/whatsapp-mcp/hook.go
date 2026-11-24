package main

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/claudecode"
	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/desktop"
	"github.com/vukappa4/whatsapp-mcp/internal/hooks"
)

// scheduledRun reports whether Claude Desktop started this Claude Code
// session for a scheduled task: it sets CLAUDE_CODE_HOST_SCHEDULED_RUN=1,
// and the hooks inherit it.
func scheduledRun() bool { return os.Getenv("CLAUDE_CODE_HOST_SCHEDULED_RUN") == "1" }

// gateTimeout is the timeout Claude Code gives this gate: its registration
// in the settings.json of the Claude config folder this session uses. A
// registration it cannot read counts as the earlier 10 s.
func gateTimeout() time.Duration {
	if dir, err := claudecode.ConfigDir(); err == nil {
		if t, err := claudecode.GateTimeout(filepath.Join(dir, "settings.json")); err == nil {
			return t
		}
	}
	return 10 * time.Second
}

// hookCmd runs a Claude Code hook and returns its exit code. user-prompt
// always exits 0: a failing user-prompt hook must not block the prompt (it
// removes the session's flag, so the next send asks). pre-tool fails closed:
// it prints an "ask" decision for anything it cannot check ("deny" in a
// scheduled run with nobody present), and exits 2 (Claude Code blocks the
// call) when it cannot even print that. An unknown
// subcommand exits 2 too: a misregistered gate blocks instead of passing.
// The two names are permanent: update replaces the binary but never the
// settings.json that setup-claude wrote, so every future binary runs the
// hooks every earlier one registered.
func hookCmd(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	dir, err := hooks.DefaultDir()
	switch args[0] {
	case "user-prompt":
		if err == nil {
			_ = hooks.UserPrompt(stdin, dir, time.Now(), hooks.PromptOptions{Scheduled: scheduledRun()})
		}
		return 0
	case "pre-tool":
		root, rerr := config.AppDir()
		env := hooks.PreToolEnv{Dir: dir, Now: time.Now(), Scheduled: scheduledRun(), DesktopRoots: desktop.Roots()}
		if env.Scheduled {
			env.RunWait = hooks.RunWaitFor(gateTimeout())
		}
		if rerr == nil {
			env.Resolver = hooks.DBResolver{Root: root}
			env.AppDir = root
		}
		if err := hooks.PreTool(stdin, stdout, env); err != nil {
			return 2
		}
		return 0
	}
	return 2
}
