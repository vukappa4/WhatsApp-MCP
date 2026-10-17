package people

import (
	"context"
	"testing"
)

// When two DM chats merge, each app-state flag comes from the newer action.
func TestMergeChatsKeepsNewestFlags(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO persons(id, updated_at) VALUES (1, 0), (2, 0)`)
	mustExec(t, db, `INSERT INTO chats(id, jid, kind, person_id, created_at, archived, archived_set_at, pinned, pinned_set_at, state_updated_at)
		VALUES (1, 'a@s.whatsapp.net', 'dm', 1, 0, 1, 100, 0, NULL, 100),
		       (2, 'b@lid', 'dm', 2, 0, 0, 200, 1, 50, 200)`)
	if err := MergeChats(ctx, db, 1, 2); err != nil {
		t.Fatal(err)
	}
	var archived, pinned bool
	var archivedAt, pinnedAt int64
	if err := db.QueryRow(`SELECT archived, pinned, archived_set_at, pinned_set_at FROM chats WHERE id = 1`).Scan(&archived, &pinned, &archivedAt, &pinnedAt); err != nil {
		t.Fatal(err)
	}
	if archived || !pinned || archivedAt != 200 || pinnedAt != 50 {
		t.Fatalf("archived=%v@%d pinned=%v@%d, want the newer unarchive and the only pin", archived, archivedAt, pinned, pinnedAt)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM chats`); n != 1 {
		t.Fatalf("chats = %d", n)
	}
}
