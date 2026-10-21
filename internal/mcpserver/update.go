package mcpserver

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/release"
	"github.com/vukappa4/whatsapp-mcp/internal/updatecheck"
)

// updateNotice is the part of the [whatsapp: ...] line that names a newer
// release; empty while there is none, so the line costs nothing more then.
func (s *Server) updateNotice() string {
	if s.Updates == nil {
		return ""
	}
	if u := s.Updates.Status(); u.Available() {
		return "update " + u.Latest + " available - whatsapp_status says how"
	}
	return ""
}

// updateLine starts with the update notice every tool result that has no
// [whatsapp: ...] line to carry it: the send tools, whatsapp_request_history
// and errors. The result is copied, never changed: a send keeps its own for
// a retry with the same idempotency key.
func (s *Server) updateLine(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		r, ok := res.(*mcp.CallToolResult)
		if method != "tools/call" || !ok || r == nil {
			return res, err
		}
		notice := s.updateNotice()
		if notice == "" {
			return res, err
		}
		line := "[whatsapp: " + notice + "]"
		content := make([]mcp.Content, 0, len(r.Content)+1)
		if len(r.Content) > 0 {
			if tc, ok := r.Content[0].(*mcp.TextContent); ok {
				if strings.HasPrefix(tc.Text, "[whatsapp: ") {
					return res, err
				}
				text := *tc
				text.Text = line + "\n" + tc.Text
				content = append(append(content, &text), r.Content[1:]...)
			}
		}
		if len(content) == 0 {
			content = append(append(content, &mcp.TextContent{Text: line}), r.Content...)
		}
		out := *r
		out.Content = content
		return &out, err
	}
}

// updateStatus is whatsapp_status's line on releases: the command that
// installs a newer one, or what the last check found.
func (s *Server) updateStatus() string {
	u := s.Updates.Status()
	now := s.now()
	if u.Available() {
		head := fmt.Sprintf("Update: %s is available, installed %s (%s).", u.Latest, u.Installed, u.URL)
		if _, err := release.AssetName(runtime.GOOS, runtime.GOARCH); err != nil {
			return fmt.Sprintf("%s Releases carry no binary for %s/%s: build %s from source as INSTALL.md says, then restart the daemon.",
				head, runtime.GOOS, runtime.GOARCH, u.Latest)
		}
		shell := ""
		if runtime.GOOS == "windows" {
			shell = " in PowerShell"
		}
		text := fmt.Sprintf("%s To install it, run%s `%s update --profile %s` in the background and wait until it exits by itself, "+
			"up to about 30 minutes: stopped midway, it can leave the daemon stopped. The pairing and the messages stay; "+
			"these WhatsApp tools are unavailable while it restarts the daemon.", head, shell, s.shellExe(), s.Profile)
		if n := settingsNote(u); n != "" {
			text += " " + strings.ToUpper(n[:1]) + n[1:] + "."
		}
		return text
	}
	if u.Off != "" {
		return "Update check: off - " + u.Off
	}
	var b strings.Builder
	if u.Latest == "" {
		b.WriteString("Update check: none done yet")
	} else {
		fmt.Fprintf(&b, "Update check: no newer release (installed %s, latest %s, checked %s)", u.Installed, u.Latest, fmtTS(u.CheckedAt.Unix(), now))
	}
	if !u.FailedAt.IsZero() {
		fmt.Fprintf(&b, "; the last check failed %s: %s", fmtTS(u.FailedAt.Unix(), now), clip(sanitize(u.Error), 200))
	}
	if !u.NextAt.IsZero() {
		fmt.Fprintf(&b, "; next %s", fmtTS(u.NextAt.Unix(), now))
	}
	if n := settingsNote(u); n != "" {
		b.WriteString("; " + n)
	}
	return b.String()
}

// settingsNote says that config.json cannot be read, so the check runs
// whatever update_check says. The error quotes the file: one clipped line.
func settingsNote(u updatecheck.Status) string {
	if u.Settings == "" {
		return ""
	}
	return "config.json cannot be read, so update_check is ignored: " + clip(sanitize(u.Settings), 200)
}
