# Install

These steps are written for an AI agent (Claude Code) installing WhatsApp MCP for its user, and work by hand too. Run them in order. Each step says how to tell that it worked. Each command is given for Windows in PowerShell and for macOS in Terminal.

In Git Bash on Windows (Claude Code's Bash tool), Git Bash does not understand the backslashes and `$env:` of the PowerShell commands. Run the `winget` lines as given, and write the other lines in their macOS form with three changes: set the asset with `a=whatsapp-mcp_windows_amd64.exe` (`a=whatsapp-mcp_windows_arm64.exe` when `echo $PROCESSOR_ARCHITECTURE` prints `ARM64`) in place of the `sysctl` line, name the downloaded program `whatsapp-mcp.exe` (`./whatsapp-mcp.exe pair`), and run the installed program as `"$LOCALAPPDATA/WhatsAppMCP/bin/whatsapp-mcp.exe"`.

## 1. Check the requirements

- Windows 10 or 11, or macOS 13 or later (Apple Silicon or Intel). On Windows 11 before 24H2 and on Windows 10 the daemon may show a console window: tell the user.
- `claude --version` prints 2.1.295 or later. Otherwise the user updates Claude Code first.
- The computer reaches github.com. Behind a proxy, set `HTTPS_PROXY`.

The program is one file, published on [GitHub Releases](https://github.com/vukappa4/WhatsApp-MCP/releases/latest). Nothing else needs installing. Each computer takes its own file (asset):

| Computer | Asset |
| --- | --- |
| Windows, `$env:PROCESSOR_ARCHITECTURE` prints `AMD64` | `whatsapp-mcp_windows_amd64.exe` |
| Windows, `$env:PROCESSOR_ARCHITECTURE` prints `ARM64` | `whatsapp-mcp_windows_arm64.exe` |
| Mac with Apple Silicon: `sysctl -n hw.optional.arm64` prints `1` | `whatsapp-mcp_darwin_arm64` |
| Intel Mac: the same command prints `0` or an error | `whatsapp-mcp_darwin_amd64` |

On a Mac, do not use `uname -m`: in a shell running under Rosetta it prints `x86_64` on Apple Silicon too. The commands of step 2 pick the asset by themselves.

## 2. Download

Download with these commands, not with the browser. A browser marks the file as downloaded from the internet, and the program is not signed: Windows may then warn about it, and macOS Gatekeeper blocks it. The commands save the program as `whatsapp-mcp.exe` (macOS: `whatsapp-mcp`) in a temporary folder, check its SHA-256 against the release's `checksums.txt` and print its version.

Windows:

```powershell
$ProgressPreference = "SilentlyContinue"
$a = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "whatsapp-mcp_windows_arm64.exe" } else { "whatsapp-mcp_windows_amd64.exe" }
$u = "https://github.com/vukappa4/WhatsApp-MCP/releases/latest/download"
New-Item -ItemType Directory -Force "$env:TEMP\whatsapp-mcp" | Out-Null
cd "$env:TEMP\whatsapp-mcp"
Invoke-WebRequest "$u/$a" -OutFile whatsapp-mcp.exe -UseBasicParsing
Invoke-WebRequest "$u/checksums.txt" -OutFile checksums.txt -UseBasicParsing
$got = (Get-FileHash whatsapp-mcp.exe -Algorithm SHA256).Hash
if ((Get-Content checksums.txt) -contains "$got  $a") { "SHA-256 OK"; .\whatsapp-mcp.exe version } else { "SHA-256 MISMATCH" }
```

macOS:

```sh
if [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ]; then a=whatsapp-mcp_darwin_arm64; else a=whatsapp-mcp_darwin_amd64; fi
u=https://github.com/vukappa4/WhatsApp-MCP/releases/latest/download
mkdir -p "${TMPDIR:-/tmp}/whatsapp-mcp" && cd "${TMPDIR:-/tmp}/whatsapp-mcp"
curl -fL -o whatsapp-mcp "$u/$a"
curl -fLO "$u/checksums.txt"
if grep -qxF "$(shasum -a 256 whatsapp-mcp | cut -d' ' -f1)  $a" checksums.txt; then echo "SHA-256 OK"; chmod +x whatsapp-mcp; ./whatsapp-mcp version; else echo "SHA-256 MISMATCH"; fi
```

Worked when it prints `SHA-256 OK` and then the release version, like `v0.2.0`. On `SHA-256 MISMATCH`, delete the folder and stop: the download is corrupted or was altered on the way. If Microsoft Defender removes the file or blocks it from running, stop and tell the user. Do not turn Defender off to get past it.

Run steps 3 and 4 from this folder. They use the Windows name `.\whatsapp-mcp.exe`: on macOS write `./whatsapp-mcp`.

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

- Windows: the program goes to `%LOCALAPPDATA%\WhatsAppMCP\bin\whatsapp-mcp.exe`, run by the task "WhatsApp MCP (prod)".
- macOS: the program goes to `~/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp`, run by the LaunchAgent `io.github.vukappa4.whatsapp-mcp.prod`. macOS may show a "Background Items Added" notification: tell the user it is this daemon.

Worked when it prints `Daemon running from ...`.

Every later command runs the installed program. Delete the download folder:

Windows:

```powershell
cd $HOME
Remove-Item -Recurse -Force "$env:TEMP\whatsapp-mcp"
```

macOS:

```sh
cd ~ && rm -rf "${TMPDIR:-/tmp}/whatsapp-mcp"
```

The commands below run the installed program as `& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe"`. On macOS write `"$HOME/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp"` in its place, with the quotes: the path has a space.

## 5. Connect Claude Code

Windows:

```powershell
& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" setup-claude
```

macOS:

```sh
"$HOME/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp" setup-claude
```

It registers the MCP server `whatsapp` at user scope through the `claude` CLI, and adds to the user's Claude Code `settings.json` the allow rules for the read tools and the two hooks that confirm sends. It keeps everything else in that file. Worked when it prints `Restart Claude Code to load them.`

It writes the Claude config folder named by `CLAUDE_CONFIG_DIR`, else `~/.claude`. When the user runs Claude Code with more than one config folder, run it once for each, with `CLAUDE_CONFIG_DIR` set to that folder.

## 6. Voice notes and audio (optional, ask the user)

Transcribing voice notes and sending voice notes need ffmpeg; transcription also needs a speech model (about 650 MB download).

Windows:

```powershell
winget install --id Gyan.FFmpeg -e --accept-source-agreements --accept-package-agreements
& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" setup-transcription
& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" install
```

Without the two `--accept` flags the first winget run waits for an answer no agent can give. Tell the user first: Windows asks them to allow the installer.

macOS (the transcription programs come from Homebrew's `whisper.cpp`, which on Intel Macs builds from source for several minutes):

```sh
brew install ffmpeg whisper.cpp
"$HOME/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp" setup-transcription
"$HOME/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp" install
```

Without Homebrew (`brew --version` fails), the user installs it from https://brew.sh first: its installer asks for their password. After a fresh Homebrew install, run `eval "$(/opt/homebrew/bin/brew shellenv)"` (Intel Macs: `eval "$(/usr/local/bin/brew shellenv)"`) in the same shell before calling `brew`.

The second `install` restarts the daemon so it finds ffmpeg.

## 7. Finish

Tell the user to restart Claude Code, then ask something like "what are my latest WhatsApp chats?". `& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" status` shows pairing, daemon and data state at any time. Logs are in `%LOCALAPPDATA%\WhatsAppMCP\prod\logs\` (macOS: `~/Library/Application Support/WhatsAppMCP/prod/logs/`).

On macOS, tell the user: the first time Claude sends a file from Desktop, Documents or Downloads, macOS may ask whether whatsapp-mcp may access that folder, and they allow it. If a send fails with "macOS may not have given whatsapp-mcp access", they add `~/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp` in System Settings > Privacy & Security > Full Disk Access (press +, then Cmd+Shift+G to type the path), then run `"$HOME/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp" install`. After every update macOS may ask again, and a Full Disk Access entry stops applying: the user removes it and adds it again.

## Link again

WhatsApp unlinks this computer when the phone stays offline for more than 14 days, or when the user removes it under Linked devices. `status` then says the profile is not paired, and the WhatsApp tools answer "not paired" or "logged this device out". Run:

```powershell
& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" stop
& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" pair
```

Start `pair` right after `stop`, in the background as in step 3: the autostart starts the daemon again within minutes, and a `pair` started after that fails because another process holds the profile (run both again). The user scans the QR code with the same WhatsApp number as before: another number in the same profile mixes two accounts' messages. If `pair` says `profile already paired`, run `unlink` with the installed program, then `pair` again. The WhatsApp tools are unavailable until the last step. When `pair` has exited by itself (`History sync quiet ...`), finish with:

```powershell
& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" install
```

Worked when it prints `Daemon running from ...` and `status` shows the account. The messages stored before stay.

## Update

Windows:

```powershell
& "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" update
```

macOS:

```sh
"$HOME/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp" update
```

It compares the installed program's version with the latest release, downloads the release's binary for this computer, checks its SHA-256 against the release's `checksums.txt`, then runs the downloaded binary's `install`: it replaces the installed program and restarts the daemon. Do not close the window while that `install` runs: Ctrl-C does not stop it, but closing the window can, and may leave the daemon stopped. The pairing and the messages stay. Worked when it prints `Updated profile prod: <old> -> <new>.` or `Already up to date: <version>.`

- `update --check` only reports: `Update available: <old> -> <new> ...` or `Already up to date: ...`.
- `update --version v0.3.0` installs that release, also a prerelease or an older one. Before an older one it warns: if the newer version changed the database, the older one does not start and `install` fails after 3 minutes; then run `update` again to go back.
- `update --force` replaces an install built from source (its `version` prints `dev`) or one too old to have a `version` command, reinstalls the same version to repair it, and goes back to the latest release from a newer installed version. Without `--force`, `update` refuses the first two and prints the command to run, and leaves a newer version alone.
- `update` makes one call to GitHub's API. GitHub allows 60 calls an hour per network without login: past that, `update` says when to try again, or download the release by hand as in step 2 and run its `install`. Behind a proxy, set `HTTPS_PROXY`.
- `update` works on the installed profile, whose daemon always uses the profile's own data folder: it refuses `--data-dir`.
- On macOS, `update` ends with a reminder: a Full Disk Access entry for the program stops applying after an update. If the user had added one (step 7), they remove it, add it again, then run `install` with the installed program.
- `update` never changes the Claude Code settings. When they are older than the program, `install` prints `Claude Code settings in <folder> are older than this version: run ...`. Only then, run `setup-claude` again as in step 5, once for every Claude config folder the user uses, and tell the user to restart Claude Code.

### Move from a source install

An install built from source with `go build` reports `dev`, or has no `version` and `update` commands at all when it was built before releases existed. Move it to releases once: download as in step 2, run `.\whatsapp-mcp.exe install` (macOS: `./whatsapp-mcp install`) from that folder, then delete the folder as in step 4. `install` replaces only the program: the same profile keeps its pairing and its messages, and the phone stays linked. When the installed program already has an `update` command, `update --force` does the same in one command. From then on `update` keeps it current. The source folder can be deleted.

## Build from source

Building needs git and Go 1.27 or later.

Windows:

```powershell
git clone https://github.com/vukappa4/WhatsApp-MCP "$HOME\WhatsApp-MCP"
cd "$HOME\WhatsApp-MCP"
$env:CGO_ENABLED = "0"; go build -o whatsapp-mcp.exe ./cmd/whatsapp-mcp
```

macOS:

```sh
git clone https://github.com/vukappa4/WhatsApp-MCP ~/WhatsApp-MCP
cd ~/WhatsApp-MCP
CGO_ENABLED=0 go build -o whatsapp-mcp ./cmd/whatsapp-mcp
```

Then continue at step 3 from that folder. A build from source reports `dev` as its version, so `update` replaces it with a release only with `update --force`.

## Uninstall

`uninstall` deletes the installed program, so these commands run a copy of it.

Windows:

```powershell
Copy-Item "$env:LOCALAPPDATA\WhatsAppMCP\bin\whatsapp-mcp.exe" "$env:TEMP\whatsapp-mcp-uninstall.exe"
& "$env:TEMP\whatsapp-mcp-uninstall.exe" setup-claude --remove
& "$env:TEMP\whatsapp-mcp-uninstall.exe" uninstall
& "$env:TEMP\whatsapp-mcp-uninstall.exe" unlink
Remove-Item "$env:TEMP\whatsapp-mcp-uninstall.exe"
```

macOS:

```sh
cp "$HOME/Library/Application Support/WhatsAppMCP/bin/whatsapp-mcp" "${TMPDIR:-/tmp}/whatsapp-mcp-uninstall"
"${TMPDIR:-/tmp}/whatsapp-mcp-uninstall" setup-claude --remove
"${TMPDIR:-/tmp}/whatsapp-mcp-uninstall" uninstall
"${TMPDIR:-/tmp}/whatsapp-mcp-uninstall" unlink
rm "${TMPDIR:-/tmp}/whatsapp-mcp-uninstall"
```

`setup-claude --remove` takes the server, hooks and allow rules out of Claude Code: run it once for every Claude config folder the user uses, as in step 5. `uninstall` stops the daemon and removes its task or LaunchAgent and its program. `unlink` logs the device out, so the phone no longer lists it. The messages stay in `%LOCALAPPDATA%\WhatsAppMCP` (macOS: `~/Library/Application Support/WhatsAppMCP`) until the user deletes that folder.
