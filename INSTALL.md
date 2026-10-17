# Install

These steps are written for an AI agent (Claude Code) installing WhatsApp MCP for its user, and work by hand too. Run them in order. Each step says how to tell that it worked. Each command is given for Windows in PowerShell and for macOS in Terminal. In Git Bash on Windows (Claude Code's Bash tool), run the `winget` lines as given, and write the `git`, `go` and `whatsapp-mcp` lines in their macOS form with `whatsapp-mcp.exe` as the program name (`./whatsapp-mcp.exe pair`): Git Bash does not understand the backslashes and `$env:` of the PowerShell commands.

## 1. Check the requirements

- Windows 10 or 11, or macOS 13 or later (Apple Silicon or Intel). On Windows 11 before 24H2 and on Windows 10 the daemon may show a console window: tell the user.
- `claude --version` prints 2.1.295 or later. Otherwise the user updates Claude Code first.
- `git --version` works and `go version` prints go1.27 or later. Otherwise:
  - Windows: `winget install --id Git.Git -e --accept-source-agreements --accept-package-agreements` and `winget install --id GoLang.Go -e --accept-source-agreements --accept-package-agreements`. Without the two flags the first winget run waits for an answer no agent can give. Tell the user first: Windows asks them to allow each installer.
  - macOS: `brew install git go`. Without Homebrew (`brew --version` fails), the user installs it from https://brew.sh first: its installer asks for their password. On a Mac without the Command Line Tools, `git --version` opens a dialog offering to install them: the user accepts it.

A tool installed just now is not on the PATH of the running shell:

- Windows: call it by its full path (`C:\Program Files\Go\bin\go.exe`, `C:\Program Files\Git\cmd\git.exe`) or open a new shell.
- macOS: after a fresh Homebrew install, run `eval "$(/opt/homebrew/bin/brew shellenv)"` (Intel Macs: `eval "$(/usr/local/bin/brew shellenv)"`) in the same shell before calling `brew` or `go`, or call them by their full path (`/opt/homebrew/bin/go`).

## 2. Get and build

Pick a folder the user keeps (updates are built from it), for example `whatsapp-mcp` in their home folder.

Windows:

```powershell
git clone https://github.com/vukappa4/whatsapp-mcp "$HOME\whatsapp-mcp"
cd "$HOME\whatsapp-mcp"
$env:CGO_ENABLED = "0"; go build -o whatsapp-mcp.exe ./cmd/whatsapp-mcp
```

macOS:

```sh
git clone https://github.com/vukappa4/whatsapp-mcp ~/whatsapp-mcp
cd ~/whatsapp-mcp
CGO_ENABLED=0 go build -o whatsapp-mcp ./cmd/whatsapp-mcp
```

Worked when `.\whatsapp-mcp.exe help` (macOS: `./whatsapp-mcp help`) prints the usage. Run every later command from this folder. The commands below use the Windows name `.\whatsapp-mcp.exe`: on macOS write `./whatsapp-mcp`.

## 3. Link the phone

Tell the user a page with a QR code is about to open in their browser, then start pairing **in the background** (it keeps running for several minutes):

```powershell
.\whatsapp-mcp.exe pair
```

It opens the page and prints `The QR code is open in the browser: <url>`. Tell the user:

1. On the phone, open WhatsApp > Settings (iPhone) or the three-dot menu (Android) > Linked devices > Link a device.
2. Scan the QR code on the page. It refreshes by itself for about 8 minutes.

If the page did not open, give the user the URL the command printed. Once the phone links, the page shows "Linked", the output says `Paired as ...`, and the phone starts sending the chat history. Leave the command running until it exits by itself, usually 2 to 15 minutes, longer for a big history. The phone must stay online with WhatsApp open meanwhile. It ends with `History sync quiet ...`.

- `QR code not scanned in time`: run the same command again.
- `profile already paired`: this computer is already linked; skip to step 4.
- The user cannot scan (camera broken, no browser): ask for their WhatsApp number with the country code and run `.\whatsapp-mcp.exe pair --phone "+39 333 123 4567"` instead. Within about 15 seconds it prints `Pairing code: ABCD-EFGH`. Give it to the user at once, since it expires in about 2 minutes: on the phone, Linked devices > Link a device > "Link with phone number instead", then type the code.

## 4. Install the daemon

Start this step only after `pair` has exited: while it runs it holds the profile, and `install` fails after waiting minutes for it.

```powershell
.\whatsapp-mcp.exe install
```

It copies the program to the app folder and starts it, then keeps it running and starts it at every login:

- Windows: the program goes to `%LOCALAPPDATA%\WhatsAppMCP\bin\`, run by the task "WhatsApp MCP (prod)".
- macOS: the program goes to `~/Library/Application Support/WhatsAppMCP/bin/`, run by the LaunchAgent `io.github.vukappa4.whatsapp-mcp.prod`. macOS may show a "Background Items Added" notification: tell the user it is this daemon.

Worked when it prints `Daemon running from ...`.

## 5. Connect Claude Code

```powershell
.\whatsapp-mcp.exe setup-claude
```

It registers the MCP server `whatsapp` at user scope through the `claude` CLI, and adds to the user's Claude Code `settings.json` the allow rules for the read tools and the two hooks that confirm sends. It keeps everything else in that file. Worked when it prints `Restart Claude Code to load them.`

## 6. Voice notes and audio (optional, ask the user)

Transcribing voice notes and sending voice notes need ffmpeg; transcription also needs a speech model (about 650 MB download).

Windows:

```powershell
winget install --id Gyan.FFmpeg -e --accept-source-agreements --accept-package-agreements
.\whatsapp-mcp.exe setup-transcription
.\whatsapp-mcp.exe install
```

macOS (the transcription programs come from Homebrew's `whisper.cpp`, which on Intel Macs builds from source for several minutes):

```sh
brew install ffmpeg whisper.cpp
./whatsapp-mcp setup-transcription
./whatsapp-mcp install
```

The second `install` restarts the daemon so it finds ffmpeg.

## 7. Finish

Tell the user to restart Claude Code, then ask something like "what are my latest WhatsApp chats?". `.\whatsapp-mcp.exe status` shows pairing, daemon and data state at any time. Logs are in `%LOCALAPPDATA%\WhatsAppMCP\prod\logs\` (macOS: `~/Library/Application Support/WhatsAppMCP/prod/logs/`).

On macOS, tell the user: the first time Claude sends a file from Desktop, Documents or Downloads, macOS may ask whether whatsapp-mcp may access that folder, and they allow it. If a send fails with "macOS may not have given whatsapp-mcp access", they add `~/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp` in System Settings > Privacy & Security > Full Disk Access (press +, then Cmd+Shift+G to type the path), then run `./whatsapp-mcp install`. After every update macOS may ask again, and a Full Disk Access entry stops applying: the user removes it and adds it again.

## Link again

WhatsApp unlinks this computer when the phone stays offline for more than 14 days, or when the user removes it under Linked devices. `status` then says the profile is not paired, and the WhatsApp tools answer "not paired" or "logged this device out". From the folder of step 2:

```powershell
.\whatsapp-mcp.exe stop
.\whatsapp-mcp.exe pair
```

Start `pair` right after `stop`, in the background as in step 3: the autostart starts the daemon again within minutes, and a `pair` started after that fails because another process holds the profile (run both again). The user scans the QR code with the same WhatsApp number as before: another number in the same profile mixes two accounts' messages. If `pair` says `profile already paired`, run `.\whatsapp-mcp.exe unlink`, then `pair` again. The WhatsApp tools are unavailable until the last step. When `pair` has exited by itself (`History sync quiet ...`), finish with:

```powershell
.\whatsapp-mcp.exe install
```

Worked when it prints `Daemon running from ...` and `status` shows the account. The messages stored before stay.

## Update

Windows:

```powershell
cd "$HOME\whatsapp-mcp"
git pull
$env:CGO_ENABLED = "0"; go build -o whatsapp-mcp.exe ./cmd/whatsapp-mcp
.\whatsapp-mcp.exe install
```

macOS:

```sh
cd ~/whatsapp-mcp
git pull
CGO_ENABLED=0 go build -o whatsapp-mcp ./cmd/whatsapp-mcp
./whatsapp-mcp install
```

## Uninstall

From the folder of step 2:

```powershell
.\whatsapp-mcp.exe setup-claude --remove
.\whatsapp-mcp.exe uninstall
.\whatsapp-mcp.exe unlink
```

`setup-claude --remove` takes the server, hooks and allow rules out of Claude Code. `uninstall` stops the daemon and removes its task or LaunchAgent and its program. `unlink` logs the device out, so the phone no longer lists it. The messages stay in `%LOCALAPPDATA%\WhatsAppMCP` (macOS: `~/Library/Application Support/WhatsAppMCP`) until the user deletes that folder.
