package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

var (
	niccolo    = types.NewJID("393335555555", types.DefaultUserServer)
	niccoloLID = types.NewJID("900000000000077", types.HiddenUserServer)
	marcoA     = types.NewJID("393331111111", types.DefaultUserServer)
	group      = types.NewJID("120363000000000009", types.GroupServer)
)

// seedSearch adds to seed: a saved contact with an accented name and a LID,
// FTS edge cases, media with a caption and a transcript, my own messages and
// 25 long "pizza" messages for paging.
func seedSearch(t *testing.T) *store.DB {
	t.Helper()
	db := seed(t)
	ctx := context.Background()
	in := &wa.Ingestor{DB: db, Self: func() (types.JID, types.JID) { return me, meLID }}
	at := t0.Add(-48 * time.Hour)
	n := 0
	msg := func(chat, sender types.JID, push string, m *waE2E.Message) {
		t.Helper()
		n++
		err := in.Message(ctx, &events.Message{Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: chat.Server == types.GroupServer, IsFromMe: sender == me},
			ID:            fmt.Sprintf("S%d", n), Timestamp: at.Add(time.Duration(n) * time.Minute), PushName: push,
		}, Message: m})
		if err != nil {
			t.Fatal(err)
		}
	}
	text := func(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

	pid, err := people.Link(ctx, db, niccolo, niccoloLID, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := people.SetNames(ctx, db, pid, people.Names{FullName: "Niccolò D'Amico", PushName: "Nicco"}); err != nil {
		t.Fatal(err)
	}
	msg(niccolo, niccolo, "Nicco", text("Perché non vieni stasera?"))
	msg(niccolo, me, "", text("e-mail: test (urgent) NOT OR NEAR"))
	msg(niccolo, niccolo, "Nicco", text("ci vediamo domani in ufficio"))
	msg(niccolo, niccolo, "Nicco", text("domani vediamo, ci sentiamo"))
	msg(niccolo, niccolo, "Nicco", text("la fatturazione è pronta"))
	msg(niccolo, niccolo, "Nicco", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("foto della fattura"), Mimetype: proto.String("image/jpeg")}})
	msg(niccolo, niccolo, "Nicco", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(7), Mimetype: proto.String("audio/ogg")}})
	if _, err := db.Exec(`UPDATE media SET transcript = 'ricordati il bonifico per Niccolò' WHERE mimetype = 'audio/ogg'`); err != nil {
		t.Fatal(err)
	}
	msg(group, niccolo, "Nicco", text("anche io in gruppo, perché no"))
	msg(group, niccolo, "Nicco", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(90), Mimetype: proto.String("audio/ogg; codecs=opus")}})
	if _, err := db.Exec(`UPDATE media SET transcript = ? WHERE mimetype = 'audio/ogg; codecs=opus'`,
		strings.Repeat("parlo a lungo ", 40)+"della assicurazione "+strings.Repeat("e poi altro ", 40)); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("bla ", 150)
	for i := range 25 {
		msg(marcoA, marcoA, "Marco", text(fmt.Sprintf("%s pizza numero %d %s", long, i, long)))
	}
	return db
}

func TestSearchPeople(t *testing.T) {
	cs := connect(t, seedSearch(t))

	// Ambiguous name: both Marcos come back, ranked, with ids and chats.
	text, isErr := call(t, cs, toolSearchPeople, map[string]any{"query": "marco"})
	if isErr || !strings.HasPrefix(text, "[whatsapp: ") || !strings.Contains(text, "2 people match \"marco\"") {
		t.Fatalf("ambiguous name:\n%s", text)
	}
	if got := ids(blockIDs, text); len(got) != 2 || !strings.HasPrefix(got[0], "p_") || !strings.HasPrefix(got[1], "p_") {
		t.Fatalf("person ids = %v:\n%s", got, text)
	}
	for _, want := range []string{"~Marco (+39 333 111 1111)", "~Marco (+39 333 222 2222)", "not saved, name from their WhatsApp profile",
		"DM (", "Calcetto</untrusted_wa", "last interaction: 2026-"} {
		if !strings.Contains(text, want) {
			t.Fatalf("people result lacks %q:\n%s", want, text)
		}
	}
	// The Marco who wrote most recently ranks first.
	if strings.Index(text, "+39 333 111 1111") > strings.Index(text, "+39 333 222 2222") {
		t.Fatalf("recency order wrong:\n%s", text)
	}

	// Accents and case are ignored; the saved name wins and the source is stated.
	for _, q := range []string{"niccolo", "NICCOLÒ", "d'amico", "damico", "nicc", "amico nic"} {
		text, isErr = call(t, cs, toolSearchPeople, map[string]any{"query": q})
		if q == "damico" {
			if isErr || !strings.Contains(text, "No person matches") {
				t.Fatalf("%q should not match (apostrophe splits words):\n%s", q, text)
			}
			continue
		}
		if isErr || !strings.Contains(text, "Niccolò D'Amico (+39 333 555 5555) · saved contact, name from the address book") {
			t.Fatalf("query %q:\n%s", q, text)
		}
	}

	// Phone in any format, a p_N id and the LID find the same person.
	for _, q := range []string{"+39 333 555 5555", "393335555555", "3335555555", "333 555 5555", "0039 333 5555555", "335 5555",
		"5555555", "900000000000077@lid", "900000000000077", "393335555555@s.whatsapp.net"} {
		text, isErr = call(t, cs, toolSearchPeople, map[string]any{"query": q})
		if isErr || !strings.Contains(text, "1 person matches") || !strings.Contains(text, "Niccolò D'Amico") {
			t.Fatalf("query %q:\n%s", q, text)
		}
	}
	pid := strings.Fields(text[strings.Index(text, "\np_")+1:])[0]
	text, _ = call(t, cs, toolSearchPeople, map[string]any{"query": pid, "response_format": "detailed"})
	for _, want := range []string{"address book \"Niccolò D'Amico\"", "WhatsApp profile ~Nicco", "phone +39 333 555 5555",
		"LID 900000000000077@lid", "group (2 from them"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detailed lacks %q:\n%s", want, text)
		}
	}

	text, isErr = call(t, cs, toolSearchPeople, map[string]any{"query": "zzzz"})
	if isErr || !strings.Contains(text, "No person matches \"zzzz\"") {
		t.Fatalf("no match:\n%s", text)
	}
	if _, isErr = call(t, cs, toolSearchPeople, map[string]any{}); !isErr {
		t.Fatal("empty query must be an error")
	}

	// Paging through a ranked list.
	text, _ = call(t, cs, toolSearchPeople, map[string]any{"query": "marco", "limit": 1})
	cur := cursorIn(nextCursor, text)
	if cur == "" || !strings.Contains(text, "showing 1-1") {
		t.Fatalf("first page:\n%s", text)
	}
	text, _ = call(t, cs, toolSearchPeople, map[string]any{"cursor": cur, "limit": 1})
	if !strings.Contains(text, "showing 2-2") || !strings.Contains(text, "+39 333 222 2222") || cursorIn(nextCursor, text) != "" {
		t.Fatalf("second page:\n%s", text)
	}
}

// A cursor is opaque but comes back from the client: a forged or garbled one
// with a negative offset must be refused, not crash the daemon.
func TestNegativeCursorOffset(t *testing.T) {
	cs := connect(t, seedSearch(t))
	for _, c := range []cursor{
		{Tool: toolSearchPeople, Query: "marco", Off: -1},
		{Tool: toolSearchMessages, Query: "pizza", Off: -1},
	} {
		text, isErr := call(t, cs, c.Tool, map[string]any{"cursor": c.encode()})
		if !isErr || !strings.Contains(text, "invalid cursor") {
			t.Fatalf("%s with offset -1: isErr=%v\n%s", c.Tool, isErr, text)
		}
	}
}
