# WhatsApp MCP

Go daemon that links to WhatsApp as a device (whatsmeow), stores everything in SQLite and serves MCP to Claude Code.

## Work

- Build `CGO_ENABLED=0 go build ./cmd/whatsapp-mcp`, test `go test ./...`. Keep `README.md` and `INSTALL.md` in step with the commands.
- Release: push a tag `vX.Y.Z` on a commit of `main` (`vX.Y.Z-rc.N` publishes a prerelease); `release.yml` refuses a tag outside `main`, tests it, then GoReleaser (`.goreleaser.yaml`) publishes the binaries and `checksums.txt`. Only the highest `vX.Y.Z` tag on `main` becomes the latest release, published only once all its assets are uploaded.
- macOS transcription tools: `tools.yml` builds parakeet-cli, whisper-cli (the whisper.cpp release `internal/media/setup.go` pins) and a minimal ffmpeg (the version `config.PinnedFFmpegDir` names); a tag `tools-whisper.cpp-<release>-<N>` publishes them as a release that is never the latest. Keep the versions in `tools.yml`'s `env` equal to those two, then pin each archive's size and SHA-256 in `setup.go` (`toolsRelease`, `darwinTools`); `transcription.yml` runs `setup-transcription` and both engines on macOS runners.
- Two installed profiles, each its own linked device: `dev` (port 8766) and `prod` (8765, in daily use). Deploy: commit, build, `<exe> install --profile dev`, check, then `--profile prod`. Never start `run` by hand for an installed profile.
- Token and speed metrics on a synthetic fake account: `go test ./internal/mcpserver -run TestSizeReport -v` (chars and tokens of tool definitions, instructions and a probe per read tool; `-args -dump` prints the texts) and `go test ./internal/mcpserver -run '^$' -bench . -benchtime 30x` (ns/op and SQL queries/op per probe).
- Real-data check, safe while the daemon runs: `WHATSAPP_MCP_E2E_DB=<prod data.db> go test ./internal/mcpserver -run TestRealData -v`.
- Update E2E, which reinstalls dev from a fake GitHub and leaves prod alone: `WHATSAPP_MCP_E2E_UPDATE=dev go test ./cmd/whatsapp-mcp -run TestUpdateE2E -v -timeout 30m`.
- After changing hooks or `setup-claude`, run `setup-claude` under both `~/.claude` and `~/.claude-work`.

## Never

- Copy or share a `session.db`: it owns the account, and two clients on one session corrupt its encryption.
- Re-enable the legacy task `WhatsApp MCP Bridge`.
- Automate bulk or unsolicited sends: WhatsApp bans for them.
- Ack a message before it is archived in `raw_events`.
- Send `BuildFatalAppStateExceptionNotification`: it logs out every linked device.
- Build with `-H=windowsgui`: shells lose the exit code and stdout.
- Strip release binaries (`-s -w`): Microsoft Defender's machine learning flagged a stripped build as a trojan.
- Register the hooks in shell form or without `"onFailure": "block"`: the send gate then fails open.
