package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Finding 2: a pair session that logged no heartbeat is not shown as offline
// from its last event to the next start.
func TestRecordDaemonStartAfterPairWithoutHeartbeat(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	base := time.Unix(1_000_000, 0)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	RecordDaemonStart(ctx, db, at(0), "run")
	Heartbeat(ctx, db, at(0))
	LogConnection(ctx, db, at(1), EvConnected, "")
	LogConnection(ctx, db, at(5), EvDaemonStop, "clean stop (signal)")
	// A pair session as pairCmd ran it before sharing the lifecycle: no
	// daemon_start, no heartbeat, no daemon_stop.
	LogConnection(ctx, db, at(10), EvPaired, "")
	LogConnection(ctx, db, at(10), EvConnected, "")
	LogConnection(ctx, db, at(20), EvAppStateSynced, "")
	if err := RecordDaemonStart(ctx, db, at(60), "run"); err != nil {
		t.Fatal(err)
	}
	LogConnection(ctx, db, at(61), EvConnected, "")

	var ts int64
	var detail string
	if err := db.QueryRow(`SELECT ts, detail FROM connection_log WHERE event = ? ORDER BY id DESC LIMIT 1`, EvDaemonStop).Scan(&ts, &detail); err != nil {
		t.Fatal(err)
	}
	if ts != at(60).Unix() || detail != StopNoHeartbeat {
		t.Fatalf("stop = %s %q, want at the new start with %q", time.Unix(ts, 0).Sub(base), detail, StopNoHeartbeat)
	}
	gaps, err := Gaps(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gaps {
		if g.To.Sub(g.From) > 5*time.Minute {
			t.Fatalf("long gap fabricated: %+v", g)
		}
	}
	if len(gaps) != 2 {
		t.Fatalf("gaps = %+v, want the real stop-to-pair gap and the restart", gaps)
	}

	// A session that beat is still closed at its last heartbeat.
	Heartbeat(ctx, db, at(70))
	if err := RecordDaemonStart(ctx, db, at(90), "run"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT ts, detail FROM connection_log WHERE event = ? ORDER BY id DESC LIMIT 1`, EvDaemonStop).Scan(&ts, &detail); err != nil {
		t.Fatal(err)
	}
	if ts != at(70).Unix() || detail != StopUnclean {
		t.Fatalf("crash stop = %s %q", time.Unix(ts, 0).Sub(base), detail)
	}
}

// Findings 2, 3, 5: migration v3 repairs fabricated pair-session stops,
// deletes DM chats without messages (keeping persons) and names WhatsApp's
// own account. It runs once.
func TestMigrationV3RepairsExistingData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	all := migrations
	migrations = all[:2]
	db, err := Open(ctx, path)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// connection_log written by the old binaries: run, clean stop, pair
	// without stop, then a start that closed the pair at its last event.
	for _, r := range []struct {
		ts            int64
		event, detail string
	}{
		{100, EvDaemonStart, ""}, {101, EvConnected, ""}, {200, EvDaemonStop, "clean stop (signal)"},
		{300, EvPaired, ""}, {300, EvConnected, ""}, {320, EvAppStateSynced, ""},
		{320, EvDaemonStop, StopUnclean}, {900, EvDaemonStart, ""}, {901, EvConnected, ""},
		// A real crash of a run session keeps its stop.
		{950, EvDisconnected, ""}, {960, EvDaemonStop, StopUnclean}, {990, EvDaemonStart, ""},
		// A run that crashed, then a pair: the stop also closes the crash, so it stays.
		{991, EvConnected, ""}, {995, EvPaired, ""}, {999, EvDaemonStop, StopUnclean}, {1500, EvDaemonStart, ""},
	} {
		exec(`INSERT INTO connection_log(ts, event, detail) VALUES (?, ?, ?)`, r.ts, r.event, r.detail)
	}
	exec(`INSERT INTO persons(id, display_name, name_source, phone_e164, updated_at) VALUES (1, '+0', 'phone', '+0', 0), (2, '+39 333', 'phone', '+39333', 0)`)
	exec(`INSERT INTO chats(id, jid, kind, person_id, created_at) VALUES (1, '0@s.whatsapp.net', 'dm', 1, 0), (2, '39333@s.whatsapp.net', 'dm', 2, 0)`)
	exec(`INSERT INTO chats(id, jid, kind, created_at) VALUES (3, '1@g.us', 'group', 0)`)
	exec(`INSERT INTO messages(chat_id, wa_id, ts, kind, text) VALUES (1, 'W1', 10, 'text', 'security code changed')`)

	if err := db.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var ts int64
	var detail string
	if err := db.QueryRow(`SELECT ts, detail FROM connection_log WHERE event = ? AND id = 7`, EvDaemonStop).Scan(&ts, &detail); err != nil {
		t.Fatal(err)
	}
	if ts != 900 {
		t.Fatalf("pair stop at %d (%q), want moved to the next start at 900", ts, detail)
	}
	if err := db.QueryRow(`SELECT ts, detail FROM connection_log WHERE id = 11`).Scan(&ts, &detail); err != nil {
		t.Fatal(err)
	}
	if ts != 960 || detail != StopUnclean {
		t.Fatalf("crash stop changed to %d %q", ts, detail)
	}
	if err := db.QueryRow(`SELECT ts FROM connection_log WHERE id = 15`).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts != 999 {
		t.Fatalf("stop after a crashed run then a pair moved to %d", ts)
	}
	gaps, err := Gaps(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gaps {
		if g.From.Unix() == 320 {
			t.Fatalf("pair session still shown offline: %+v", g)
		}
	}
	var chats, persons int
	db.QueryRow(`SELECT (SELECT COUNT(*) FROM chats), (SELECT COUNT(*) FROM persons)`).Scan(&chats, &persons)
	if chats != 2 || persons != 2 {
		t.Fatalf("chats=%d persons=%d, want the empty DM gone and both persons kept", chats, persons)
	}
	var name, source string
	db.QueryRow(`SELECT display_name, name_source FROM persons WHERE id = 1`).Scan(&name, &source)
	if name != "WhatsApp" || source != "system" {
		t.Fatalf("system account = %q/%q", name, source)
	}
}

// Findings 3, 6: empty chats are not counted as unnamed, and a chat's last
// message is the one its time refers to, never a later poll vote.
func TestStatsAndLastMessageIgnoreNoise(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO chats(id, jid, kind, created_at, last_message_at) VALUES (1, '1@g.us', 'group', 0, NULL), (2, '2@g.us', 'group', 0, 100)`,
		`INSERT INTO messages(chat_id, wa_id, ts, kind, text) VALUES (2, 'M', 100, 'text', 'ciao'), (2, 'V', 150, 'poll_vote', 'voted: si')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st, err := GetStats(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if st.UnnamedChats != 1 {
		t.Fatalf("unnamed = %d, want only the group with messages", st.UnnamedChats)
	}
	chats, _, err := ListChats(ctx, db, ChatFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chats {
		if c.ID == 2 && (c.Last == nil || c.Last.WAID != "M") {
			t.Fatalf("last message = %+v, want M (the poll vote does not move the chat time)", c.Last)
		}
	}
}
