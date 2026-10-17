package logx

import (
	"bytes"
	"strings"
	"testing"
)

// The MCP SDK's per-request Info lines go to Debug; warnings stay.
func TestQuietDemotesInfo(t *testing.T) {
	var info, debug bytes.Buffer
	Quiet(New(&info, false)).With("k", "v").Info("server connecting")
	Quiet(New(&info, false)).Warn("calling tools/call: boom")
	Quiet(New(&debug, true)).Info("server connecting")
	if strings.Contains(info.String(), "server connecting") {
		t.Fatalf("info logger wrote a demoted line: %s", info.String())
	}
	if !strings.Contains(info.String(), "level=WARN") {
		t.Fatalf("warning lost: %s", info.String())
	}
	if !strings.Contains(debug.String(), "level=DEBUG") || !strings.Contains(debug.String(), "server connecting") {
		t.Fatalf("debug logger = %s", debug.String())
	}
	if Quiet(nil) != nil {
		t.Fatal("Quiet(nil) must stay nil")
	}
}
