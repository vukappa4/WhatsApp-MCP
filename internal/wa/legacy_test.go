package wa

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

func legacyFixture(t *testing.T) (messages, session string) {
	t.Helper()
	dir := t.TempDir()
	messages, session = filepath.Join(dir, "messages.db"), filepath.Join(dir, "whatsapp.db")
	exec := func(path string, stmts ...string) {
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	exec(session,
		`CREATE TABLE whatsmeow_device (jid TEXT PRIMARY KEY, lid TEXT, push_name TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO whatsmeow_device VALUES ('393700000001:36@s.whatsapp.net', '900000000000001:36@lid', 'Mario')`,
		`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`,
		`INSERT INTO whatsmeow_lid_map VALUES ('900000000000001', '393700000001'), ('111111111111111', '393331234567')`,
		`CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT, redacted_phone TEXT)`,
		`INSERT INTO whatsmeow_contacts VALUES ('393700000001:36@s.whatsapp.net', '393331234567@s.whatsapp.net', 'Marco', 'Marco Rossi', 'Marchino', '', '')`,
	)
	exec(messages,
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)`,
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB, file_length INTEGER,
			PRIMARY KEY (id, chat_jid))`,
		`INSERT INTO chats VALUES
			('393331234567@s.whatsapp.net', 'Marco Rossi', '2026-10-12 23:09:08+02:00'),
			('111111111111111@lid', '111111111111111', '2026-10-13 09:00:00+02:00'),
			('120363000000000001@g.us', 'Group 120363000000000001', '2026-10-13 10:00:00+02:00'),
			('status@broadcast', '', '2026-10-13 10:00:00+02:00')`,
		`INSERT INTO messages VALUES
			('P1', '393331234567@s.whatsapp.net', '393331234567', 'ciao dal numero', '2026-10-12 23:09:08+02:00', 0, '', '', '', NULL, NULL, NULL, 0),
			('L1', '111111111111111@lid', '111111111111111', 'ciao dal lid', '2026-10-13 09:00:00+02:00', 0, '', '', '', NULL, NULL, NULL, 0),
			('L2', '111111111111111@lid', '900000000000001', '', '2026-10-13 09:05:00+02:00', 1, 'image', 'img.jpg',
				'https://mmg.whatsapp.net/v/t62/abc.enc?ccb=1&oh=x', x'01', x'02', x'03', 2048),
			('G1', '120363000000000001@g.us', '111111111111111', 'nel gruppo', '2026-10-13 10:00:00+01:00', 0, '', '', '', NULL, NULL, NULL, 0),
			('S1', 'status@broadcast', '393331234567', 'stato', '2026-10-13 10:00:00+02:00', 0, '', '', '', NULL, NULL, NULL, 0)`,
	)
	return messages, session
}

func TestImportLegacy(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	msgs, sess := legacyFixture(t)

	st, err := ImportLegacy(ctx, db, msgs, sess)
	if err != nil {
		t.Fatal(err)
	}
	if st.MessagesImported != 4 || st.MessagesSkipped != 1 || st.MergedDMChats != 1 || st.Chats != 2 {
		t.Fatalf("stats = %+v", st)
	}
	in := &Ingestor{DB: db}
	if got := one[string](t, in, `SELECT p.display_name || ' ' || p.phone_e164 FROM messages m JOIN persons p ON p.id = m.sender_person_id WHERE m.wa_id = 'G1'`); got != "Marco Rossi +393331234567" {
		t.Fatalf("group sender = %q", got)
	}
	if n := one[int](t, in, `SELECT COUNT(DISTINCT chat_id) FROM messages WHERE wa_id IN ('P1', 'L1', 'L2')`); n != 1 {
		t.Fatalf("DM messages in %d chats, want 1", n)
	}
	if got := one[bool](t, in, `SELECT p.is_me FROM messages m JOIN persons p ON p.id = m.sender_person_id WHERE m.wa_id = 'L2'`); !got {
		t.Fatal("from-me message not attributed to me")
	}
	if got := one[string](t, in, `SELECT md.direct_path || ' ' || md.mimetype FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = 'L2'`); got != "/v/t62/abc.enc?ccb=1&oh=x image/jpeg" {
		t.Fatalf("media = %q", got)
	}
	if got := one[int64](t, in, `SELECT ts FROM messages WHERE wa_id = 'G1'`); got != 1791882000 { // 10:00+01:00 = 09:00Z
		t.Fatalf("ts = %d", got)
	}
	if got := one[string](t, in, `SELECT name FROM chats WHERE kind = 'group'`); got != "" {
		t.Fatalf("fallback group name kept: %q", got)
	}

	again, err := ImportLegacy(ctx, db, msgs, sess)
	if err != nil {
		t.Fatal(err)
	}
	if again.MessagesImported != 0 || again.ArchivedNew != 0 || again.ChatsImported != 0 || again.Messages != 4 {
		t.Fatalf("second import = %+v", again)
	}
}
