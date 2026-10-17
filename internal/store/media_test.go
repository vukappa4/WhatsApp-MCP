package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A v2 database with media rows upgrades to v4: states map to the new set,
// rows and links survive, foreign keys stay on, and the FTS triggers that
// read media still fire.
func TestMigrateV4RebuildsMedia(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	raw, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for i, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		schemaV1, schemaV2,
		`INSERT INTO schema_migrations VALUES (1, 0), (2, 0)`,
		`INSERT INTO chats(id, jid, kind, created_at) VALUES (1, '1@g.us', 'group', 0)`,
		`INSERT INTO media(id, mimetype, state) VALUES (1, 'audio/ogg', 'remote'), (2, 'image/jpeg', 'gone'), (3, 'image/png', 'downloading')`,
		`INSERT INTO media(id, mimetype, state, local_path, transcript) VALUES (4, 'audio/ogg', 'downloaded', 'x\y.ogg', 'ciao a tutti')`,
		`INSERT INTO messages(chat_id, wa_id, ts, kind, media_id) VALUES (1, 'A', 1, 'voice', 1), (1, 'B', 2, 'image', 2),
			(1, 'C', 3, 'image', 3), (1, 'D', 4, 'voice', 4)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("setup step %d: %v", i, err)
		}
	}
	raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := map[int64]string{}
	rows, err := db.Query(`SELECT id, state FROM media`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var s string
		rows.Scan(&id, &s)
		got[id] = s
	}
	rows.Close()
	want := map[int64]string{1: MediaPending, 2: MediaUnavailable, 3: MediaPending, 4: MediaDownloaded}
	for id, s := range want {
		if got[id] != s {
			t.Fatalf("media %d state %q, want %q (all: %v)", id, got[id], s, got)
		}
	}
	if _, err := db.Exec(`UPDATE media SET state = 'remote' WHERE id = 1`); err == nil {
		t.Fatal("old state accepted by the new CHECK")
	}

	// Foreign keys are on for every pooled connection and still enforced.
	conns := make([]*sql.Conn, 3)
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
		var fk int
		c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk)
		if fk != 1 {
			t.Fatalf("connection %d has foreign_keys=%d", i, fk)
		}
	}
	for _, c := range conns {
		c.Close()
	}
	if _, err := db.Exec(`DELETE FROM media WHERE id = 1`); err == nil {
		t.Fatal("deleting a referenced media row must fail")
	}

	// The transcript stored before the upgrade is still searchable; new
	// transcripts and new messages are indexed by the recreated triggers.
	match := func(q string) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if match("tutti") != 1 {
		t.Fatal("old transcript lost from FTS")
	}
	if err := SetTranscript(ctx, db, 1, "ci vediamo in ufficio", "it", "test"); err != nil {
		t.Fatal(err)
	}
	if match("ufficio") != 1 {
		t.Fatal("transcript update not indexed")
	}
	if _, err := db.Exec(`INSERT INTO media(id, mimetype, transcript) VALUES (5, 'audio/ogg', 'parola nuova')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(chat_id, wa_id, ts, kind, media_id) VALUES (1, 'E', 5, 'voice', 5)`); err != nil {
		t.Fatal(err)
	}
	if match("nuova") != 1 {
		t.Fatal("insert trigger does not read media")
	}

	j, ok, err := LoadMediaJob(ctx, db, 4)
	if err != nil || !ok || j.LocalPath != `x\y.ogg` || !j.HasTranscript || j.ChatJID != "1@g.us" || j.Kind != "voice" {
		t.Fatalf("LoadMediaJob = %+v, %v, %v", j, ok, err)
	}
	counts, err := MediaStateCounts(ctx, db)
	if err != nil || counts[MediaPending] != 3 || counts[MediaDownloaded] != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
}

// A database at v3 (chat flags, group info state, album links) upgrades to
// v4: the media rebuild keeps every v3 column, index and trigger outside
// media, album links and chat flags survive, and the FTS triggers still fire.
func TestMigrateV3ToV4KeepsDataQualitySchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	all := migrations
	migrations = all[:3]
	db, err := Open(ctx, path)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	for i, q := range []string{
		`INSERT INTO persons(id, display_name, name_source, phone_e164, updated_at) VALUES (1, 'Marco', 'full_name', '+39333', 0)`,
		`INSERT INTO chats(id, jid, kind, person_id, created_at, pinned, pinned_set_at, archived_set_at, info_attempts, info_error)
			VALUES (1, '39333@s.whatsapp.net', 'dm', 1, 0, 1, 50, 40, 0, '')`,
		`INSERT INTO chats(id, jid, kind, created_at, info_attempts, info_checked_at, info_error) VALUES (2, '1@g.us', 'group', 0, 2, 70, 'not in group')`,
		`INSERT INTO media(id, mimetype, state) VALUES (1, 'image/jpeg', 'remote'), (2, 'image/jpeg', 'gone')`,
		`INSERT INTO media(id, mimetype, state, local_path, transcript) VALUES (3, 'audio/ogg', 'downloaded', 'a.ogg', 'riunione domattina')`,
		`INSERT INTO messages(id, chat_id, wa_id, ts, kind, source, text) VALUES (10, 2, 'ALBUM', 1, 'text', 'live', '')`,
		`INSERT INTO messages(id, chat_id, wa_id, ts, kind, source, media_id, album_wa_id) VALUES
			(11, 2, 'P1', 2, 'image', 'history', 1, 'ALBUM'), (12, 2, 'P2', 3, 'image', 'history', 2, 'ALBUM')`,
		`INSERT INTO messages(id, chat_id, sender_person_id, wa_id, ts, kind, source, media_id) VALUES (13, 1, 1, 'V1', 4, 'voice', 'live', 3)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("setup step %d: %v", i, err)
		}
	}
	// Schema objects outside media, as v3 left them.
	objects := func() map[string]string {
		t.Helper()
		rows, err := db.Query(`SELECT type || ' ' || name, COALESCE(sql, '') FROM sqlite_master
			WHERE tbl_name NOT IN ('media', 'messages_fts') AND name NOT LIKE 'messages_fts%' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]string{}
		for rows.Next() {
			var k, v string
			rows.Scan(&k, &v)
			m[k] = v
		}
		return m
	}
	before := objects()
	db.Close()

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil || v != SchemaVersion() {
		t.Fatalf("version %d, %v", v, err)
	}
	after := objects()
	// Objects later migrations change on purpose.
	later := map[string]bool{"table raw_events": true}
	for k, sqlText := range before {
		// Triggers that read media are recreated with the same text.
		if _, ok := after[k]; !ok {
			t.Errorf("%s dropped", k)
		} else if after[k] != sqlText && !later[k] {
			t.Errorf("%s changed by v4: before %q, after %q", k, sqlText, after[k])
		}
	}
	var idx int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'media_state'`).Scan(&idx)
	if idx != 1 {
		t.Error("media_state index missing")
	}

	var albums int
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE album_wa_id = 'ALBUM' AND media_id IS NOT NULL`).Scan(&albums)
	if albums != 2 {
		t.Fatalf("album items with media = %d, want 2", albums)
	}
	var pinned, pinnedAt, attempts int64
	var infoErr string
	db.QueryRow(`SELECT pinned, pinned_set_at FROM chats WHERE id = 1`).Scan(&pinned, &pinnedAt)
	db.QueryRow(`SELECT info_attempts, info_error FROM chats WHERE id = 2`).Scan(&attempts, &infoErr)
	if pinned != 1 || pinnedAt != 50 || attempts != 2 || infoErr != "not in group" {
		t.Fatalf("chat flags lost: pinned=%d at %d, info %d %q", pinned, pinnedAt, attempts, infoErr)
	}
	counts, err := MediaStateCounts(ctx, db)
	if err != nil || counts[MediaPending] != 1 || counts[MediaUnavailable] != 1 || counts[MediaDownloaded] != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign key violations after v4")
	}
	rows.Close()

	match := func(q string) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if match("domattina") != 1 {
		t.Fatal("v3 transcript lost from FTS")
	}
	if err := SetTranscript(ctx, db, 1, "foto del cantiere", "it", "test"); err != nil {
		t.Fatal(err)
	}
	if match("cantiere") != 1 {
		t.Fatal("transcript update not indexed after v4")
	}
	j, ok, err := LoadMediaJob(ctx, db, 11)
	if err != nil || !ok || j.ChatJID != "1@g.us" || j.Kind != "image" {
		t.Fatalf("LoadMediaJob(album item) = %+v, %v, %v", j, ok, err)
	}
}

// v5 makes a re-upload that failed the key check retryable; the phone saying
// the file is gone stays final.
func TestMigrateV5RetriesForeignKeyReuploads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	all := migrations
	migrations = all[:4]
	db, err := Open(ctx, path)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO media(id, mimetype, state, attempts, error) VALUES
		(1, 'image/jpeg', 'unavailable', 1, 'the phone re-uploaded the file with a different key; forward it from the phone to read it'),
		(2, 'image/jpeg', 'unavailable', 1, 'the phone no longer has this file')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state1, err1, state2 string
	var attempts1 int
	db.QueryRow(`SELECT state, error, attempts FROM media WHERE id = 1`).Scan(&state1, &err1, &attempts1)
	db.QueryRow(`SELECT state FROM media WHERE id = 2`).Scan(&state2)
	if state1 != MediaFailed || attempts1 != 0 || !strings.Contains(err1, "key it does not share") || state2 != MediaUnavailable {
		t.Fatalf("after v5: %s %d %q, %s", state1, attempts1, err1, state2)
	}
}

// v6: history notifications that failed for good within WhatsApp's two-week
// retention are pending again; older failures and done ones stay.
func TestMigrateV6RetriesRecentFailedHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	all := migrations
	migrations = all[:5]
	db, err := Open(ctx, path)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, q := range []string{
		`INSERT INTO raw_events(kind, dedupe_key, received_at, payload, ingested_at, attempts, error) VALUES
			('history_notification', 'hsn:recent', ` + fmt.Sprint(now-3600) + `, x'00', 1, 5, 'download: websocket not connected'),
			('history_notification', 'hsn:old', ` + fmt.Sprint(now-20*86400) + `, x'00', 1, 5, 'download: 404'),
			('history_notification', 'hsn:done', ` + fmt.Sprint(now-3600) + `, x'00', 1, 1, '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if db, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for key, want := range map[string]bool{"hsn:recent": true, "hsn:old": false, "hsn:done": false} {
		var pending bool
		if err := db.QueryRow(`SELECT ingested_at IS NULL FROM raw_events WHERE dedupe_key = ?`, key).Scan(&pending); err != nil || pending != want {
			t.Errorf("%s pending = %v (%v), want %v", key, pending, err, want)
		}
	}
}

// An orphaned media row goes back to its own message: another chat's message
// with the same WhatsApp id takes it only when it holds the same file.
func TestAttachMediaMatchesChatOrFile(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO chats(id, jid, kind, created_at) VALUES (1, 'a@g.us', 'group', 0), (2, 'b@g.us', 'group', 0)`,
		`INSERT INTO media(id, mimetype, state, local_path, file_sha256, msg_chat_jid, msg_wa_id) VALUES (7, 'image/jpeg', 'downloaded', 'a.jpg', x'01', 'a@g.us', 'SAME')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	other, err := AttachMedia(ctx, db, MediaFields{Mimetype: "image/jpeg", SHA256: []byte{2}}, 2, "SAME")
	if err != nil || other == 7 {
		t.Fatalf("other chat, other file took the row: %d %v", other, err)
	}
	if _, err := db.Exec(`INSERT INTO messages(chat_id, wa_id, ts, kind, media_id) VALUES (2, 'SAME', 0, 'image', ?)`, other); err != nil {
		t.Fatal(err)
	}
	own, err := AttachMedia(ctx, db, MediaFields{Mimetype: "image/jpeg"}, 1, "SAME")
	if err != nil || own != 7 {
		t.Fatalf("own chat: %d %v", own, err)
	}
}

// Merging two copies of a message keeps the media row that holds the
// downloaded file and transcript, even when it belongs to the dropped copy.
func TestMergeMessageRowsKeepsDownloadedMedia(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO chats(id, jid, kind, created_at) VALUES (1, 'a@g.us', 'group', 0), (2, 'b@g.us', 'group', 0)`,
		`INSERT INTO media(id, mimetype, direct_path, state) VALUES (1, '', '/v/keep', 'pending')`,
		`INSERT INTO media(id, mimetype, state, local_path, transcript) VALUES (2, 'audio/ogg', 'downloaded', 'b.ogg', 'ciao')`,
		`INSERT INTO messages(id, chat_id, wa_id, ts, kind, source, media_id) VALUES (10, 1, 'X', 5, 'voice', 'history', 1), (20, 2, 'X', 5, 'voice', 'live', 2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := MergeMessageRows(ctx, db, 10, 20); err != nil {
		t.Fatal(err)
	}
	j, ok, err := LoadMediaJob(ctx, db, 10)
	if err != nil || !ok {
		t.Fatalf("merged message has no media: %v %v", ok, err)
	}
	if j.State != MediaDownloaded || j.LocalPath != "b.ogg" || j.Transcript != "ciao" || j.Mimetype != "audio/ogg" || j.DirectPath != "/v/keep" {
		t.Fatalf("merged media = %s %q %q %q %q", j.State, j.LocalPath, j.Transcript, j.Mimetype, j.DirectPath)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM media`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("media rows = %d %v", n, err)
	}
}

// A reset row keeps its transcript while pending. When the downloaded row
// wins the merge without one, the transcript moves over and stays searchable.
func TestMergeMessageRowsKeepsResetTranscript(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO chats(id, jid, kind, created_at) VALUES (1, 'a@g.us', 'group', 0), (2, 'b@g.us', 'group', 0)`,
		`INSERT INTO media(id, mimetype, state, local_path, transcript, transcript_lang, transcript_engine, transcript_error)
			VALUES (1, 'audio/ogg', 'pending', '', 'appuntamento in stazione', 'it', 'whisper', ''),
			(2, 'audio/ogg', 'downloaded', 'b.ogg', NULL, NULL, NULL, 'timeout')`,
		`INSERT INTO messages(id, chat_id, wa_id, ts, kind, source, media_id) VALUES (10, 1, 'X', 5, 'voice', 'history', 1), (20, 2, 'X', 5, 'voice', 'live', 2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := MergeMessageRows(ctx, db, 10, 20); err != nil {
		t.Fatal(err)
	}
	j, ok, err := LoadMediaJob(ctx, db, 10)
	if err != nil || !ok {
		t.Fatalf("merged message has no media: %v %v", ok, err)
	}
	if j.State != MediaDownloaded || j.LocalPath != "b.ogg" || j.Transcript != "appuntamento in stazione" ||
		j.TranscriptLang != "it" || j.TranscriptEngine != "whisper" || j.TranscriptError != "" {
		t.Fatalf("merged media = %s %q %q %q %q %q", j.State, j.LocalPath, j.Transcript, j.TranscriptLang, j.TranscriptEngine, j.TranscriptError)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM media`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("media rows = %d %v", n, err)
	}
	var id int64
	if err := db.QueryRow(`SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'stazione'`).Scan(&id); err != nil || id != 10 {
		t.Fatalf("transcript search = %d %v", id, err)
	}
}

// v10 makes downloaded rows that hold an absolute path (a sent file's
// original location) pending again, and leaves relative paths alone.
func TestMigrateV10ResetsAbsoluteMediaPaths(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	all := migrations
	migrations = all[:9]
	db, err := Open(ctx, path)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO media(id, mimetype, state, local_path, transcript) VALUES
		(1, 'application/pdf', 'downloaded', 'C:\Users\me\Documents\offerta.pdf', NULL),
		(2, 'audio/ogg', 'downloaded', '/Users/me/Desktop/nota.ogg', 'ciao'),
		(3, 'image/jpeg', 'downloaded', '393331111111@s.whatsapp.net\3EB0A_image.jpg', NULL),
		(4, 'image/jpeg', 'downloaded', '\\server\share\x.jpg', NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, state, local_path, COALESCE(transcript, '') FROM media ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int
		var state, local, tr string
		rows.Scan(&id, &state, &local, &tr)
		got = append(got, fmt.Sprintf("%d %s %s %s", id, state, local, tr))
	}
	want := []string{"1 pending  ", "2 pending  ciao", `3 downloaded 393331111111@s.whatsapp.net\3EB0A_image.jpg `, "4 pending  "}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
