package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// launchdLabel is the launchd job that runs the profile's daemon.
func launchdLabel(p config.Profile) string { return "io.github.vukappa4.whatsapp-mcp." + string(p) }

// launchdPATH is the daemon's PATH: launchd's default (/usr/bin:/bin:
// /usr/sbin:/sbin) lacks Homebrew, where the user's own ffmpeg lives.
const launchdPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// launchdThrottle is the least time between two starts: a daemon that
// fails at start is tried again once a minute, as the Windows task does. A
// daemon that ran longer and then crashed restarts at once.
const launchdThrottle = time.Minute

// launchdPlist is the LaunchAgent install writes: the daemon starts at the
// user's login and when the agent is loaded, is restarted when it crashes,
// is killed or exits with an error (at most once a minute), and stays
// stopped after a clean exit (the stop command, or SIGTERM). launchd waits stopTimeout after SIGTERM before
// it kills: enough for a send in flight to finish.
func launchdPlist(exe, workDir string, p config.Profile) []byte {
	var b bytes.Buffer
	str := func(s string) {
		b.WriteString("<string>")
		_ = xml.EscapeText(&b, []byte(s))
		b.WriteString("</string>")
	}
	key := func(k string) { fmt.Fprintf(&b, "\n\t<key>%s</key>", k) }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>`)
	key("Label")
	str(launchdLabel(p))
	key("ProgramArguments")
	b.WriteString("<array>")
	for _, a := range []string{exe, "run", "--profile", string(p)} {
		str(a)
	}
	b.WriteString("</array>")
	key("WorkingDirectory")
	str(workDir)
	key("EnvironmentVariables")
	b.WriteString("<dict><key>PATH</key>")
	str(launchdPATH)
	b.WriteString("</dict>")
	key("RunAtLoad")
	b.WriteString("<true/>")
	key("KeepAlive")
	b.WriteString("<dict><key>SuccessfulExit</key><false/></dict>")
	key("ThrottleInterval")
	fmt.Fprintf(&b, "<integer>%d</integer>", int(launchdThrottle.Seconds()))
	key("ExitTimeOut")
	fmt.Fprintf(&b, "<integer>%d</integer>", int(stopTimeout.Seconds()))
	key("ProcessType")
	str("Interactive")
	b.WriteString("\n</dict>\n</plist>\n")
	return b.Bytes()
}
