package mcpserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// ReadAround loads the context of every search hit at once; each window must
// equal what ReadMessages gives from that anchor.
func TestReadAroundMatchesOneByOne(t *testing.T) {
	fx, _ := fixtureDB(t)
	ctx := context.Background()
	// Edge cases go into a transaction that is rolled back, so the shared
	// fixture stays as it is.
	db, err := fx.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Rollback()
	// Three messages at one second in the busiest chat: the id breaks the tie.
	var tieChat, tieTS int64
	if err := db.QueryRowContext(ctx, `SELECT chat_id, MAX(ts) FROM messages GROUP BY chat_id ORDER BY COUNT(*) DESC LIMIT 1`).Scan(&tieChat, &tieTS); err != nil {
		t.Fatal(err)
	}
	tieTS -= 3600
	for _, wa := range []string{"FXTIE1", "FXTIE2", "FXTIE3"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO messages (chat_id, wa_id, ts, kind, text) VALUES (?, ?, ?, 'text', 'tie')`, tieChat, wa, tieTS); err != nil {
			t.Fatal(err)
		}
	}

	var anchors []store.Anchor
	add := func(query string, args ...any) {
		t.Helper()
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var a store.Anchor
			if err := rows.Scan(&a.ChatID, &a.TS, &a.ID); err != nil {
				t.Fatal(err)
			}
			anchors = append(anchors, a)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	// Neighbours in one chat, so windows overlap and share messages.
	add(`SELECT chat_id, ts, id FROM messages WHERE chat_id = ? ORDER BY ts DESC, id DESC LIMIT 6`, tieChat)
	add(`SELECT chat_id, ts, id FROM messages WHERE wa_id IN ('FXTIE1', 'FXTIE2', 'FXTIE3')`)
	// The first and the last message of a chat: one window is short or empty.
	add(`SELECT chat_id, ts, id FROM messages WHERE chat_id = ? ORDER BY ts, id LIMIT 1`, tieChat)
	// Messages next to reacted ones, so context lines carry reactions.
	add(`SELECT m.chat_id, m.ts, m.id FROM messages m WHERE EXISTS (SELECT 1 FROM messages r JOIN reactions x ON x.message_id = r.id
		WHERE r.chat_id = m.chat_id AND r.ts BETWEEN m.ts - 600 AND m.ts AND r.id != m.id) ORDER BY m.id LIMIT 15`)
	// A spread over many chats, the same anchor twice, and a missing chat.
	add(`SELECT chat_id, ts, id FROM messages WHERE id % 97 = 0 ORDER BY id LIMIT 30`)
	anchors = append(anchors, anchors[0], store.Anchor{ChatID: 999999, TS: tieTS, ID: 1})

	var shared, reacted, short bool
	for _, n := range []int{0, 1, 3} {
		before, after, err := store.ReadAround(ctx, db, anchors, n)
		if err != nil {
			t.Fatal(err)
		}
		if len(before) != len(anchors) || len(after) != len(anchors) {
			t.Fatalf("n=%d: %d/%d windows for %d anchors", n, len(before), len(after), len(anchors))
		}
		seen := map[int64]bool{}
		for i, a := range anchors {
			for dir, got := range [][]store.MessageRow{before[i], after[i]} {
				var want []store.MessageRow
				if n > 0 {
					if want, _, err = store.ReadMessages(ctx, db, store.MessageQuery{ChatID: a.ChatID, Limit: n, Newer: dir == 1,
						HasAnchor: true, AnchorTS: a.TS, AnchorID: a.ID}); err != nil {
						t.Fatal(err)
					}
				}
				if len(got) == 0 && len(want) == 0 {
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("n=%d anchor %+v dir %d:\n got %+v\nwant %+v", n, a, dir, got, want)
				}
				if n > 0 && len(got) < n {
					short = true
				}
				for _, m := range got {
					if seen[m.ID] && i < len(anchors)-2 {
						shared = true
					}
					seen[m.ID] = true
					if len(m.Reactions) > 0 {
						reacted = true
					}
				}
			}
		}
	}
	if !shared || !reacted || !short {
		t.Fatalf("fixture lacks a case: shared window %v, reacted context %v, short window %v", shared, reacted, short)
	}
}
