package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// plistValue decodes a plist dict into a map: strings, booleans, integers,
// arrays of strings and nested dicts.
func plistValue(t *testing.T, d *xml.Decoder, start xml.StartElement) any {
	t.Helper()
	switch start.Name.Local {
	case "string", "integer":
		var s string
		if err := d.DecodeElement(&s, &start); err != nil {
			t.Fatal(err)
		}
		return s
	case "true", "false":
		_ = d.Skip()
		return start.Name.Local == "true"
	case "array":
		var out []string
		for {
			tok, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			if se, ok := tok.(xml.StartElement); ok {
				out = append(out, plistValue(t, d, se).(string))
			}
			if _, ok := tok.(xml.EndElement); ok {
				return out
			}
		}
	case "dict":
		out := map[string]any{}
		var k string
		for {
			tok, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			switch v := tok.(type) {
			case xml.StartElement:
				if v.Name.Local == "key" {
					if err := d.DecodeElement(&k, &v); err != nil {
						t.Fatal(err)
					}
				} else {
					out[k] = plistValue(t, d, v)
				}
			case xml.EndElement:
				return out
			}
		}
	}
	t.Fatalf("unexpected plist element %s", start.Name.Local)
	return nil
}

// The LaunchAgent runs the exe itself at login, keeps PATH with Homebrew,
// restarts after a crash only, and gives a send in flight time to finish.
func TestLaunchdPlist(t *testing.T) {
	exe := "/Users/a&b/Library/Application Support/WhatsAppMCP/bin-dev/whatsapp-mcp"
	def := launchdPlist(exe, "/Users/a&b/Library/Application Support/WhatsAppMCP/bin-dev", config.ProfileDev)
	if !bytes.HasPrefix(def, []byte(`<?xml version="1.0" encoding="UTF-8"?>`)) || !bytes.Contains(def, []byte("<!DOCTYPE plist")) {
		t.Fatalf("not a plist:\n%s", def)
	}
	d := xml.NewDecoder(bytes.NewReader(def))
	var root map[string]any
	for root == nil {
		tok, err := d.Token()
		if err == io.EOF {
			t.Fatal("no dict")
		} else if err != nil {
			t.Fatal(err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			root = plistValue(t, d, se).(map[string]any)
		}
	}
	args, _ := root["ProgramArguments"].([]string)
	if strings.Join(args, "|") != exe+"|run|--profile|dev" {
		t.Fatalf("ProgramArguments = %q", args)
	}
	keep, _ := root["KeepAlive"].(map[string]any)
	env, _ := root["EnvironmentVariables"].(map[string]any)
	for k, want := range map[string]any{
		"Label": "io.github.vukappa4.whatsapp-mcp.dev", "RunAtLoad": true, "ThrottleInterval": "60", "ExitTimeOut": "160",
		"WorkingDirectory": "/Users/a&b/Library/Application Support/WhatsAppMCP/bin-dev", "ProcessType": "Interactive",
	} {
		if root[k] != want {
			t.Errorf("%s = %v, want %v", k, root[k], want)
		}
	}
	if keep["SuccessfulExit"] != false {
		t.Errorf("KeepAlive = %v: a clean stop must stay stopped", keep)
	}
	if path, _ := env["PATH"].(string); !strings.HasPrefix(path, "/opt/homebrew/bin:/usr/local/bin:") {
		t.Errorf("PATH = %q", path)
	}
}
