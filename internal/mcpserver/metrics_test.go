package mcpserver

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// The size report and the benchmarks measure what a calling LLM pays: the
// characters of the tool definitions, the instructions and a fixed probe
// set of read calls on the synthetic fixture, and the time and SQL
// statements of each call. Run them with:
//
//	go test ./internal/mcpserver -run TestSizeReport -v
//	go test ./internal/mcpserver -run '^$' -bench . -benchtime 20x
//
// Tokens are estimated as chars / 3.5; chars are Unicode code points.

// probe is one read tool call of the fixed probe set.
type probe struct {
	name string // tool/case/format
	tool string
	call func(context.Context, *Server) (*mcp.CallToolResult, error)
}

// probes builds the fixed probe set: every read tool in concise and
// detailed mode (status has no modes).
func probes(tb testing.TB, db *store.DB) []probe {
	tb.Helper()
	newest := func(kind string) string {
		var id int64
		if err := db.QueryRow(`SELECT m.id FROM messages m JOIN media md ON md.id = m.media_id
			WHERE m.kind = ? AND md.state = 'downloaded' ORDER BY m.ts DESC LIMIT 1`, kind).Scan(&id); err != nil {
			tb.Fatalf("probe %s: %v", kind, err)
		}
		return messageID(id)
	}
	voice, image, doc := newest("voice"), newest("image"), newest("document")

	ps := []probe{{name: "status", tool: toolStatus, call: func(ctx context.Context, s *Server) (*mcp.CallToolResult, error) {
		return s.status(ctx, nil, statusIn{})
	}}}
	add := func(tool, name string, call func(f string) func(context.Context, *Server) (*mcp.CallToolResult, error)) {
		for _, f := range []string{"concise", "detailed"} {
			ps = append(ps, probe{name: strings.TrimPrefix(tool, "whatsapp_") + "/" + name + "/" + f, tool: tool, call: call(f)})
		}
	}
	type fn = func(context.Context, *Server) (*mcp.CallToolResult, error)
	listChats := func(in listChatsIn) func(string) fn {
		return func(f string) fn {
			in := in
			in.ResponseFormat = f
			return func(ctx context.Context, s *Server) (*mcp.CallToolResult, error) { return s.listChats(ctx, nil, in) }
		}
	}
	readChat := func(in readChatIn) func(string) fn {
		return func(f string) fn {
			in := in
			in.ResponseFormat = f
			return func(ctx context.Context, s *Server) (*mcp.CallToolResult, error) { return s.readChat(ctx, nil, in) }
		}
	}
	searchPeople := func(in searchPeopleIn) func(string) fn {
		return func(f string) fn {
			in := in
			in.ResponseFormat = f
			return func(ctx context.Context, s *Server) (*mcp.CallToolResult, error) { return s.searchPeople(ctx, nil, in) }
		}
	}
	searchMessages := func(in searchMessagesIn) func(string) fn {
		return func(f string) fn {
			in := in
			in.ResponseFormat = f
			return func(ctx context.Context, s *Server) (*mcp.CallToolResult, error) {
				return s.searchMessages(ctx, nil, in)
			}
		}
	}
	getMedia := func(in getMediaIn) func(string) fn {
		return func(f string) fn {
			in := in
			in.ResponseFormat = f
			return func(ctx context.Context, s *Server) (*mcp.CallToolResult, error) { return s.getMedia(ctx, nil, in) }
		}
	}
	add(toolListChats, "default", listChats(listChatsIn{}))
	add(toolListChats, "unread", listChats(listChatsIn{Filter: "unread"}))
	add(toolListChats, "query", listChats(listChatsIn{Query: "marco"}))
	add(toolReadChat, "group", readChat(readChatIn{Chat: "Calcetto del giovedì ⚽"}))
	add(toolReadChat, "dm", readChat(readChatIn{Chat: "+39 000 000 0001"}))
	add(toolReadChat, "dm-range", readChat(readChatIn{Chat: "+39 000 000 0002", After: "2026-09-01", Before: "2026-09-20"}))
	add(toolSearchPeople, "name", searchPeople(searchPeopleIn{Query: "marco"}))
	add(toolSearchPeople, "phone", searchPeople(searchPeopleIn{Query: "000 0012"}))
	add(toolSearchMessages, "query", searchMessages(searchMessagesIn{Query: "fattura"}))
	add(toolSearchMessages, "from", searchMessages(searchMessagesIn{From: "+39 000 000 0002"}))
	add(toolSearchMessages, "context", searchMessages(searchMessagesIn{Query: "preventivo domani", Context: 2}))
	add(toolGetMedia, "voice", getMedia(getMediaIn{Message: voice}))
	add(toolGetMedia, "image", getMedia(getMediaIn{Message: image}))
	add(toolGetMedia, "document", getMedia(getMediaIn{Message: doc}))
	return ps
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// dumpProbes prints the tools/list payload and every probe's text in the
// size report:
// go test ./internal/mcpserver -run TestSizeReport -v -args -dump
var dumpProbes = flag.Bool("dump", false, "TestSizeReport: print the tools/list payload and the text of every probe")

func sizeLine(name string, chars int) string {
	return fmt.Sprintf("SIZE %-34s chars %7d  tokens %6d", name, chars, int(float64(chars)/3.5+0.5))
}

// TestSizeReport reports the characters and estimated tokens of the
// tools/list payload, the instructions and every probe, with totals per tool.
func TestSizeReport(t *testing.T) {
	s, _ := fixtureServer(t)
	ctx := context.Background()

	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.NewMCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "size", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(sizeLine("tools_list", utf8.RuneCount(payload)))
	if *dumpProbes {
		t.Logf("---- tools_list\n%s", payload)
	}
	for _, tl := range tools.Tools {
		b, _ := json.Marshal(tl)
		t.Log(sizeLine("tools_list/"+tl.Name, utf8.RuneCount(b)))
	}
	t.Log(sizeLine("instructions", utf8.RuneCountInString(cs.InitializeResult().Instructions)))

	perTool := map[string]int{}
	all, tags := 0, 0
	for _, p := range probes(t, s.DB) {
		res, err := p.call(ctx, s)
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		// The fixture's temp dir has a random length: count it as a fixed name.
		text := strings.ReplaceAll(resultText(res), fixture.dir, `C:\fixture`)
		if res.IsError {
			t.Fatalf("%s returned an error:\n%s", p.name, text)
		}
		if !strings.HasPrefix(text, "[whatsapp: ") {
			t.Fatalf("%s lacks the freshness line:\n%s", p.name, text)
		}
		n := utf8.RuneCountInString(text)
		perTool[p.tool] += n
		all += n
		for _, tag := range wrapperTag.FindAllString(text, -1) {
			tags += utf8.RuneCountInString(tag)
		}
		t.Log(sizeLine("probe/"+p.name, n))
		if *dumpProbes {
			t.Logf("---- %s\n%s", p.name, text)
		}
	}
	names := make([]string, 0, len(perTool))
	for k := range perTool {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		t.Log(sizeLine("probes/"+strings.TrimPrefix(k, "whatsapp_"), perTool[k]))
	}
	t.Log(sizeLine("probes/all", all))
	t.Log(sizeLine("probes/all/untrusted_tags", tags))
}

// wrapperTag matches the opening and closing tags around untrusted text.
var wrapperTag = regexp.MustCompile(`</?untrusted_whatsapp nonce=[0-9a-f]+>`)

// BenchmarkTools times every probe and reports its SQL statements per call.
func BenchmarkTools(b *testing.B) {
	s, n := fixtureServer(b)
	ctx := context.Background()
	for _, p := range probes(b, s.DB) {
		b.Run(p.name, func(b *testing.B) {
			if _, err := p.call(ctx, s); err != nil { // warm the pool and caches
				b.Fatal(err)
			}
			before := n.Load()
			b.ResetTimer()
			for range b.N {
				if _, err := p.call(ctx, s); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(n.Load()-before)/float64(b.N), "queries/op")
		})
	}
}
