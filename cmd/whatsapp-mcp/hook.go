package main

import (
	"io"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/hooks"
)

// hookCmd runs a Claude Code hook and returns its exit code. user-prompt
// always exits 0: a failing user-prompt hook must not block the prompt (it
// removes the session's flag, so the next send asks). pre-tool fails closed:
// it prints an "ask" decision for anything it cannot check, and exits 2
// (Claude Code blocks the call) when it cannot even print that. An unknown
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
			_ = hooks.UserPrompt(stdin, dir, time.Now())
		}
		return 0
	case "pre-tool":
		root, rerr := config.AppDir()
		env := hooks.PreToolEnv{Dir: dir, Now: time.Now()}
		if rerr == nil {
			env.Resolver = hooks.DBResolver{Root: root}
		}
		if err := hooks.PreTool(stdin, stdout, env); err != nil {
			return 2
		}
		return 0
	}
	return 2
}
