package mcpserver

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxPreparedTexts bounds the statements the read tools prepare: half the
// store's statement cache, so they never evict each other.
const maxPreparedTexts = 64

var anyCursor = regexp.MustCompile(`cursor="([^"]+)"`)

// No query text depends on a page size or on how many rows a step returns:
// lists go in as one JSON argument. The read tools then prepare a fixed set
// of statements, and the statement cache never churns on new variants.
func TestPreparedStatementsFixed(t *testing.T) {
	prepared := &preparedTexts{}
	db, _ := fixtureDBPreparing(t, prepared)
	s := serveFixture(t, db)
	ctx := context.Background()
	var around int64
	if err := db.QueryRow(`SELECT m.id FROM messages m JOIN chats c ON c.id = m.chat_id
		WHERE c.kind = 'group' ORDER BY m.ts LIMIT 1 OFFSET 200`).Scan(&around); err != nil {
		t.Fatal(err)
	}

	type call func(limit int, cursor string) (*mcp.CallToolResult, error)
	listChats := func(in listChatsIn) call {
		return func(limit int, cursor string) (*mcp.CallToolResult, error) {
			in := in
			in.Limit, in.Cursor = limit, cursor
			return s.listChats(ctx, nil, in)
		}
	}
	readChat := func(in readChatIn) call {
		return func(limit int, cursor string) (*mcp.CallToolResult, error) {
			in := in
			in.Limit, in.Cursor = limit, cursor
			if cursor != "" {
				in.Chat, in.Around = "", ""
			}
			return s.readChat(ctx, nil, in)
		}
	}
	searchPeople := func(in searchPeopleIn) call {
		return func(limit int, cursor string) (*mcp.CallToolResult, error) {
			in := in
			in.Limit, in.Cursor = limit, cursor
			if cursor != "" {
				in.Query = ""
			}
			return s.searchPeople(ctx, nil, in)
		}
	}
	searchMessages := func(in searchMessagesIn) call {
		return func(limit int, cursor string) (*mcp.CallToolResult, error) {
			in := in
			in.Limit, in.Cursor = limit, cursor
			if cursor != "" {
				in = searchMessagesIn{Limit: limit, Cursor: cursor, ResponseFormat: in.ResponseFormat}
			}
			return s.searchMessages(ctx, nil, in)
		}
	}
	var calls []call
	for _, f := range []string{"concise", "detailed"} {
		calls = append(calls,
			listChats(listChatsIn{ResponseFormat: f}),
			listChats(listChatsIn{Filter: "unread", ResponseFormat: f}),
			listChats(listChatsIn{Filter: "group", ResponseFormat: f}),
			listChats(listChatsIn{Query: "marco", ResponseFormat: f}),
			readChat(readChatIn{Chat: "Calcetto del giovedì ⚽", ResponseFormat: f}),
			readChat(readChatIn{Chat: "+39 000 000 0001", ResponseFormat: f}),
			readChat(readChatIn{Around: messageID(around), ResponseFormat: f}),
			searchPeople(searchPeopleIn{Query: "marco", ResponseFormat: f}),
			searchPeople(searchPeopleIn{Query: "a", ResponseFormat: f}),
			searchPeople(searchPeopleIn{Query: "000 0012", ResponseFormat: f}),
			searchMessages(searchMessagesIn{Query: "fattura", ResponseFormat: f}),
			searchMessages(searchMessagesIn{Query: "fattura", Sort: "newest", ResponseFormat: f}),
			searchMessages(searchMessagesIn{Query: "preventivo domani", Context: 1, ResponseFormat: f}),
			searchMessages(searchMessagesIn{Query: "preventivo", Context: 3, ResponseFormat: f}),
			searchMessages(searchMessagesIn{From: "+39 000 000 0002", ResponseFormat: f}),
			searchMessages(searchMessagesIn{HasMedia: "any", ResponseFormat: f}),
			searchMessages(searchMessagesIn{HasMedia: "voice", ResponseFormat: f}),
		)
	}
	// sweep runs every call at this page size, then once more from the
	// cursor it returns.
	sweep := func(limit int) {
		t.Helper()
		for i, c := range calls {
			res, err := c(limit, "")
			if err != nil || res.IsError {
				t.Fatalf("call %d at limit %d: %v %s", i, limit, err, resultText(res))
			}
			cur := cursorIn(anyCursor, resultText(res))
			if cur == "" {
				continue
			}
			if res, err = c(limit, cur); err != nil || res.IsError {
				t.Fatalf("call %d at limit %d from its cursor: %v %s", i, limit, err, resultText(res))
			}
		}
	}

	for _, p := range probes(t, db) {
		if _, err := p.call(ctx, s); err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
	}
	sweep(5)
	base := prepared.snapshot()
	for _, limit := range []int{1, 2, 13, 50} {
		sweep(limit)
	}
	after := prepared.snapshot()
	var grown []string
	for q := range after {
		if !base[q] {
			grown = append(grown, q)
		}
	}
	sort.Strings(grown)
	for _, q := range grown {
		t.Errorf("new statement at another page size:\n%s", q)
	}
	if len(after) > maxPreparedTexts {
		t.Errorf("read tools prepared %d statements, want at most %d", len(after), maxPreparedTexts)
	}
	t.Logf("read tools prepared %d distinct statements", len(after))
}
