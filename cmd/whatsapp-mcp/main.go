// Command whatsapp-mcp is the WhatsApp linked-device daemon that serves MCP to Claude Code.
//
// On Windows it is a console program whose manifest sets
// consoleAllocationPolicy "detached": started from a terminal it uses that
// terminal like any CLI; started without one (Task Scheduler) Windows
// creates no console, so run never shows a window.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/vukappa4/whatsapp-mcp/internal/childproc"
	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/instance"
	"github.com/vukappa4/whatsapp-mcp/internal/logx"
	"github.com/vukappa4/whatsapp-mcp/internal/mcpserver"
	"github.com/vukappa4/whatsapp-mcp/internal/media"
	"github.com/vukappa4/whatsapp-mcp/internal/pairpage"
	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `whatsapp-mcp: WhatsApp linked device serving MCP on 127.0.0.1

Usage:
  whatsapp-mcp run           [--profile prod|dev] [--transcriber parakeet|whisper|off]
                                                    start the daemon (MCP on 127.0.0.1:8765 prod, 8766 dev)
  whatsapp-mcp pair          [--profile prod|dev] [--no-browser | --phone <+39 333 123 4567>]
                                                    link this profile to the phone: by a QR code on a page it
                                                    opens in the browser (--no-browser: in this terminal), or
                                                    with --phone by a pairing code entered on that phone
  whatsapp-mcp unlink        [--profile prod|dev]   log this linked device out of WhatsApp (daemon stopped; data kept)
  whatsapp-mcp status        [--profile prod|dev]   show pairing, daemon and data state
  whatsapp-mcp stop          [--profile prod|dev]   stop the running daemon cleanly (hard kill after --timeout)
  whatsapp-mcp reingest      [--profile prod|dev]   rebuild data.db from the raw event archive (daemon stopped)
  whatsapp-mcp import-legacy [--profile prod|dev] --messages <messages.db> --session <whatsapp.db>
                                                    import the old bridge's data (read-only; use copies)
  whatsapp-mcp setup-transcription [--engine parakeet|whisper|all] [--source-dir <dir>]...
                                                    install the transcription model into <app dir>/tools, and on
                                                    Windows the pinned whisper.cpp tools (SHA-256 verified)
  whatsapp-mcp install       [--profile prod|dev]   copy this binary to <app dir>/bin (prod) or bin-dev (dev),
                                                    register the autostart (Windows: logon task; macOS:
                                                    LaunchAgent) and (re)start the daemon
  whatsapp-mcp uninstall     [--profile prod|dev]   remove the autostart and the installed binary, stop the daemon (data kept)
  whatsapp-mcp setup-claude  [--profile prod|dev] [--remove]
                                                    register the installed daemon with Claude Code: the MCP server
                                                    (user scope, via the claude CLI), the read-tool allow rules and
                                                    the send-gate hooks in settings.json; --remove takes them out.
                                                    Writes the config dir in CLAUDE_CONFIG_DIR, else ~/.claude:
                                                    run it once per config dir
  whatsapp-mcp hook user-prompt                     Claude Code UserPromptSubmit hook (JSON on stdin)
  whatsapp-mcp hook pre-tool                        Claude Code PreToolUse hook for the send tools
  whatsapp-mcp version                              print the version: the release tag (v0.2.0) or dev

Common flags:
  --profile   prod (default) or dev
  --data-dir  override <app dir>/<profile>
  --debug     verbose logging

App dir: %LOCALAPPDATA%\WhatsAppMCP on Windows, ~/Library/Application Support/WhatsAppMCP on macOS.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = runCmd(args)
	case "pair":
		err = pairCmd(args)
	case "unlink":
		err = unlinkCmd(args)
	case "status":
		err = statusCmd(args)
	case "import-legacy":
		err = importCmd(args)
	case "reingest":
		err = reingestCmd(args)
	case "stop":
		err = stopCmd(args)
	case "setup-transcription":
		err = setupTranscriptionCmd(args)
	case "install":
		err = installCmd(args)
	case "uninstall":
		err = uninstallCmd(args)
	case "setup-claude":
		err = setupClaudeCmd(args)
	case "hook":
		os.Exit(hookCmd(args, os.Stdin, os.Stdout))
	case "version", "--version", "-v":
		err = versionCmd(args, os.Stdout)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// versionCmd prints the version stamped at build time: the release tag
// (v0.2.0, v0.2.0-rc.1), or dev for a local build.
func versionCmd(args []string, out io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %v", args)
	}
	_, err := fmt.Fprintln(out, version)
	return err
}

type common struct {
	profile, dataDir string
	debug            bool
}

func flags(name string, args []string, extra func(*flag.FlagSet)) (*config.Config, common, error) {
	var c common
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&c.profile, "profile", "prod", "prod or dev")
	fs.StringVar(&c.dataDir, "data-dir", "", "data directory override")
	fs.BoolVar(&c.debug, "debug", false, "verbose logging")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, c, err
	}
	if fs.NArg() > 0 {
		return nil, c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	p, err := config.ParseProfile(c.profile)
	if err != nil {
		return nil, c, err
	}
	cfg, err := config.New(p, c.dataDir)
	if err != nil {
		return nil, c, err
	}
	return cfg, c, cfg.EnsureDirs()
}

// openLog writes to the rotated log file and, when one exists, the console.
func openLog(cfg *config.Config, debug bool) (*slog.Logger, func(), error) {
	f, err := logx.OpenRotating(cfg.LogDir())
	if err != nil {
		return nil, nil, err
	}
	// File first: MultiWriter stops at the first failing writer, and a GUI build has no stderr.
	// Every process of a profile shares the file: the pid tells their lines apart.
	log := logx.New(io.MultiWriter(f, os.Stderr), debug).With("profile", cfg.Profile, "pid", os.Getpid())
	return log, func() { f.Close() }, nil
}

// acquire takes the profile lock for cmd. A refusal is logged as this
// process's (its pid is on the line) and says that the process holding the
// lock, usually the running daemon, is unaffected.
func acquire(cfg *config.Config, log *slog.Logger, cmd, hint string) (*instance.Lock, error) {
	lock, err := instance.Acquire(string(cfg.Profile))
	if err != nil {
		if cmd != "run" || refusalLogDue(cfg, time.Now()) {
			log.Warn(cmd+" refused: another whatsapp-mcp process holds this profile and keeps running unaffected", "err", err)
		}
		if hint != "" {
			err = fmt.Errorf("%w: %s", err, hint)
		}
		return nil, err
	}
	return lock, nil
}

// refusalLogDue reports whether a refused run may log now: the autostart
// retries a failed start every minute (on Windows also every 5 minutes as
// watchdog), so while another process (reingest, pair) holds the profile
// the refusal is logged at most once an hour. The mark is a file in the log
// dir.
func refusalLogDue(cfg *config.Config, now time.Time) bool {
	mark := filepath.Join(cfg.LogDir(), "run-refused.mark")
	if fi, err := os.Stat(mark); err == nil && now.Sub(fi.ModTime()) < time.Hour {
		return false
	}
	if err := os.WriteFile(mark, nil, 0o600); err == nil {
		_ = os.Chtimes(mark, now, now)
	}
	return true
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runCmd(args []string) error {
	transcriber := media.EngineParakeet
	cfg, c, err := flags("run", args, func(fs *flag.FlagSet) {
		fs.StringVar(&transcriber, "transcriber", media.EngineParakeet, "voice-note transcription engine: parakeet, whisper or off")
	})
	if err != nil {
		return err
	}
	toolsDir, err := config.ToolsDir()
	if err != nil {
		return err
	}
	tr, err := media.NewTranscriber(transcriber, toolsDir, config.FFmpegCandidates())
	if err != nil {
		return err
	}
	log, closeLog, err := openLog(cfg, c.debug)
	if err != nil {
		return err
	}
	defer closeLog()
	lock, err := acquire(cfg, log, "run", "")
	if err != nil {
		return err
	}
	defer lock.Release()
	// The pid goes out as soon as the lock is ours: install and stop find
	// this process even while migrations or a resumed reingest run.
	pidFile := filepath.Join(cfg.DataDir, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	defer os.Remove(pidFile)
	if err := childproc.KillChildrenOnExit(); err != nil {
		log.Warn("transcription processes may outlive the daemon", "err", err)
	}

	ctx, cancel := signalContext()
	defer cancel()
	token, err := cfg.EnsureToken()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	db, err := store.Open(ctx, cfg.DataDB())
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info("daemon starting", "version", version, "data_dir", cfg.DataDir, "port", cfg.Port)

	// Bind the port before touching the WhatsApp session: a second instance stops here.
	ln, err := mcpserver.Listen(cfg.Port)
	if err != nil {
		return err
	}

	life, err := wa.BeginLifecycle(ctx, db, log, "run", version)
	if err != nil {
		ln.Close()
		return err
	}
	client, err := wa.NewClient(ctx, cfg.SessionDB(), db, log)
	if err != nil {
		ln.Close()
		life.End(nil, "error: "+err.Error())
		return err
	}
	// A reingest that never finished left data.db half built: finish it
	// before serving anything (MCP requests wait on the bound port).
	if _, err := client.Ingestor().ResumeReingest(ctx, client.SessionDB()); err != nil {
		ln.Close()
		life.End(client, "error: "+err.Error())
		return err
	}
	mgr := media.New(media.Options{DB: db, Root: cfg.MediaDir(), Source: media.WhatsAppSource{Client: client.WhatsApp},
		Transcriber: tr, Log: log.With("component", "media")})
	client.SetMediaSink(mgr)
	if err := client.Start(); err != nil {
		ln.Close()
		life.End(client, "error: "+err.Error())
		return err
	}
	mgr.Start()
	if tr != nil {
		if err := tr.Check(); err != nil {
			log.Warn("transcription not ready", "engine", tr.Engine(), "detail", err)
		} else {
			ff, _ := childproc.FFmpeg()
			log.Info("transcription ready", "engine", tr.Engine(), "ffmpeg", ff)
		}
	}

	if n, err := mcpserver.ResolveUnconfirmedSends(ctx, db); err != nil {
		log.Warn("resolve unconfirmed sends", "err", err)
	} else if n > 0 {
		log.Info("unconfirmed sends found stored: recorded as sent", "count", n)
	}

	reason := "signal"
	srv := &mcpserver.Server{DB: db, WA: client, Media: mgr, Profile: string(cfg.Profile), DataDir: cfg.DataDir, Version: version, Log: log,
		Stop: func() { reason = "stop command"; cancel() }, Sender: client, Ingest: client.Ingestor()}
	log.Info("serving MCP", "url", fmt.Sprintf("http://127.0.0.1:%d/mcp", cfg.Port))
	serveErr := srv.Serve(ctx, ln, token, cfg.Port)
	if serveErr != nil {
		log.Error("MCP server stopped", "err", serveErr)
		reason = "error: " + serveErr.Error()
	}
	// A send already on the wire finishes, and is stored, before WhatsApp
	// disconnects; new sends are refused meanwhile.
	if !srv.Drain(sendDrainTimeout) {
		log.Warn("a send was still running at shutdown", "waited", sendDrainTimeout)
	}
	cancel()
	mgr.Stop()
	life.End(client, reason)
	log.Info("daemon stopped", "reason", reason)
	return serveErr
}

// qrRounds is how many QR logins pair runs, about 160 s each, before it
// gives up: about 8 minutes for the user to reach the phone.
const qrRounds = 3

// openPairPage serves the QR code page and opens it in the browser. It
// returns nil, and pair falls back to the terminal QR, when either fails.
func openPairPage(log *slog.Logger) *pairpage.Page {
	page, err := pairpage.Start()
	if err != nil {
		log.Warn("QR page not available: showing the QR code in the terminal", "err", err)
		return nil
	}
	if err := pairpage.Open(page.URL); err != nil {
		log.Warn("could not open the browser", "err", err)
		fmt.Printf("Open this page in a browser to scan the QR code: %s\n", page.URL)
	} else {
		fmt.Printf("The QR code is open in the browser: %s\n", page.URL)
	}
	return page
}

func pairCmd(args []string) (err error) {
	var phoneArg string
	var noBrowser bool
	cfg, c, err := flags("pair", args, func(fs *flag.FlagSet) {
		fs.StringVar(&phoneArg, "phone", "", "the phone's number with its country code: show a pairing code for it instead of QR codes")
		fs.BoolVar(&noBrowser, "no-browser", false, "show the QR code in this terminal instead of a browser page")
	})
	if err != nil {
		return err
	}
	phone, err := internationalPhone(phoneArg)
	if err != nil {
		return err
	}
	log, closeLog, err := openLog(cfg, c.debug)
	if err != nil {
		return err
	}
	defer closeLog()
	lock, err := acquire(cfg, log, "pair", "stop the daemon (`whatsapp-mcp stop --profile "+string(cfg.Profile)+"`), then pair at once: its autostart starts it again within minutes")
	if err != nil {
		return err
	}
	defer lock.Release()
	ctx, cancel := signalContext()
	defer cancel()
	db, err := store.Open(ctx, cfg.DataDB())
	if err != nil {
		return err
	}
	defer db.Close()

	wa.ConfigurePairing("WhatsApp MCP " + string(cfg.Profile))
	client, err := wa.NewClient(ctx, cfg.SessionDB(), db, log)
	if err != nil {
		return err
	}
	// Pairing connects like run, so it records the same lifecycle: without
	// heartbeats and a clean stop, the next start would show the whole
	// pairing session as offline.
	life, err := wa.BeginLifecycle(ctx, db, log, "pair", version)
	if err != nil {
		client.Stop()
		return err
	}
	opts := wa.PairOptions{Phone: phone, Rounds: qrRounds}
	if phone == "" && !noBrowser {
		if page := openPairPage(log); page != nil {
			defer page.Close()
			opts.QR = page.QR
			opts.Paired = func(digits string) { page.Paired(people.FormatPhone("+" + digits)) }
			defer func() {
				if err != nil {
					page.Failed(err.Error())
				} else {
					page.Done()
				}
			}()
		}
	}
	err = client.Pair(os.Stdout, 2*time.Minute, opts)
	reason := "pairing done"
	if err != nil {
		reason = "error: " + err.Error()
	} else if ctx.Err() != nil {
		reason = "signal"
	}
	life.End(client, reason)
	return err
}

// internationalPhone turns a phone number written with its country code
// (+39 333 123 4567, 0039 333...) into its digits. Empty stays empty. A
// number copied from WhatsApp carries no-break spaces, which count as
// spaces, and direction marks, which are dropped.
func internationalPhone(s string) (string, error) {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Bidi_Control, r):
			return -1
		case unicode.IsSpace(r):
			return ' '
		}
		return r
	}, s))
	if s == "" {
		return "", nil
	}
	bad := fmt.Errorf("phone %q: write it with the country code, like +39 333 123 4567", s)
	var digits strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case strings.ContainsRune(" +-.()", r):
		default:
			return "", bad
		}
	}
	d := digits.String()
	switch {
	case strings.HasPrefix(s, "+") && !strings.Contains(s[1:], "+"):
	case strings.HasPrefix(s, "00") && !strings.Contains(s, "+"):
		d = d[2:]
	default:
		return "", bad
	}
	if len(d) < 8 || len(d) > 15 || d[0] == '0' {
		return "", bad
	}
	return d, nil
}

// unlinkCmd logs the profile's linked device out, so the phone no longer
// lists it. The daemon must be stopped; the data stays.
func unlinkCmd(args []string) error {
	cfg, c, err := flags("unlink", args, nil)
	if err != nil {
		return err
	}
	log, closeLog, err := openLog(cfg, c.debug)
	if err != nil {
		return err
	}
	defer closeLog()
	lock, err := acquire(cfg, log, "unlink", "stop the daemon before unlinking (uninstall, so its autostart does not start it again)")
	if err != nil {
		return err
	}
	defer lock.Release()
	ctx, cancel := signalContext()
	defer cancel()
	db, err := store.Open(ctx, cfg.DataDB())
	if err != nil {
		return err
	}
	defer db.Close()
	client, err := wa.NewClient(ctx, cfg.SessionDB(), db, log)
	if err != nil {
		return err
	}
	defer client.Stop()
	if err := client.Unlink(time.Minute); errors.Is(err, wa.ErrNotLinked) {
		fmt.Println("Not linked: the phone does not list this device. Nothing to do.")
		return nil
	} else if err != nil {
		return err
	}
	log.Info("linked device logged out")
	fmt.Printf("Logged out: the phone no longer lists this device. Data kept in %s.\n", cfg.DataDir)
	return nil
}

func statusCmd(args []string) error {
	cfg, _, err := flags("status", args, nil)
	if err != nil {
		return err
	}
	ctx := context.Background()
	fmt.Printf("Profile:  %s\nData dir: %s\nMCP URL:  http://127.0.0.1:%d/mcp\nToken:    %s\n", cfg.Profile, cfg.DataDir, cfg.Port, cfg.TokenFile())
	running := "no"
	switch instance.Probe(string(cfg.Profile)) {
	case instance.HeldHere:
		running = "yes"
	case instance.HeldInaccessible:
		running = "yes, by a process this user cannot inspect (another user, or an elevated process): stop it from there"
	}
	fmt.Printf("Daemon:   running=%s\n", running)
	fmt.Printf("Startup:  %s\n", serviceState(cfg.Profile))

	paired := "no - to link: `whatsapp-mcp stop`, then `whatsapp-mcp pair`, then `whatsapp-mcp install`, each with --profile " + string(cfg.Profile) + " (INSTALL.md, Link again)"
	if _, err := os.Stat(cfg.SessionDB()); err == nil {
		sdb, err := sql.Open("sqlite", store.ReadOnlyDSN(cfg.SessionDB()))
		if err == nil {
			ids, err := wa.ReadSession(ctx, sdb)
			if err == nil && !ids.OwnPN.IsEmpty() {
				paired = "yes, as " + people.FormatPhone("+"+ids.OwnPN.User)
			} else if err != nil {
				paired = "unknown: " + err.Error()
			}
			sdb.Close()
		}
	}
	fmt.Printf("Paired:   %s\n", paired)

	if _, err := os.Stat(cfg.DataDB()); errors.Is(err, os.ErrNotExist) {
		fmt.Println("Data:     data.db not created yet")
		return nil
	}
	ddb, err := sql.Open("sqlite", store.ReadOnlyDSN(cfg.DataDB()))
	if err != nil {
		return err
	}
	defer ddb.Close()
	st, err := store.GetStats(ctx, ddb)
	if err != nil {
		return fmt.Errorf("read data.db: %w", err)
	}
	fmt.Printf("Data:     %d messages, %d chats, %d persons; %d chats without a human name\n", st.Messages, st.Chats, st.Persons, st.UnnamedChats)
	if interrupted, _ := store.ReingestInterrupted(ctx, ddb); interrupted {
		fmt.Println("Reingest: INTERRUPTED - data.db is incomplete; the daemon finishes the rebuild when it starts")
	}
	if st.NewestTS != 0 {
		fmt.Printf("Range:    %s to %s\n", time.Unix(st.OldestTS, 0).Format(time.RFC3339), time.Unix(st.NewestTS, 0).Format(time.RFC3339))
	}
	evs, err := store.RecentConnections(ctx, ddb, 5)
	if err != nil {
		return err
	}
	if len(evs) > 0 {
		fmt.Println("Recent connection events:")
		for _, e := range evs {
			fmt.Printf("  %s %s %s\n", e.TS.Format(time.RFC3339), e.Event, e.Detail)
		}
	}
	return nil
}

func importCmd(args []string) error {
	var messages, session string
	cfg, c, err := flags("import-legacy", args, func(fs *flag.FlagSet) {
		fs.StringVar(&messages, "messages", "", "legacy messages.db (a copy)")
		fs.StringVar(&session, "session", "", "legacy whatsapp.db session (a copy)")
	})
	if err != nil {
		return err
	}
	if messages == "" || session == "" {
		return errors.New("--messages and --session are both required")
	}
	for _, p := range []string{messages, session} {
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	log, closeLog, err := openLog(cfg, c.debug)
	if err != nil {
		return err
	}
	defer closeLog()
	lock, err := acquire(cfg, log, "import-legacy", "stop the daemon before importing")
	if err != nil {
		return err
	}
	defer lock.Release()
	ctx, cancel := signalContext()
	defer cancel()
	db, err := store.Open(ctx, cfg.DataDB())
	if err != nil {
		return err
	}
	defer db.Close()
	start := time.Now()
	st, err := wa.ImportLegacy(ctx, db, messages, session)
	if err != nil {
		return err
	}
	log.Info("legacy import done", "imported", st.MessagesImported, "archived_new", st.ArchivedNew, "skipped", st.MessagesSkipped)
	fmt.Printf(`Legacy import into %s (%s)
Legacy:   %d chats (%d without a human name), %d messages, %d LID links, %d contacts
DM merge: %d legacy DM chats, %d folded into another JID's chat of the same person
Imported: %d chats, %d messages (%d new raw events archived, %d legacy rows skipped)
Data now: %d persons, %d chats, %d messages; %d chats without a human name
`, cfg.DataDB(), time.Since(start).Round(time.Millisecond),
		st.LegacyChats, st.LegacyUnnamedChats, st.LegacyMessages, st.LIDLinks, st.Contacts,
		st.LegacyDMJIDs, st.MergedDMChats,
		st.ChatsImported, st.MessagesImported, st.ArchivedNew, st.MessagesSkipped,
		st.Persons, st.Chats, st.Messages, st.Unnamed)
	return nil
}

func reingestCmd(args []string) error {
	cfg, c, err := flags("reingest", args, nil)
	if err != nil {
		return err
	}
	log, closeLog, err := openLog(cfg, c.debug)
	if err != nil {
		return err
	}
	defer closeLog()
	lock, err := acquire(cfg, log, "reingest", "stop the daemon before reingesting")
	if err != nil {
		return err
	}
	defer lock.Release()
	ctx, cancel := signalContext()
	defer cancel()
	db, err := store.Open(ctx, cfg.DataDB())
	if err != nil {
		return err
	}
	defer db.Close()
	client, err := wa.NewClient(ctx, cfg.SessionDB(), db, log)
	if err != nil {
		return err
	}
	defer client.Stop()
	start := time.Now()
	n, err := client.Ingestor().Reingest(ctx, client.SessionDB())
	if err != nil {
		return err
	}
	st, err := store.GetStats(ctx, db)
	if err != nil {
		return err
	}
	fmt.Printf("Reingested %d raw events in %s: %d persons, %d chats, %d messages; %d chats without a human name\n",
		n, time.Since(start).Round(time.Millisecond), st.Persons, st.Chats, st.Messages, st.UnnamedChats)
	return nil
}

func stopCmd(args []string) error {
	var timeout time.Duration
	cfg, _, err := flags("stop", args, func(fs *flag.FlagSet) {
		fs.DurationVar(&timeout, "timeout", stopTimeout, "how long to wait for a clean stop before killing (a send in flight takes up to 2 minutes)")
	})
	if err != nil {
		return err
	}
	if err := stopDaemon(cfg, timeout, os.Stdout); err != nil {
		return err
	}
	if serviceInstalled(cfg.Profile) {
		fmt.Printf("%s: run `whatsapp-mcp uninstall --profile %s` to keep it stopped.\n", serviceRestartNote(cfg.Profile), cfg.Profile)
	}
	return nil
}

// sendDrainTimeout is how long shutdown waits for a send in flight: a send
// and its storage are bounded by 2 minutes.
const sendDrainTimeout = 2*time.Minute + 10*time.Second

// stopTimeout is how long stop and install wait for a clean stop before
// killing: enough for a send in flight to finish.
const stopTimeout = sendDrainTimeout + 30*time.Second

// stopDaemon asks the profile's daemon to stop cleanly and kills it after
// timeout. A daemon that is not running is not an error.
func stopDaemon(cfg *config.Config, timeout time.Duration, out io.Writer) error {
	if !instance.Held(string(cfg.Profile)) {
		fmt.Fprintln(out, "Daemon is not running.")
		return nil
	}
	// The lock is released before the process ends: wait for the process
	// itself, so its exe can be replaced or deleted right after.
	pid, pidErr := daemonPID(cfg)
	gone := func(d time.Duration) {
		if pidErr == nil {
			waitExit(pid, d)
		}
	}
	token, err := cfg.ReadToken()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	// A daemon still starting (migrations, a resumed reingest) does not
	// listen yet: ask again until it does, or ends, or the timeout passes.
	err = postStop(cfg.Port, token)
	for err != nil && time.Now().Before(deadline) {
		if !instance.Held(string(cfg.Profile)) {
			gone(time.Until(deadline) + 5*time.Second)
			fmt.Fprintln(out, "Daemon stopped.")
			return nil
		}
		time.Sleep(time.Second)
		err = postStop(cfg.Port, token)
	}
	if err != nil {
		fmt.Fprintln(out, "Clean stop request failed:", err)
	} else {
		for time.Now().Before(deadline) {
			if !instance.Held(string(cfg.Profile)) {
				gone(time.Until(deadline) + 5*time.Second)
				fmt.Fprintln(out, "Daemon stopped cleanly.")
				return nil
			}
			time.Sleep(200 * time.Millisecond)
		}
		fmt.Fprintf(out, "Daemon still running after %s.\n", timeout)
	}
	if pidErr != nil {
		return fmt.Errorf("cannot hard-kill: %w", pidErr)
	}
	img, ierr := processImage(pid)
	switch {
	case errors.Is(ierr, errors.ErrUnsupported):
	case ierr != nil:
		return fmt.Errorf("cannot check pid %d from daemon.pid before killing it: %w", pid, ierr)
	case !strings.HasPrefix(strings.ToLower(filepath.Base(img)), "whatsapp-mcp"):
		return fmt.Errorf("pid %d in daemon.pid is %s, not the daemon: not killing it", pid, img)
	}
	p, ferr := os.FindProcess(pid)
	if ferr != nil {
		return ferr
	}
	if kerr := p.Kill(); kerr != nil {
		return fmt.Errorf("kill pid %d: %w", pid, kerr)
	}
	gone(10 * time.Second)
	fmt.Fprintf(out, "Killed pid %d; the next start records an unclean exit.\n", pid)
	return nil
}

// postStop asks the daemon on port to stop cleanly.
func postStop(port int, token string) error {
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/admin/stop", port), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("stop endpoint answered %s", resp.Status)
	}
	return nil
}

// daemonPID reads the pid the running daemon wrote at startup.
func daemonPID(cfg *config.Config) (int, error) {
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "daemon.pid"))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("bad pid file: %w", err)
	}
	return pid, nil
}

// setupTranscriptionCmd installs the transcription tools shared by every
// profile. It touches no profile data.
func setupTranscriptionCmd(args []string) error {
	var engine string
	var sources []string
	fs := flag.NewFlagSet("setup-transcription", flag.ContinueOnError)
	fs.StringVar(&engine, "engine", media.EngineParakeet, "parakeet, whisper or all")
	fs.Func("source-dir", "folder with already downloaded copies to use instead of downloading (repeatable)", func(s string) error {
		sources = append(sources, s)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	dir, err := config.ToolsDir()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	fmt.Println("Installing transcription tools into", dir)
	if err := media.Setup(ctx, media.SetupOptions{ToolsDir: dir, Engine: engine, SourceDirs: sources, FFmpegCandidates: config.FFmpegCandidates(), Out: os.Stdout}); err != nil {
		return err
	}
	fmt.Println("Done. Restart the daemon if ffmpeg was just installed; otherwise the next voice note is transcribed.")
	return nil
}
