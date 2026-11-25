package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// TestRealData drives every read tool over HTTP against a copy of a real
// data.db and logs hits, latency and response sizes, never message contents.
// It runs only with WHATSAPP_MCP_E2E_DB set to a data.db path. The database
// is opened read-only and copied with VACUUM INTO first, so even a live
// profile's data.db is never written.
func TestRealData(t *testing.T) {
	src := os.Getenv("WHATSAPP_MCP_E2E_DB")
	if src == "" {
		t.Skip("set WHATSAPP_MCP_E2E_DB to a data.db to run")
	}
	ctx := context.Background()
	dst := filepath.Join(t.TempDir(), "data.db")
	// mode=ro without query_only: VACUUM INTO writes only the destination.
	ro, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(src)}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		t.Fatal(err)
	}
	ro.Close()
	db, err := store.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cs := connect(t, db)

	type probe struct {
		tool string
		args map[string]any
	}
	var probes []probe
	people := func(args map[string]any) { probes = append(probes, probe{toolSearchPeople, args}) }
	search := func(args map[string]any) { probes = append(probes, probe{toolSearchMessages, args}) }
	for _, q := range []string{"marco", "giulia", "andrea", "luca", "ale", "rossi", "nicolo", "333", "+39 3", "347", "p_1"} {
		people(map[string]any{"query": q})
	}
	people(map[string]any{"query": "marco", "response_format": "detailed"})
	for _, q := range []string{"ciao", "grazie", "domani", "perche", "perché", "fattura", "riunione", "lavor*", `"a domani"`,
		"ok", "e-mail: (urgent", `"`, "NOT OR", "a*"} {
		search(map[string]any{"query": q})
	}
	search(map[string]any{"query": "ciao", "sort": "newest"})
	search(map[string]any{"query": "ciao", "context": 3})
	search(map[string]any{"query": "ciao", "context": 1})
	search(map[string]any{"query": "ciao", "response_format": "detailed"})
	search(map[string]any{"query": "ciao", "limit": 50})
	search(map[string]any{"has_media": "voice"})
	search(map[string]any{"has_media": "any", "after": "2026-10-01"})
	search(map[string]any{"from_me": true, "query": "grazie"})
	search(map[string]any{"query": "ciao", "from": "marco"})
	search(map[string]any{"query": "ciao", "in_chat": "c_1"})
	probes = append(probes, probe{toolListChats, map[string]any{}}, probe{toolReadChat, map[string]any{"chat": "c_1"}})

	for _, p := range probes {
		start := time.Now()
		text, isErr := call(t, cs, p.tool, p.args)
		ms := time.Since(start).Milliseconds()
		shown := len(ids(lineIDs, text))
		if p.tool == toolSearchPeople || p.tool == toolListChats {
			shown = len(ids(blockIDs, text))
		}
		total := 0
		if m := totalShown.FindStringSubmatch(text); m != nil {
			total, _ = strconv.Atoi(m[1])
		}
		if strings.Contains(text, "fts5") || strings.Contains(text, "SQL logic error") {
			t.Errorf("%s %v leaked a database error", p.tool, p.args)
		}
		args, _ := json.Marshal(p.args)
		t.Logf("%-24s %-52s err=%-5v shown=%-3d total=%-6d %4d ms %6d chars ~%5d tokens",
			p.tool, args, isErr, shown, total, ms, len(text), len(text)*10/35)
		if os.Getenv("WHATSAPP_MCP_E2E_SHAPE") != "" {
			t.Log(redact(text))
		}
		// Paging works on real data too.
		if cur := cursorIn(nextCursor, text); cur != "" && p.tool == toolSearchMessages && p.args["query"] == "ciao" && len(p.args) == 1 {
			start = time.Now()
			text2, isErr := call(t, cs, p.tool, map[string]any{"cursor": cur})
			page2 := ids(lineIDs, text2)
			seen := map[string]bool{}
			for _, id := range append(ids(lineIDs, text), page2...) {
				seen[id] = true
			}
			t.Logf("  page 2: err=%v shown=%d distinct over both pages=%d %d ms", isErr, len(page2), len(seen), time.Since(start).Milliseconds())
		}
	}
}

// totalShown finds the match count search_messages and search_people state.
var totalShown = regexp.MustCompile(`(\d+) (?:match(?:es)?|people match|person matches)\b`)

var wrapped = regexp.MustCompile(`<untrusted_wa n=\w+>(?s:.*?)</untrusted_wa n=\w+>`)

// redact hides sender-written text so only the shape of a response is logged:
// a block becomes its line count.
func redact(s string) string {
	return wrapped.ReplaceAllStringFunc(s, func(m string) string {
		if n := strings.Count(m, "\n"); n > 1 {
			return fmt.Sprintf("<U: %d lines>", n-1)
		}
		return "<U>"
	})
}
