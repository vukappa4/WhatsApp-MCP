package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dir with space", "data.db")
	if err := mkdirFor(path); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestMigrationsAreVersionedAndIdempotent(t *testing.T) {
	db, path := open(t)
	db.Close()
	db2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var v, n int
	if err := db2.QueryRow(`SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&v, &n); err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion() || n != SchemaVersion() {
		t.Fatalf("schema version %d (%d rows), want %d", v, n, SchemaVersion())
	}
	var fk, wal string
	db2.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	db2.QueryRow(`PRAGMA journal_mode`).Scan(&wal)
	if fk != "1" || wal != "wal" {
		t.Fatalf("pragmas: foreign_keys=%s journal_mode=%s", fk, wal)
	}
}

func TestFTS5TriggersFollowTextAndTranscript(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	chat, err := EnsureChat(ctx, db, "1@g.us", "group")
	if err != nil {
		t.Fatal(err)
	}
	res, _ := db.Exec(`INSERT INTO media(mimetype) VALUES ('audio/ogg')`)
	media, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO messages(chat_id, wa_id, ts, kind, text, media_id) VALUES (?, 'X', 1, 'voice', '', ?)`, chat, media); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(chat_id, wa_id, ts, kind, text) VALUES (?, 'Y', 2, 'text', 'Perché è così caro?')`, chat); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("perche") != 1 { // diacritics folded
		t.Fatal("diacritic-insensitive match failed")
	}
	db.Exec(`UPDATE media SET transcript = 'ci vediamo domani in ufficio' WHERE id = ?`, media)
	if count("ufficio") != 1 {
		t.Fatal("transcript not indexed")
	}
	db.Exec(`UPDATE messages SET text = 'prezzo modificato' WHERE wa_id = 'Y'`)
	if count("caro") != 0 || count("modificato") != 1 {
		t.Fatal("edit not reflected in FTS")
	}
	db.Exec(`DELETE FROM messages WHERE wa_id = 'X'`)
	if count("ufficio") != 0 {
		t.Fatal("delete not reflected in FTS")
	}
}

func TestBumpChatNeverMovesBackwards(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	id, _ := EnsureChat(ctx, db, "2@g.us", "group")
	for _, ts := range []int64{100, 300, 200} {
		if err := BumpChat(ctx, db, id, ts); err != nil {
			t.Fatal(err)
		}
	}
	var got int64
	db.QueryRow(`SELECT last_message_at FROM chats WHERE id = ?`, id).Scan(&got)
	if got != 300 {
		t.Fatalf("last_message_at = %d, want 300", got)
	}
}

func TestGapsAndUncleanExit(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	base := time.Unix(1_000_000, 0)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	RecordDaemonStart(ctx, db, at(0), "")
	LogConnection(ctx, db, at(1), EvConnected, "")
	Heartbeat(ctx, db, at(10))
	// Crash: no daemon_stop. The next start closes the run at the last heartbeat.
	if err := RecordDaemonStart(ctx, db, at(60), ""); err != nil {
		t.Fatal(err)
	}
	LogConnection(ctx, db, at(61), EvConnected, "")
	LogConnection(ctx, db, at(70), EvLoggedOut, "")
	gaps, err := Gaps(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 2 {
		t.Fatalf("gaps = %+v", gaps)
	}
	if !gaps[0].From.Equal(at(10)) || !gaps[0].To.Equal(at(61)) || gaps[0].Reason != EvDaemonStop {
		t.Fatalf("first gap = %+v", gaps[0])
	}
	if !gaps[1].From.Equal(at(70)) || !gaps[1].To.IsZero() {
		t.Fatalf("open gap = %+v", gaps[1])
	}
}

func mkdirFor(path string) error { return os.MkdirAll(filepath.Dir(path), 0o700) }

func TestMergeMessageKeepsStrongestFacts(t *testing.T) {
	legacy := MessageFields{Source: SourceLegacy, Kind: "text", Text: "vecchio", TS: 10, SenderPerson: 1}
	hist := MessageFields{Source: SourceHistory, Kind: "image", Text: "didascalia", ReplyTo: "Q", TS: 10, SenderPerson: 2}
	got := MergeMessage(legacy, hist)
	if got.Source != SourceHistory || got.Kind != "image" || got.Text != "didascalia" || got.ReplyTo != "Q" || got.SenderPerson != 2 {
		t.Fatalf("upgrade: %+v", got)
	}
	// A poorer copy only fills gaps.
	back := MergeMessage(got, MessageFields{Source: SourceLegacy, Kind: "text", Text: "vecchio", ReplyPreview: "anteprima"})
	if back.Kind != "image" || back.Text != "didascalia" || back.ReplyPreview != "anteprima" || back.Source != SourceHistory {
		t.Fatalf("downgrade: %+v", back)
	}
	// An edited text survives an unedited richer copy; deletion is never undone.
	edited := MessageFields{Source: SourceHistory, Text: "modificato", EditedAt: 50, DeletedAt: 60}
	live := MergeMessage(edited, MessageFields{Source: SourceLive, Text: "originale"})
	if live.Text != "modificato" || live.EditedAt != 50 || live.DeletedAt != 60 || live.Source != SourceLive {
		t.Fatalf("edit/delete: %+v", live)
	}
}

func TestKeepaliveRestoredClosesGap(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	base := time.Unix(2_000_000, 0)
	LogConnection(ctx, db, base, EvConnected, "")
	LogConnection(ctx, db, base.Add(time.Minute), EvKeepAliveLost, "")
	LogConnection(ctx, db, base.Add(2*time.Minute), EvKeepAliveBack, "")
	gaps, _ := Gaps(ctx, db)
	if len(gaps) != 1 || !gaps[0].To.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("gaps = %+v", gaps)
	}
}

func TestArchiveRawIsIdempotentAndCompressed(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	payload := []byte(strings.Repeat("ciao ", 1000))
	a, err := db.ArchiveRaw(ctx, RawMessage, "k1", payload, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.ArchiveRaw(ctx, RawMessage, "k1", payload, time.Unix(2, 0))
	if err != nil || b.ID != a.ID {
		t.Fatalf("second archive = %+v, %v", b, err)
	}
	var stored int
	db.QueryRow(`SELECT length(payload) FROM raw_events`).Scan(&stored)
	if stored >= len(payload)/10 {
		t.Fatalf("payload not compressed: %d bytes", stored)
	}
	evs, err := ListRaw(ctx, db, RawFilter{PendingOnly: true})
	if err != nil || len(evs) != 1 || string(evs[0].Payload) != string(payload) {
		t.Fatalf("list = %v, %v", len(evs), err)
	}
	if err := MarkRaw(ctx, db, a.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := CountPendingRaw(ctx, db); n != 0 {
		t.Fatal("still pending after MarkRaw")
	}
}

// A panic inside a transaction rolls it back and releases the write lock.
func TestTxRollsBackOnPanic(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	func() {
		defer func() { _ = recover() }()
		_ = db.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sync_state(key, value, updated_at) VALUES ('k', 'v', 0)`); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	done := make(chan error, 1)
	go func() {
		done <- db.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO sync_state(key, value, updated_at) VALUES ('k2', 'v', 0)`)
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write lock still held after the panic")
	}
	if _, ok, _ := GetState(ctx, db, "k"); ok {
		t.Fatal("the panicking transaction committed")
	}
}

func TestListChatsQueryWithKind(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	for i, name := range []string{"Family", "Work"} {
		id, err := EnsureChat(ctx, db, string(rune('1'+i))+"@g.us", "group")
		if err != nil {
			t.Fatal(err)
		}
		if err := SetChatName(ctx, db, id, name); err != nil {
			t.Fatal(err)
		}
		if err := BumpChat(ctx, db, id, int64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []ChatFilter{
		{Kind: "group", Query: "Family", Limit: 10},
		{Kind: "group", Query: "Family", HasCursor: true, AfterTS: 1000, AfterID: 1000, Limit: 10},
	} {
		chats, _, err := ListChats(ctx, db, f)
		if err != nil {
			t.Fatal(err)
		}
		if len(chats) != 1 || chats[0].Name != "Family" {
			t.Errorf("%+v: got %d chats %+v, want only Family", f, len(chats), chats)
		}
	}
}
