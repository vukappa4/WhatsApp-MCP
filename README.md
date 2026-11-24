# WhatsApp MCP

Your WhatsApp inside Claude Code. A small daemon on your computer links to your account as a WhatsApp linked device (like WhatsApp Web), keeps your chats in a local database and serves them to Claude Code over MCP.

Ask Claude things like "what did Marco say about Friday?", "summarize the family group this week", "find the PDF Giulia sent me in March", "reply to Paola that I'm running 10 minutes late".

## What it does

- **Reads everything you see in the app**: chats, groups, people, replies, edits, reactions, photos, documents, and voice notes as text (transcribed on your computer).
- **Searches** messages, people and chats across your whole history, including the history the phone sends when you link it.
- **Sends messages and files only when you ask.** A Claude Code hook checks every send against your own last message: it passes silently only when you asked to send to that person, otherwise Claude Code asks you to confirm. Text Claude reads in a message cannot authorize a send by itself. The one exception: a Claude Desktop scheduled task you approve sends text messages to the chats you approved for it ([Scheduled follow-ups](#scheduled-follow-ups)).
- **Always says how fresh the data is**: every answer starts with the connection state, so stale data is never shown as current.

## Scheduled follow-ups

Claude Desktop runs Claude Code tasks on a schedule. One such task can send WhatsApp text messages with nobody at the computer, to chats you approve:

1. In a Claude Code session in Claude Desktop, ask for it, like "every weekday at 9 ask Marco whether the invoice arrived". Claude creates the scheduled task, then calls `whatsapp_send_autosend_allow` with the task and the chats.
2. Claude Code asks you to confirm, showing the task, its schedule, permission mode and folder, its whole prompt (at most 1,500 characters) and the chats: people with their phone numbers (none for a person without a stored number), groups with their c_N id. Approve only if you asked for it.
3. From then on, runs of that task send text messages to those chats without asking, at most 20 a day to each chat: past that, each send asks. Anything else the task does on WhatsApp (another chat, a file, a history request) asks.

A run you start by hand ("Run now") or open and write in is yours: the task's own prompt never counts as your request there, so its sends ask unless you type a message asking for them.

The approval lasts until you revoke it (ask Claude: it calls `whatsapp_send_autosend_revoke`) or until the task's prompt, schedule, permission mode or folder changes; then approve it again. Approving again replaces the chats. A scheduled run can never approve itself. Neither a task nor a session in "bypass permissions" or "don't ask" mode can approve, since those modes settle a confirmation without showing it to you. `whatsapp_status` lists the approvals and whether each is active. Every such send is logged, without its text, in `autosend.log` in the profile's logs folder (`%LOCALAPPDATA%\WhatsAppMCP\prod\logs\` on Windows, `~/Library/Application Support/WhatsAppMCP/prod/logs/` on macOS).

Limits: only Claude Desktop's local scheduled tasks, on Windows and macOS, while the app is open and the computer awake. Autosend needs the program the hooks run at this version or later: prod's when it is installed, so update prod. It guards against Claude and the messages it reads, not against a program running as you, which can edit Desktop's files, the database or the settings; nor does it check that a run received the prompt its task file holds. Never use it for bulk or unsolicited messages.

## Install

Requirements: Windows 10 or 11 (before Windows 11 24H2 the daemon may show a console window) or macOS 13 or later (Apple Silicon or Intel), [Claude Code](https://claude.com/claude-code) 2.1.295 or later, and your phone with WhatsApp at hand. Voice notes on macOS also need [Homebrew](https://brew.sh).

The easy way: give Claude Code this link and ask it to install it.

```
Install https://github.com/vukappa4/WhatsApp-MCP for me
```

Claude follows [INSTALL.md](INSTALL.md): it downloads the program for your computer from [GitHub Releases](https://github.com/vukappa4/WhatsApp-MCP/releases/latest) and checks its SHA-256, links your phone with a QR code it opens in your browser, starts the daemon at login and connects it to Claude Code. You can follow the same steps by hand, or [build from source](INSTALL.md#build-from-source).

## Update

Run the installed program's `update` command. Windows (PowerShell):

```powershell
& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" update
```

macOS (Terminal):

```sh
"$HOME/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp" update
```

It downloads the latest release, checks its SHA-256 and restarts the daemon on it. Your pairing and messages stay. It can take many minutes, about half an hour at worst: let it run until it ends, since stopping it midway can leave the daemon stopped. `update --check` only reports whether a newer release exists. See [INSTALL.md](INSTALL.md#update) for the other options and for moving an install built from source to releases.

## Privacy and security

- Your messages stay on your computer, in `%LOCALAPPDATA%\WhatsAppMCP` on Windows and `~/Library/Application Support/WhatsAppMCP` on macOS. There is no server and no telemetry. What Claude reads goes to Anthropic like the rest of your Claude conversation.
- The MCP endpoint listens only on `127.0.0.1` and needs a random token that only your user can read.
- `whatsapp_send_file` sends only from your usual folders (Desktop, Documents, Downloads, Pictures, Videos or Movies, Music, OneDrive, iCloud Drive) and refuses keys, credentials and databases even there.

## Limits

- **Unofficial client.** This is not an official WhatsApp product. WhatsApp can ban accounts that use unofficial clients. The risk is low for personal use. Never use it for bulk or unsolicited messages.
- **Windows and macOS, Claude Code only.** The send confirmation is a Claude Code hook: other MCP clients would send without it.
- **Italian defaults.** Times are shown in Italian time (Europe/Rome), numbers without a country code are read as Italian, and send requests are recognized in Italian and English.
- Nothing arrives while the computer is off or asleep; WhatsApp delivers what was missed when it reconnects. If the phone stays offline for more than about 2 weeks, WhatsApp unlinks the device.

## Uninstall

See [INSTALL.md](INSTALL.md#uninstall).

## Credits

Built on [whatsmeow](https://github.com/tulir/whatsmeow) by Tulir Asokan.

## License

[MIT](LICENSE).

Not affiliated with, endorsed by or sponsored by WhatsApp or Meta.
