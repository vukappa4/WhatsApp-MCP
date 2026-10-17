package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

var (
	readChatHead  = regexp.MustCompile(`(?m)^Chat c_(\d+): .*\. (\d+) messages, `)
	newerCursorRe = regexp.MustCompile(`(?m)^Newer messages: cursor="([^"]+)"`)
)

// cursorCheck reads pages and checks each cursor against plain SQL: a page
// offers an older (newer) cursor exactly when the chat has a message within
// the page's bounds before its first (after its last) message.
type cursorCheck struct {
	t   *testing.T
	s   *Server
	ctx context.Context
	// pages counts checked pages by which cursors they carry: none, older
	// only, newer only, both. trimmed counts pages cut to the budget.
	pages   [4]int
	trimmed int
}

func (c *cursorCheck) read(in readChatIn) (older, newer string) {
	t := c.t
	t.Helper()
	res, err := c.s.readChat(c.ctx, nil, in)
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(res)
	if res.IsError {
		t.Fatalf("%+v: %s", in, text)
	}
	head := readChatHead.FindStringSubmatch(text)
	if head == nil {
		if !strings.Contains(text, "No messages in this range.") {
			t.Fatalf("%+v: no header:\n%.300s", in, text)
		}
		return "", ""
	}
	chat, _ := strconv.ParseInt(head[1], 10, 64)
	msgs := ids(lineIDs, text)
	if n, _ := strconv.Atoi(head[2]); n != len(msgs) || n == 0 {
		t.Fatalf("%+v: header says %s messages, lines show %d", in, head[2], len(msgs))
	}
	if strings.Contains(text, " left out to keep the answer under") {
		c.trimmed++
	}

	var after, before int64
	switch {
	case in.Cursor != "":
		cur, err := decodeCursor(in.Cursor, toolReadChat)
		if err != nil {
			t.Fatal(err)
		}
		after, before = cur.After, cur.Before
	case in.Around != "":
	default:
		if in.After != "" {
			tm, _ := parseTime(in.After)
			after = tm.Unix()
		}
		if in.Before != "" {
			tm, _ := parseTime(in.Before)
			before = tm.Unix()
		}
	}
	pos := func(m string) (ts, id int64) {
		id, _ = parseID(m, "m_")
		if err := c.s.DB.QueryRowContext(c.ctx, `SELECT ts FROM messages WHERE id = ?`, id).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts, id
	}
	exists := func(cmp string, ts, id int64) bool {
		var n int
		if err := c.s.DB.QueryRowContext(c.ctx, `SELECT COUNT(*) FROM messages WHERE chat_id = ?1
			AND (?2 = 0 OR ts >= ?2) AND (?3 = 0 OR ts < ?3)
			AND (ts `+cmp+` ?4 OR (ts = ?4 AND id `+cmp+` ?5))`, chat, after, before, ts, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	older, newer = cursorIn(oldCursor, text), cursorIn(newerCursorRe, text)
	check := func(cur string, newerSide bool, m string) {
		t.Helper()
		ts, id := pos(m)
		cmp := "<"
		if newerSide {
			cmp = ">"
		}
		if want := exists(cmp, ts, id); want != (cur != "") {
			t.Fatalf("%+v: messages beyond %s (newer %v): %v, cursor %q\n%.400s", in, m, newerSide, want, cur, text)
		}
		if cur == "" {
			return
		}
		got, err := decodeCursor(cur, toolReadChat)
		if err != nil {
			t.Fatal(err)
		}
		want := cursor{Tool: toolReadChat, Chat: chat, Newer: newerSide, TS: ts, ID: id, After: after, Before: before}
		if got != want {
			t.Fatalf("%+v: cursor %+v, want %+v", in, got, want)
		}
	}
	check(older, false, msgs[0])
	check(newer, true, msgs[len(msgs)-1])
	k := 0
	if older != "" {
		k |= 1
	}
	if newer != "" {
		k |= 2
	}
	c.pages[k]++
	return older, newer
}

// walk reads a page, then follows its older and newer cursors for up to n pages each.
func (c *cursorCheck) walk(in readChatIn, n int) {
	older, newer := c.read(in)
	for i := 0; i < n && older != ""; i++ {
		var nw string
		older, nw = c.read(readChatIn{Cursor: older, Limit: in.Limit, ResponseFormat: in.ResponseFormat})
		if i == 0 && nw != "" {
			c.read(readChatIn{Cursor: nw, Limit: in.Limit, ResponseFormat: in.ResponseFormat})
		}
	}
	for i := 0; i < n && newer != ""; i++ {
		_, newer = c.read(readChatIn{Cursor: newer, Limit: in.Limit, ResponseFormat: in.ResponseFormat})
	}
}

// covered fails unless pages with each of the cursor sets were checked.
func (c *cursorCheck) covered(sets ...int) {
	c.t.Helper()
	for _, k := range sets {
		if n := c.pages[k]; n == 0 {
			c.t.Fatalf("no page with cursor set %d (1 older, 2 newer): %v", k, c.pages)
		}
	}
}

// read_chat offers a cursor exactly where more messages lie within the
// request's bounds, on every kind of page: newest, after, before, a range,
// around a message, and pages reached through cursors in both directions.
func TestReadChatCursorsMatchMessages(t *testing.T) {
	s, _ := fixtureServer(t)
	ctx := context.Background()
	c := &cursorCheck{t: t, s: s, ctx: ctx}
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id, COUNT(m.id) FROM chats c JOIN messages m ON m.chat_id = c.id GROUP BY c.id ORDER BY c.id`)
	if err != nil {
		t.Fatal(err)
	}
	type chatN struct{ id, n int64 }
	var chats []chatN
	for rows.Next() {
		var x chatN
		if err := rows.Scan(&x.id, &x.n); err != nil {
			t.Fatal(err)
		}
		chats = append(chats, x)
	}
	rows.Close()
	at := func(chat, off int64) (ts, id int64) {
		if err := s.DB.QueryRowContext(ctx, `SELECT ts, id FROM messages WHERE chat_id = ? ORDER BY ts, id LIMIT 1 OFFSET ?`, chat, off).Scan(&ts, &id); err != nil {
			t.Fatal(err)
		}
		return ts, id
	}
	iso := func(ts int64) string { return time.Unix(ts, 0).Format(time.RFC3339) }
	for i, ch := range chats {
		chat := chatID(ch.id)
		c.walk(readChatIn{Chat: chat}, 1)
		if i%5 != 0 {
			continue
		}
		c.walk(readChatIn{Chat: chat, Limit: 4}, 3)
		lo, _ := at(ch.id, ch.n/4)
		hi, _ := at(ch.id, ch.n*3/4)
		c.walk(readChatIn{Chat: chat, After: iso(lo), Limit: 5}, 2)
		c.walk(readChatIn{Chat: chat, Before: iso(hi), Limit: 5}, 2)
		c.walk(readChatIn{Chat: chat, After: iso(lo), Before: iso(hi), Limit: 3}, 2)
		for _, off := range []int64{0, ch.n / 2, ch.n - 1} {
			_, id := at(ch.id, off)
			for _, limit := range []int{1, 2, 7} {
				c.walk(readChatIn{Around: messageID(id), Limit: limit}, 1)
			}
		}
	}
	c.covered(0, 1, 2, 3)
}

// Pages cut to the answer budget keep a cursor to the messages left out, on
// whichever side they were dropped.
func TestReadChatCursorsAfterBudget(t *testing.T) {
	db := seed(t)
	ctx := context.Background()
	in := &wa.Ingestor{DB: db, Self: func() (types.JID, types.JID) { return me, meLID }}
	jid := types.NewJID("393335555557", types.DefaultUserServer)
	long := strings.Repeat("parola ", 600) // 4,200 characters
	for i := 0; i < 80; i++ {
		// Pairs share a second, so the id tie-break decides the order.
		if err := in.Message(ctx, &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid, Sender: jid},
			ID: fmt.Sprintf("C%02d", i), Timestamp: t0.Add(time.Duration(i/2) * time.Minute), PushName: "Cleo"},
			Message: &waE2E.Message{Conversation: proto.String(fmt.Sprintf("n%02d %s", i, long))}}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{DB: db, WA: fixedStatus{State: wa.StateConnected, Since: t0}, Profile: "dev", DataDir: "x", Version: "test",
		Now: func() time.Time { return t0.Add(3 * time.Hour) }}
	c := &cursorCheck{t: t, s: s, ctx: ctx}
	var mid int64
	db.QueryRow(`SELECT id FROM messages WHERE wa_id = 'C40'`).Scan(&mid)
	for _, limit := range []int{30, 100} {
		for _, in := range []readChatIn{
			{Chat: "+39 333 555 5557"},
			{Chat: "+39 333 555 5557", After: t0.Add(5 * time.Minute).Format(time.RFC3339)},
			{Chat: "+39 333 555 5557", Before: t0.Add(35 * time.Minute).Format(time.RFC3339)},
			{Chat: "+39 333 555 5557", After: t0.Add(5 * time.Minute).Format(time.RFC3339), Before: t0.Add(35 * time.Minute).Format(time.RFC3339)},
			{Around: messageID(mid)},
		} {
			in.Limit, in.ResponseFormat = limit, "detailed"
			c.walk(in, 4)
		}
	}
	if c.trimmed == 0 {
		t.Fatal("no page was cut to the budget")
	}
	c.covered(1, 2, 3)
}
