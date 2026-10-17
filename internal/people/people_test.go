package people

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, db *store.DB, q string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func count(t *testing.T, db *store.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

var (
	lid = types.NewJID("256186360283325", types.HiddenUserServer)
	pn  = types.NewJID("393331234567", types.DefaultUserServer)
)

func TestMergeLinksPersonsAndMergesDMChats(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)

	// The LID person wrote two messages in the LID chat and reacted in a group.
	pl, err := Resolve(ctx, db, types.JID{User: lid.User, Server: types.HiddenUserServer, Device: 12})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetNames(ctx, db, pl, Names{PushName: "Marco"}); err != nil {
		t.Fatal(err)
	}
	cl, err := EnsureDMChat(ctx, db, pl, lid)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE chats SET last_message_at = 300, unread_count = 1 WHERE id = ?`, cl)
	mustExec(t, db, `INSERT INTO messages(chat_id, wa_id, sender_person_id, ts, kind, text) VALUES (?, 'A', ?, 100, 'text', 'ciao dal lid')`, cl, pl)
	dupL := mustExec(t, db, `INSERT INTO messages(chat_id, wa_id, sender_person_id, ts, kind, text) VALUES (?, 'B', ?, 300, 'text', 'doppione')`, cl, pl)
	mustExec(t, db, `INSERT INTO reactions(message_id, person_id, emoji, ts) VALUES (?, ?, '👍', 310)`, dupL, pl)

	group := mustExec(t, db, `INSERT INTO chats(jid, kind, name, created_at) VALUES ('1203@g.us', 'group', 'Team', 0)`)
	gm := mustExec(t, db, `INSERT INTO messages(chat_id, wa_id, ts, kind, text) VALUES (?, 'G', 50, 'text', 'group msg')`, group)
	mustExec(t, db, `INSERT INTO reactions(message_id, person_id, emoji, ts) VALUES (?, ?, '❤️', 60)`, gm, pl)

	// The phone person is the saved contact, with its own DM chat.
	pp, err := Resolve(ctx, db, pn)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetNames(ctx, db, pp, Names{FullName: "Marco Rossi", FirstName: "Marco"}); err != nil {
		t.Fatal(err)
	}
	cp, err := EnsureDMChat(ctx, db, pp, pn)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE chats SET last_message_at = 200, unread_count = 2 WHERE id = ?`, cp)
	mustExec(t, db, `INSERT INTO messages(chat_id, wa_id, sender_person_id, ts, kind, text) VALUES (?, 'B', ?, 300, 'text', 'doppione')`, cp, pp)
	mustExec(t, db, `INSERT INTO messages(chat_id, wa_id, sender_person_id, ts, kind, text) VALUES (?, 'C', ?, 200, 'text', 'ciao dal telefono')`, cp, pp)
	mustExec(t, db, `INSERT INTO reactions(message_id, person_id, emoji, ts) VALUES (?, ?, '😂', 70)`, gm, pp)

	if pl == pp {
		t.Fatal("LID and phone must start as separate persons")
	}

	merged, err := Link(ctx, db, lid, pn, t1)
	if err != nil {
		t.Fatal(err)
	}
	if merged != pl { // lower id survives
		t.Fatalf("survivor = %d, want %d", merged, pl)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM persons`); n != 1 {
		t.Fatalf("persons = %d, want 1", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM jids WHERE person_id = ?`, merged); n != 2 {
		t.Fatalf("jids on survivor = %d, want 2", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM chats WHERE kind = 'dm'`); n != 1 {
		t.Fatalf("dm chats = %d, want 1", n)
	}
	chat, _ := DMChatID(ctx, db, merged)
	if n := count(t, db, `SELECT COUNT(*) FROM messages WHERE chat_id = ?`, chat); n != 3 {
		t.Fatalf("messages in merged chat = %d, want 3 (A, B once, C)", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM messages WHERE sender_person_id = ?`, merged); n != 3 {
		t.Fatalf("messages repointed = %d, want 3", n)
	}
	if n := count(t, db, `SELECT last_message_at FROM chats WHERE id = ?`, chat); n != 300 {
		t.Fatalf("last_message_at = %d, want 300 (max, never backwards)", n)
	}
	if n := count(t, db, `SELECT unread_count FROM chats WHERE id = ?`, chat); n != 3 {
		t.Fatalf("unread = %d, want 3", n)
	}
	// The reaction on the duplicate survived on the kept copy.
	if n := count(t, db, `SELECT COUNT(*) FROM reactions r JOIN messages m ON m.id = r.message_id WHERE m.wa_id = 'B'`); n != 1 {
		t.Fatalf("reactions on B = %d, want 1", n)
	}
	// Both persons reacted to the group message: the newest reaction wins.
	var emoji string
	if err := db.QueryRow(`SELECT emoji FROM reactions WHERE message_id = ? AND person_id = ?`, gm, merged).Scan(&emoji); err != nil {
		t.Fatal(err)
	}
	if emoji != "😂" {
		t.Fatalf("group reaction = %q, want newest 😂", emoji)
	}
	var name, source, phone string
	var saved bool
	if err := db.QueryRow(`SELECT display_name, name_source, phone_e164, saved_contact FROM persons WHERE id = ?`, merged).
		Scan(&name, &source, &phone, &saved); err != nil {
		t.Fatal(err)
	}
	if name != "Marco Rossi" || source != SourceFullName || phone != "+393331234567" || !saved {
		t.Fatalf("person = %q %q %q saved=%v", name, source, phone, saved)
	}
	// FTS follows the merge: the duplicate's index row is gone, the rest stays.
	if n := count(t, db, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'doppione'`); n != 1 {
		t.Fatalf("fts doppione = %d, want 1", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'telefono'`); n != 1 {
		t.Fatalf("fts telefono = %d, want 1", n)
	}

	// Linking again is a no-op.
	again, err := Link(ctx, db, pn, lid, t1)
	if err != nil || again != merged {
		t.Fatalf("relink = %d, %v", again, err)
	}
}

func TestMergeKeepsMe(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	other, _ := Resolve(ctx, db, pn)
	me, _ := Resolve(ctx, db, lid)
	if err := MarkMe(ctx, db, me); err != nil {
		t.Fatal(err)
	}
	got, err := Merge(ctx, db, other, me)
	if err != nil {
		t.Fatal(err)
	}
	if got != me {
		t.Fatalf("survivor = %d, want me %d", got, me)
	}
	if n := count(t, db, `SELECT is_me FROM persons WHERE id = ?`, me); n != 1 {
		t.Fatal("is_me lost")
	}
}

func TestDisplayNamePriority(t *testing.T) {
	cases := []struct {
		full, first, biz, push, phone string
		want, src                     string
	}{
		{"Marco Rossi", "Marco", "Rossi Srl", "Marchino", "+393331234567", "Marco Rossi", SourceFullName},
		{"", "Marco", "Rossi Srl", "Marchino", "+393331234567", "Marco", SourceFirstName},
		{"", "", "Rossi Srl", "Marchino", "+393331234567", "Rossi Srl", SourceBusinessName},
		{"", "", "", "Marchino", "+393331234567", "Marchino", SourcePushName},
		{"", "", "", "", "+393331234567", "+39 333 123 4567", SourcePhone},
		{"", "", "", "", "", "", ""},
	}
	for _, c := range cases {
		got, src := DisplayName(c.full, c.first, c.biz, c.push, c.phone)
		if got != c.want || src != c.src {
			t.Errorf("DisplayName(%q,%q,%q,%q,%q) = %q,%q want %q,%q", c.full, c.first, c.biz, c.push, c.phone, got, src, c.want, c.src)
		}
	}
}

func TestNormalizeStripsDevice(t *testing.T) {
	j, kind, ok := Normalize(types.NewADJID("393331234567", 0, 5))
	if !ok || kind != KindPN || j.String() != "393331234567@s.whatsapp.net" {
		t.Fatalf("got %s %s %v", j, kind, ok)
	}
	if _, _, ok := Normalize(types.NewJID("1203", types.GroupServer)); ok {
		t.Fatal("group JID must not normalize to a person")
	}
}

// Finding 5: a message stored in both chats keeps the richer fields of each copy.
func TestMergeChatsMergesDuplicateFieldwise(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	pl, _ := Resolve(ctx, db, lid)
	pp, _ := Resolve(ctx, db, pn)
	cl, _ := EnsureDMChat(ctx, db, pl, lid)
	cp, _ := EnsureDMChat(ctx, db, pp, pn)
	// Lower person id survives, so the LID chat keeps its row: a plain legacy copy.
	keep := mustExec(t, db, `INSERT INTO messages(chat_id, wa_id, sender_person_id, ts, kind, text, source) VALUES (?, 'B', ?, 300, 'text', 'prima', 'legacy')`, cl, pl)
	mustExec(t, db, `INSERT INTO reactions(message_id, person_id, emoji, ts) VALUES (?, ?, '👍', 10)`, keep, pl)
	media := mustExec(t, db, `INSERT INTO media(mimetype, direct_path) VALUES ('image/jpeg', '/v/x')`)
	drop := mustExec(t, db, `INSERT INTO messages(chat_id, wa_id, sender_person_id, ts, kind, text, source, edited_at, deleted_at, reply_to_wa_id, reply_preview, media_id)
		VALUES (?, 'B', ?, 300, 'image', 'dopo modifica', 'live', 350, 400, 'A', 'citazione', ?)`, cp, pp, media)
	mustExec(t, db, `INSERT INTO reactions(message_id, person_id, emoji, ts) VALUES (?, ?, '❤️', 20)`, drop, pp)

	if _, err := Link(ctx, db, lid, pn, t1); err != nil {
		t.Fatal(err)
	}
	var source, kind, text, reply, preview, mime string
	var edited, deleted int64
	err := db.QueryRow(`SELECT m.source, m.kind, m.text, m.reply_to_wa_id, m.reply_preview, m.edited_at, m.deleted_at, md.mimetype
		FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = 'B'`).
		Scan(&source, &kind, &text, &reply, &preview, &edited, &deleted, &mime)
	if err != nil {
		t.Fatal(err)
	}
	if source != "live" || kind != "image" || text != "dopo modifica" || reply != "A" || preview != "citazione" || edited != 350 || deleted != 400 || mime != "image/jpeg" {
		t.Fatalf("merged = %s %s %q %s %q %d %d %s", source, kind, text, reply, preview, edited, deleted, mime)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM messages WHERE wa_id = 'B'`); n != 1 {
		t.Fatalf("copies = %d", n)
	}
	// Both persons are one now: the newest reaction survives.
	var emoji string
	db.QueryRow(`SELECT emoji FROM reactions`).Scan(&emoji)
	if n := count(t, db, `SELECT COUNT(*) FROM reactions`); n != 1 || emoji != "❤️" {
		t.Fatalf("reactions = %d %q", n, emoji)
	}
}

// Addition B: profile names render like WhatsApp shows strangers.
func TestProfileName(t *testing.T) {
	cases := map[string]string{
		"Marco":                    "~Marco",
		"  Ma‮rco\n<b>Rossi</b>  ": "~Marco bRossi/b",
		"":                         "",
		strings.Repeat("a", 50):    "~" + strings.Repeat("a", 39) + "…",
	}
	for in, want := range cases {
		if got := ProfileName(in); got != want {
			t.Errorf("ProfileName(%q) = %q, want %q", in, got, want)
		}
	}
}
