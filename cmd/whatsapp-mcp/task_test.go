package main

import (
	"bytes"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// The task runs the exe itself at logon, with no time limit, on battery,
// restarts on failure and has a 5-minute watchdog that never starts a
// second instance.
func TestTaskXML(t *testing.T) {
	exe := `C:\Users\a&b\AppData\Local\WhatsAppMCP\bin-dev\whatsapp-mcp.exe`
	def, err := taskXML(`PC\mario`, exe, `C:\Users\a&b\AppData\Local\WhatsAppMCP\bin-dev`, config.ProfileDev,
		time.Date(2026, 10, 13, 14, 47, 31, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	s := string(def)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`,
		`<URI>\WhatsApp MCP (dev)</URI>`,
		"<LogonTrigger>\n      <Enabled>true</Enabled>\n      <UserId>PC\\mario</UserId>",
		"<Interval>PT5M</Interval>",
		"<StartBoundary>2026-10-13T14:47:00</StartBoundary>",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<RestartOnFailure>\n      <Interval>PT1M</Interval>\n      <Count>999</Count>",
		`<Command>C:\Users\a&amp;b\AppData\Local\WhatsAppMCP\bin-dev\whatsapp-mcp.exe</Command>`,
		"<Arguments>run --profile dev</Arguments>",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("task XML lacks %q:\n%s", want, s)
		}
	}
	var back schedTask
	if err := xml.NewDecoder(bytes.NewReader(bytes.Replace(def, []byte("UTF-16"), []byte("UTF-8"), 1))).Decode(&back); err != nil {
		t.Fatal(err)
	}
	if back.Actions.Exec.Command != exe {
		t.Fatalf("command round trip = %q", back.Actions.Exec.Command)
	}

	enc := utf16LE([]byte("Aè"))
	if !bytes.Equal(enc[:2], []byte{0xFF, 0xFE}) || string(utf16.Decode([]uint16{uint16(enc[2]) | uint16(enc[3])<<8, uint16(enc[4]) | uint16(enc[5])<<8})) != "Aè" {
		t.Fatalf("utf16LE = % x", enc)
	}
}

// The linked resource carries the current manifest, whose detached console
// policy keeps run windowless: regenerate with `go generate ./cmd/whatsapp-mcp/manifest`.
// The production build (CGO_ENABLED=0) links it.
func TestManifestEmbedded(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("manifest", "whatsapp-mcp.manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(manifest, []byte(`<consoleAllocationPolicy xmlns="http://schemas.microsoft.com/SMI/2024/WindowsSettings">detached</consoleAllocationPolicy>`)) {
		t.Fatal("manifest lacks the detached console allocation policy")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		syso, err := os.ReadFile(filepath.Join("manifest", "rsrc_windows_"+arch+".syso"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(syso, manifest) {
			t.Fatalf("rsrc_windows_%s.syso is stale: run go generate ./cmd/whatsapp-mcp/manifest", arch)
		}
	}
	if testing.Short() || runtime.GOOS != "windows" {
		return
	}
	bin := filepath.Join(t.TempDir(), "whatsapp-mcp.exe")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	exe, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(exe, []byte("<consoleAllocationPolicy")) {
		t.Fatal("the cgo-free build lacks the manifest")
	}
}
