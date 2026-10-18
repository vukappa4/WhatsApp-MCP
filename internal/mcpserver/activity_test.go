package mcpserver

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// activityOneByOne is the reference for store.GetPeopleActivity: the same
// facts for one person, one plain query at a time.
func activityOneByOne(t *testing.T, db store.Execer, id int64, maxGroups int) store.PersonActivity {
	t.Helper()
	ctx := context.Background()
	var a store.PersonActivity
	each := func(q string, fn func(*sql.Rows) error, args ...any) {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := fn(rows); err != nil {
				t.Fatal(err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	each(`SELECT jid, kind FROM jids WHERE person_id = ? ORDER BY kind DESC, jid`, func(r *sql.Rows) error {
		var j store.JIDRow
		a.JIDs = append(a.JIDs, j)
		return r.Scan(&a.JIDs[len(a.JIDs)-1].JID, &a.JIDs[len(a.JIDs)-1].Kind)
	}, id)
	each(`SELECT n.pn, jp.person_id FROM number_links n
		JOIN jids jl ON jl.jid = n.lid AND jl.person_id = ?1
		JOIN jids jp ON jp.jid = n.pn AND jp.person_id != ?1
		ORDER BY n.seen_at DESC, n.pn`, func(r *sql.Rows) error {
		var pn string
		var f store.FormerPhone
		err := r.Scan(&pn, &f.Owner)
		user, _, _ := strings.Cut(pn, "@")
		f.Phone = "+" + user
		a.Former = append(a.Former, f)
		return err
	}, id)
	var dmID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM chats WHERE kind = 'dm' AND person_id = ?`, id).Scan(&dmID); err == nil {
		c, _, err := store.ChatByID(ctx, db, dmID)
		if err != nil {
			t.Fatal(err)
		}
		dm := store.ChatActivity{Chat: c, LastAt: c.LastMessageAt}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_id = ?`, dmID).Scan(&dm.Messages); err != nil {
			t.Fatal(err)
		}
		a.DM = &dm
		a.LastAt, a.LastChatID = dm.LastAt, dmID
	} else if err != sql.ErrNoRows {
		t.Fatal(err)
	}
	each(`SELECT m.chat_id, COUNT(*), MAX(m.ts) FROM messages m JOIN chats c ON c.id = m.chat_id
		WHERE m.sender_person_id = ? AND c.kind != 'dm' GROUP BY m.chat_id ORDER BY MAX(m.ts) DESC, m.chat_id DESC`, func(r *sql.Rows) error {
		var g store.ChatActivity
		if err := r.Scan(&g.Chat.ID, &g.Messages, &g.LastAt); err != nil {
			return err
		}
		if a.GroupsTotal == 0 && g.LastAt > a.LastAt {
			a.LastAt, a.LastChatID = g.LastAt, g.Chat.ID
		}
		a.GroupsTotal++
		if len(a.Groups) < maxGroups {
			c, ok, err := store.ChatByID(ctx, db, g.Chat.ID)
			if ok {
				g.Chat = c
			}
			a.Groups = append(a.Groups, g)
			return err
		}
		return nil
	}, id)
	return a
}

// GetPeopleActivity for a whole page equals asking for each person alone.
func TestGetPeopleActivityMatchesOneByOne(t *testing.T) {
	fx, _ := fixtureDB(t)
	ctx := context.Background()
	// WhatsApp gives a former number to another account inside a transaction
	// that is rolled back, so the shared fixture stays as it is.
	db, err := fx.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Rollback()
	var pn, lid, other string
	if err := db.QueryRowContext(ctx, `SELECT pn, lid FROM number_links WHERE pn LIKE '39000001%' ORDER BY pn LIMIT 1`).Scan(&pn, &lid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT jid FROM jids WHERE kind = 'lid' AND jid != ? ORDER BY jid LIMIT 1`, lid).Scan(&other); err != nil {
		t.Fatal(err)
	}
	pnJID, _ := types.ParseJID(pn)
	otherJID, _ := types.ParseJID(other)
	if _, err := people.Link(ctx, db, pnJID, otherJID, fxNow); err != nil {
		t.Fatal(err)
	}
	all, err := store.AllPersonNames(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(all))
	for i := range all {
		ids[i] = all[i].ID
	}
	var former, capped, noDM, groupNewer int
	for _, maxGroups := range []int{0, 3, 10} {
		got, err := store.GetPeopleActivity(ctx, db, ids, maxGroups)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			want := activityOneByOne(t, db, id, maxGroups)
			if g := got[id]; !reflect.DeepEqual(g, want) {
				t.Fatalf("p_%d maxGroups %d:\n got %+v\nwant %+v", id, maxGroups, g, want)
			}
			if maxGroups == 3 {
				if len(want.Former) > 0 {
					former++
				}
				if want.GroupsTotal > 3 {
					capped++
				}
				if want.DM == nil && want.GroupsTotal > 0 {
					noDM++
				}
				if want.DM != nil && want.LastChatID != want.DM.Chat.ID {
					groupNewer++
				}
			}
		}
	}
	// The fixture must exercise every branch, or the comparison proves little.
	if former == 0 || capped == 0 || noDM == 0 || groupNewer == 0 {
		t.Fatalf("fixture lacks cases: %d with former numbers, %d over the group cap, %d in groups without a DM, %d with a group message newer than the DM",
			former, capped, noDM, groupNewer)
	}
}

// FillLastAt gives the newest DM message or message they sent.
func TestFillLastAt(t *testing.T) {
	db, _ := fixtureDB(t)
	ctx := context.Background()
	all, err := store.AllPersonNames(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	// Every person twice: a repeated person gets its LastAt too.
	all = append(all, all...)
	if err := store.FillLastAt(ctx, db, all); err != nil {
		t.Fatal(err)
	}
	var some int
	for _, c := range all {
		var dm, sent int64
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(last_message_at), 0) FROM chats WHERE kind = 'dm' AND person_id = ?`, c.ID).Scan(&dm); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts), 0) FROM messages WHERE sender_person_id = ?`, c.ID).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		if want := max(dm, sent); c.LastAt != want {
			t.Fatalf("p_%d LastAt %d, want %d", c.ID, c.LastAt, want)
		}
		if c.LastAt != 0 {
			some++
		}
	}
	if some == 0 {
		t.Fatal("no person has a LastAt")
	}
}
