# WhatsApp MCP

Go daemon that links to WhatsApp as a device (whatsmeow), stores everything in SQLite and serves MCP to Claude Code.

## Work

- Build `CGO_ENABLED=0 go build ./cmd/whatsapp-mcp`, test `go test ./...`. Keep `README.md` and `INSTALL.md` in step with the commands.
- Two installed profiles, each its own linked device: `dev` (port 8766) and `prod` (8765, in daily use). Deploy: commit, build, `<exe> install --profile dev`, check, then `--profile prod`. Never start `run` by hand for an installed profile.
- Token and speed metrics on a synthetic fake account: `go test ./internal/mcpserver -run TestSizeReport -v` (chars and tokens of tool definitions, instructions and a probe per read tool; `-args -dump` prints the texts) and `go test ./internal/mcpserver -run '^$' -bench . -benchtime 30x` (ns/op and SQL queries/op per probe).
- Real-data check, safe while the daemon runs: `WHATSAPP_MCP_E2E_DB=<prod data.db> go test ./internal/mcpserver -run TestRealData -v`.
- After changing hooks or `setup-claude`, run `setup-claude` under both `~/.claude` and `~/.claude-work`.

## Never

- Copy or share a `session.db`: it owns the account, and two clients on one session corrupt its encryption.
- Re-enable the legacy task `WhatsApp MCP Bridge`.
- Automate bulk or unsolicited sends: WhatsApp bans for them.
- Ack a message before it is archived in `raw_events`.
- Send `BuildFatalAppStateExceptionNotification`: it logs out every linked device.
- Build with `-H=windowsgui`: shells lose the exit code and stdout.
- Register the hooks in shell form or without `"onFailure": "block"`: the send gate then fails open.
