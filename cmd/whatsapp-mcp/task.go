package main

import (
	"encoding/xml"
	"fmt"
	"time"
	"unicode/utf16"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// taskName is the Task Scheduler task that runs the profile's daemon.
func taskName(p config.Profile) string { return "WhatsApp MCP (" + string(p) + ")" }

// Task Scheduler schema 1.4 (http://schemas.microsoft.com/windows/2004/02/mit/task),
// only the elements install sets.
type schedTask struct {
	XMLName    xml.Name `xml:"Task"`
	Version    string   `xml:"version,attr"`
	XMLNS      string   `xml:"xmlns,attr"`
	Info       taskInfo `xml:"RegistrationInfo"`
	Triggers   taskTriggers
	Principals struct {
		Principal taskPrincipal
	}
	Settings taskSettings
	Actions  taskActions
}

type taskInfo struct {
	Description string
	URI         string
}

type taskTriggers struct {
	Logon struct {
		Enabled bool
		UserID  string `xml:"UserId"`
	} `xml:"LogonTrigger"`
	Time struct {
		Repetition struct {
			Interval          string
			StopAtDurationEnd bool
		}
		StartBoundary string
		Enabled       bool
	} `xml:"TimeTrigger"`
}

type taskPrincipal struct {
	ID        string `xml:"id,attr"`
	UserID    string `xml:"UserId"`
	LogonType string
	RunLevel  string
}

type taskSettings struct {
	MultipleInstancesPolicy    string
	DisallowStartIfOnBatteries bool
	StopIfGoingOnBatteries     bool
	AllowHardTerminate         bool
	StartWhenAvailable         bool
	RunOnlyIfNetworkAvailable  bool
	IdleSettings               struct {
		StopOnIdleEnd bool
		RestartOnIdle bool
	}
	AllowStartOnDemand bool
	Enabled            bool
	Hidden             bool
	RunOnlyIfIdle      bool
	WakeToRun          bool
	ExecutionTimeLimit string
	Priority           int
	RestartOnFailure   struct {
		Interval string
		Count    int
	}
}

type taskActions struct {
	Context string `xml:",attr"`
	Exec    struct {
		Command          string
		Arguments        string
		WorkingDirectory string
	}
}

// taskXML is the task definition install registers: the daemon starts at the
// user's logon, only while that user is logged on (no stored password), with
// no time limit, on battery too, restarted every minute after a failure (up
// to 999 times), and a trigger every 5 minutes that starts it again when it
// is not running (IgnoreNew: never a second instance). The action runs the
// exe itself; its manifest keeps Windows from creating a console.
func taskXML(user, exe, workDir string, p config.Profile, now time.Time) ([]byte, error) {
	t := schedTask{Version: "1.4", XMLNS: "http://schemas.microsoft.com/windows/2004/02/mit/task"}
	t.Info = taskInfo{
		Description: fmt.Sprintf("WhatsApp MCP daemon, profile %s. Managed by `whatsapp-mcp install|uninstall --profile %s`.", p, p),
		URI:         `\` + taskName(p),
	}
	t.Triggers.Logon.Enabled = true
	t.Triggers.Logon.UserID = user
	t.Triggers.Time.Repetition.Interval = "PT5M"
	t.Triggers.Time.StartBoundary = now.Truncate(time.Minute).Format("2006-01-02T15:04:05")
	t.Triggers.Time.Enabled = true
	t.Principals.Principal = taskPrincipal{ID: "Author", UserID: user, LogonType: "InteractiveToken", RunLevel: "LeastPrivilege"}
	s := &t.Settings
	s.MultipleInstancesPolicy = "IgnoreNew"
	s.AllowHardTerminate = true
	s.StartWhenAvailable = true
	s.AllowStartOnDemand = true
	s.Enabled = true
	s.ExecutionTimeLimit = "PT0S"
	s.Priority = 5 // normal: MCP answers Claude interactively; transcription lowers its own children
	s.RestartOnFailure.Interval = "PT1M"
	s.RestartOnFailure.Count = 999
	t.Actions.Context = "Author"
	t.Actions.Exec.Command = exe
	t.Actions.Exec.Arguments = "run --profile " + string(p)
	t.Actions.Exec.WorkingDirectory = workDir
	body, err := xml.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(`<?xml version="1.0" encoding="UTF-16"?>`+"\n"), body...), nil
}

// utf16LE encodes s as UTF-16 little endian with a byte order mark, the
// encoding schtasks /XML expects.
func utf16LE(s []byte) []byte {
	u := utf16.Encode([]rune(string(s)))
	out := make([]byte, 0, 2+2*len(u))
	out = append(out, 0xFF, 0xFE)
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}
