# WhatsApp MCP

Your WhatsApp inside Claude Code. A small daemon on your computer links to your account as a WhatsApp linked device (like WhatsApp Web), keeps your chats in a local database and serves them to Claude Code over MCP.

Ask Claude things like "what did Marco say about Friday?", "summarize the family group this week", "find the PDF Giulia sent me in March", "reply to Paola that I'm running 10 minutes late".

## What it does

- **Reads everything you see in the app**: chats, groups, people, replies, edits, reactions, photos, documents, and voice notes as text (transcribed on your computer).
- **Searches** messages, people and chats across your whole history, including the history the phone sends when you link it.
- **Sends messages and files only when you ask.** A Claude Code hook checks every send against your own last message: it passes silently only when you asked to send to that person, otherwise Claude Code asks you to confirm. Text Claude reads in a message cannot authorize a send by itself.
- **Always says how fresh the data is**: every answer starts with the connection state, so stale data is never shown as current.

## Install

Requirements: Windows 10 or 11 (before Windows 11 24H2 the daemon may show a console window) or macOS 13 or later (Apple Silicon or Intel), [Claude Code](https://claude.com/claude-code) 2.1.295 or later, and your phone with WhatsApp at hand.

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

It downloads the latest release, checks its SHA-256 and restarts the daemon on it. Your pairing and messages stay. It can take many minutes, about half an hour at worst: let it run until it ends, since stopping it midway can leave the daemon stopped. `update --check` only reports whether a newer release exists.

You do not have to remember it: the daemon checks for a newer release once a day. While one exists, Claude sees it in every WhatsApp result, tells you once and offers to run `update` for you. See [INSTALL.md](INSTALL.md#update) for the other options and for moving an install built from source to releases.

## Privacy and security

- Your messages stay on your computer, in `%LOCALAPPDATA%\WhatsAppMCP` on Windows and `~/Library/Application Support/WhatsAppMCP` on macOS. There is no server and no telemetry. What Claude reads goes to Anthropic like the rest of your Claude conversation.
- Besides WhatsApp, the daemon contacts only GitHub's API, once a day, to read the latest release's version. The request carries only the program's version, and GitHub sees your IP address as with any web request. To turn it off, put `{"update_check": false}` in `config.json` in the profile's data folder (`prod` or `dev` in the folder above); the daemon reads it within 10 minutes.
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
