package mcpserver

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// findChatsRef is the exact-name lookup as one plain OR over every name
// column, the reference store.FindChats must match.
const findChatsRef = `SELECT c.id FROM chats c LEFT JOIN persons p ON p.id = c.person_id
	WHERE (c.kind != 'dm' AND c.name = ?1 COLLATE NOCASE) OR (c.kind = 'dm' AND (p.display_name = ?1 COLLATE NOCASE
		OR p.full_name = ?1 COLLATE NOCASE OR p.first_name = ?1 COLLATE NOCASE OR p.push_name = ?1 COLLATE NOCASE
		OR p.business_name = ?1 COLLATE NOCASE))
	ORDER BY COALESCE(c.last_message_at, 0) DESC, c.id DESC LIMIT 20`

// TestFindChatsByNameMatchesReference looks up every group and person name
// of the fixture, in its own and in other letter cases, and requires the same
// chats in the same order as the plain OR reference.
func TestFindChatsByNameMatchesReference(t *testing.T) {
	db, _ := fixtureDB(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// The fixture has no business names, no person named like a group, and
	// every full name is the display name.
	for _, q := range []string{
		`UPDATE persons SET full_name = 'Fake Full ' || (id % 4) WHERE id % 13 = 0 AND full_name != ''`,
		`UPDATE persons SET business_name = 'Fake Shop ' || (id % 3) WHERE id % 17 = 0`,
		`UPDATE persons SET push_name = (SELECT name FROM chats WHERE kind = 'group' ORDER BY id LIMIT 1)
			WHERE id IN (SELECT person_id FROM chats WHERE kind = 'dm' ORDER BY id LIMIT 2)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// Every name column must hold a name that display_name alone does not
	// find, or a lookup that skipped that column would pass.
	for _, col := range []string{"full_name", "first_name", "push_name", "business_name"} {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM persons p JOIN chats c ON c.person_id = p.id AND c.kind = 'dm'
			WHERE p.`+col+` != '' AND p.`+col+` != p.display_name COLLATE NOCASE`).Scan(&n); err != nil || n == 0 {
			t.Fatalf("%s: %d dm persons with a name only in it (err %v)", col, n, err)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM chats WHERE kind != 'dm'
		UNION SELECT display_name FROM persons UNION SELECT full_name FROM persons UNION SELECT first_name FROM persons
		UNION SELECT push_name FROM persons UNION SELECT business_name FROM persons`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s != "" {
			names = append(names, s, strings.ToUpper(s), strings.ToLower(s))
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	names = append(names, "no such name", " ")
	var groups, dms, multi, mixed int
	for _, name := range names {
		rows, err := tx.QueryContext(ctx, findChatsRef, name)
		if err != nil {
			t.Fatal(err)
		}
		var want []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			want = append(want, id)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		chats, err := store.FindChats(ctx, tx, 0, "", name)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]int64, len(chats))
		kinds := map[bool]bool{}
		for i, c := range chats {
			got[i] = c.ID
			kinds[c.Kind == "dm"] = true
		}
		if !slices.Equal(got, want) {
			t.Fatalf("FindChats(%q) = %v, want %v", name, got, want)
		}
		if kinds[false] {
			groups++
		}
		if kinds[true] {
			dms++
		}
		if len(got) > 1 {
			multi++
		}
		if len(kinds) == 2 {
			mixed++
		}
	}
	if groups == 0 || dms == 0 || multi == 0 || mixed == 0 {
		t.Fatalf("coverage: %d names find groups, %d dms, %d several chats, %d both kinds; want each > 0", groups, dms, multi, mixed)
	}
	if chats, err := store.FindChats(ctx, tx, 0, "", ""); err != nil || len(chats) != 0 {
		t.Fatalf(`FindChats("") = %d chats, %v; want none`, len(chats), err)
	}
	t.Logf("%d names: %d find groups, %d dms, %d several chats, %d both kinds", len(names), groups, dms, multi, mixed)
}
