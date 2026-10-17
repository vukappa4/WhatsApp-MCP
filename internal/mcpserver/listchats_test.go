package mcpserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// ListChats loads each chat's last message for a whole page at once; it must
// equal reading each chat's newest message at or before its LastMessageAt.
func TestListChatsLastMatchesOneByOne(t *testing.T) {
	fx, _ := fixtureDB(t)
	ctx := context.Background()
	// Edge cases go into a transaction that is rolled back, so the shared
	// fixture stays as it is.
	db, err := fx.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Rollback()
	// A poll vote after the newest message does not bump recency and must not
	// become the shown last message.
	var voteChat, last int64
	if err := db.QueryRowContext(ctx, `SELECT id, last_message_at FROM chats ORDER BY last_message_at DESC, id DESC LIMIT 1`).Scan(&voteChat, &last); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages (chat_id, wa_id, ts, kind) VALUES (?, 'FXVOTE', ?, 'poll_vote')`, voteChat, last+60); err != nil {
		t.Fatal(err)
	}
	// Two messages at the newest second: the higher id is the last one.
	var tieChat int64
	if err := db.QueryRowContext(ctx, `SELECT id, last_message_at FROM chats WHERE id != ? ORDER BY last_message_at DESC, id DESC LIMIT 1`, voteChat).Scan(&tieChat, &last); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages (chat_id, wa_id, ts, kind, text) VALUES (?, 'FXTIE', ?, 'text', 'tie')`, tieChat, last); err != nil {
		t.Fatal(err)
	}
	// A chat without messages has no last message.
	empty, err := store.EnsureChat(ctx, db, "120363999999999999@g.us", "group")
	if err != nil {
		t.Fatal(err)
	}

	var seen, reacted, none int
	f := store.ChatFilter{Limit: 37}
	for {
		chats, more, err := store.ListChats(ctx, db, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range chats {
			seen++
			want, _, err := store.ReadMessages(ctx, db, store.MessageQuery{ChatID: c.ID, Limit: 1, Before: c.LastMessageAt + 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(want) == 0 {
				if c.Last != nil {
					t.Fatalf("c_%d: got last m_%d, want none", c.ID, c.Last.ID)
				}
				none++
				continue
			}
			if c.Last == nil || !reflect.DeepEqual(*c.Last, want[0]) {
				t.Fatalf("c_%d last:\n got %+v\nwant %+v", c.ID, c.Last, want[0])
			}
			if c.ID == voteChat && c.Last.Kind == "poll_vote" {
				t.Fatalf("c_%d shows the poll vote as last message", c.ID)
			}
			if c.ID == tieChat && c.Last.WAID != "FXTIE" {
				t.Fatalf("c_%d shows %s, want the tied message with the higher id", c.ID, c.Last.WAID)
			}
			if len(c.Last.Reactions) > 0 {
				reacted++
			}
		}
		if !more {
			break
		}
		l := chats[len(chats)-1]
		f.HasCursor, f.AfterTS, f.AfterID = true, l.LastMessageAt, l.ID
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if seen != total {
		t.Fatalf("paged through %d chats, want %d", seen, total)
	}
	// The fixture must exercise every branch, or the comparison proves little.
	if reacted == 0 || none == 0 {
		t.Fatalf("fixture lacks cases: %d last messages with reactions, %d chats without messages (c_%d)", reacted, none, empty)
	}
}
