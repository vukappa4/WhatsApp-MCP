package people

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

var (
	numA = types.NewJID("393331111111", types.DefaultUserServer)
	numB = types.NewJID("393332222222", types.DefaultUserServer)
	lidX = types.NewJID("777777777777777", types.HiddenUserServer)
	t1   = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	t2   = t1.Add(30 * 24 * time.Hour)
	t3   = t2.Add(200 * 24 * time.Hour)
)

func mustLink(t *testing.T, db *store.DB, a, b types.JID, at time.Time) int64 {
	t.Helper()
	id, err := Link(context.Background(), db, a, b, at)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func personOf(t *testing.T, db *store.DB, j types.JID) int64 {
	t.Helper()
	id, err := Lookup(context.Background(), db, j)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func phoneOf(t *testing.T, db *store.DB, id int64) string {
	t.Helper()
	var phone string
	if err := db.QueryRow(`SELECT COALESCE(phone_e164, '') FROM persons WHERE id = ?`, id).Scan(&phone); err != nil {
		t.Fatal(err)
	}
	return phone
}

// A contact who changes number stays one person; the new number is current
// and the old one is still theirs.
func TestNumberChangeKeepsOnePersonWithTheNewNumber(t *testing.T) {
	db := openDB(t)
	p := mustLink(t, db, lid, numA, t1)
	if q := mustLink(t, db, lid, numB, t2); q != p {
		t.Fatalf("new number made person %d, want %d", q, p)
	}
	if got := phoneOf(t, db, p); got != "+393332222222" {
		t.Fatalf("phone = %s, want the new number", got)
	}
	if personOf(t, db, numA) != p {
		t.Fatal("the old number left the person while no one else had it")
	}
	// The same evidence again, or an older one, changes nothing.
	mustLink(t, db, lid, numA, t1)
	if got := phoneOf(t, db, p); got != "+393332222222" {
		t.Fatalf("phone after an old observation = %s", got)
	}
}

// Observations applied out of order end the same: the newest one wins.
func TestNumberChangeOutOfOrder(t *testing.T) {
	db := openDB(t)
	p := mustLink(t, db, numB, lid, t2)
	mustLink(t, db, numA, lid, t1)
	if got := phoneOf(t, db, p); got != "+393332222222" {
		t.Fatalf("phone = %s, want the newer number", got)
	}
	// The contact goes back to the first number: newer evidence makes it current.
	mustLink(t, db, lid, numA, t3)
	if got := phoneOf(t, db, p); got != "+393331111111" {
		t.Fatalf("phone = %s after going back", got)
	}
}

// WhatsApp gives the contact's old number to a stranger: the number moves to
// the stranger's person; the contact keeps its names, DM chat and new number.
func TestReassignedNumberMovesWithoutMerging(t *testing.T) {
	ctx := context.Background()
	for _, order := range []string{"live", "reversed"} {
		t.Run(order, func(t *testing.T) {
			db := openDB(t)
			steps := []func(){
				func() { mustLink(t, db, lid, numA, t1) },
				func() { mustLink(t, db, lid, numB, t2) },
				func() { mustLink(t, db, lidX, numA, t3) },
			}
			if order == "reversed" {
				steps[0], steps[2] = steps[2], steps[0]
			}
			for _, s := range steps {
				s()
			}
			contact, stranger := personOf(t, db, lid), personOf(t, db, lidX)
			if contact == stranger {
				t.Fatal("the stranger was merged into the contact")
			}
			if err := SetNames(ctx, db, contact, Names{FullName: "Lorenzo Bianchi"}); err != nil {
				t.Fatal(err)
			}
			chat, err := EnsureDMChat(ctx, db, contact, lid)
			if err != nil {
				t.Fatal(err)
			}
			mustLink(t, db, lidX, numA, t3.Add(time.Hour)) // the stranger writes again
			if personOf(t, db, numA) != stranger {
				t.Fatal("the reassigned number does not belong to its new owner")
			}
			if got, want := phoneOf(t, db, contact)+" "+phoneOf(t, db, stranger), "+393332222222 +393331111111"; got != want {
				t.Fatalf("phones %q, want %q", got, want)
			}
			if c, _ := DMChatID(ctx, db, contact); c != chat {
				t.Fatal("the contact's DM chat moved")
			}
			if n := count(t, db, `SELECT COUNT(*) FROM persons WHERE id = ? AND full_name = 'Lorenzo Bianchi'`, contact); n != 1 {
				t.Fatal("the contact's name changed")
			}
			if n := count(t, db, `SELECT COUNT(*) FROM persons WHERE id = ? AND full_name = ''`, stranger); n != 1 {
				t.Fatal("the stranger took the contact's name")
			}
			if n := count(t, db, `SELECT COUNT(*) FROM identity_notes WHERE jid = ? AND other_jid = ? AND note = 'number reassigned'`,
				numA.String(), lidX.String()); n != 1 {
				t.Fatal("reassignment not noted")
			}
			// The contact's old number stays recorded as theirs once.
			if n := count(t, db, `SELECT COUNT(*) FROM number_links WHERE pn = ? AND lid = ?`, numA.String(), lid.String()); n != 1 {
				t.Fatal("former number forgotten")
			}
		})
	}
}

// A person known only by a number merges into the LID that has it: that is
// the whole point of a link.
func TestNumberOnlyPersonMergesIntoItsLID(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	pp, err := Resolve(ctx, db, numA)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetNames(ctx, db, pp, Names{FullName: "Anna Neri"}); err != nil {
		t.Fatal(err)
	}
	pl, err := Resolve(ctx, db, lid)
	if err != nil {
		t.Fatal(err)
	}
	got := mustLink(t, db, lid, numA, t1)
	if got != pp || personOf(t, db, lid) != pp || personOf(t, db, numA) != pp {
		t.Fatalf("link = %d, want the older person %d holding both (lid person was %d)", got, pp, pl)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM persons`); n != 1 {
		t.Fatalf("persons = %d", n)
	}
}

// Two numbers or two LIDs prove nothing; Merge refuses two LIDs.
func TestNoLinkWithoutALIDAndANumber(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	a := mustLink(t, db, numA, numB, t1)
	if b := personOf(t, db, numB); a == b || b == 0 {
		t.Fatalf("two numbers: persons %d %d", a, b)
	}
	x := mustLink(t, db, lid, lidX, t1)
	y := personOf(t, db, lidX)
	if x == y || y == 0 {
		t.Fatalf("two LIDs: persons %d %d", x, y)
	}
	if _, err := Merge(ctx, db, x, y); !errors.Is(err, ErrTwoAccounts) {
		t.Fatalf("merge of two LIDs: %v", err)
	}
}

// A source without times confirms a link in force without writing.
func TestLinkCurrentWritesNothingForAKnownLink(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	p := mustLink(t, db, lid, numA, t1)
	mustLink(t, db, lid, numB, t2)
	got, err := LinkCurrent(ctx, db, numA, lid, t3)
	if err != nil || got != p {
		t.Fatalf("LinkCurrent = %d, %v", got, err)
	}
	if n := count(t, db, `SELECT seen_at FROM number_links WHERE pn = ?`, numA.String()); int64(n) != t1.Unix() {
		t.Fatal("a known link was rewritten")
	}
	if got := phoneOf(t, db, p); got != "+393332222222" {
		t.Fatalf("phone = %s, want the newest observed number", got)
	}
	// A new pair is a link at the given time.
	if _, err := LinkCurrent(ctx, db, lidX, numA, t3); err != nil {
		t.Fatal(err)
	}
	if personOf(t, db, numA) != personOf(t, db, lidX) {
		t.Fatal("a new current link did not move the number")
	}
}

// Whatever the observations and their order, no person holds two LIDs, every
// number belongs to the LID that had it last, and every LID's current number
// is the newest it holds.
func TestLinksPartitionByLastOwnerInAnyOrder(t *testing.T) {
	ctx := context.Background()
	type obs struct {
		l, pn types.JID
		at    time.Time
	}
	for seed := range int64(30) {
		r := rand.New(rand.NewSource(seed))
		var all []obs
		minutes := r.Perm(1000)
		for i := range 40 { // distinct times: ties are the only order-dependent case
			all = append(all, obs{
				types.NewJID(fmt.Sprintf("10000000000000%d", r.Intn(5)), types.HiddenUserServer),
				types.NewJID(fmt.Sprintf("3933300000%02d", r.Intn(8)), types.DefaultUserServer),
				t1.Add(time.Duration(minutes[i]) * time.Minute),
			})
		}
		var want string
		for run := range 3 {
			db := openDB(t)
			order := r.Perm(len(all))
			for _, i := range order {
				o := all[i]
				// Some numbers are known alone first, as a contact or a message.
				if r.Intn(4) == 0 {
					if _, err := Resolve(ctx, db, o.pn); err != nil {
						t.Fatal(err)
					}
				}
				mustLink(t, db, o.l, o.pn, o.at)
			}
			if n := count(t, db, `SELECT COUNT(*) FROM (SELECT person_id FROM jids WHERE kind = 'lid' GROUP BY person_id HAVING COUNT(*) > 1)`); n != 0 {
				t.Fatalf("seed %d: %d persons hold more than one LID", seed, n)
			}
			if n := count(t, db, `SELECT COUNT(*) FROM jids j WHERE j.kind = 'pn' AND EXISTS (SELECT 1 FROM number_links n WHERE n.pn = j.jid)
				AND j.person_id != (SELECT jl.person_id FROM number_links n JOIN jids jl ON jl.jid = n.lid WHERE n.pn = j.jid ORDER BY n.seen_at DESC LIMIT 1)`); n != 0 {
				t.Fatalf("seed %d: %d numbers are not with the LID that had them last", seed, n)
			}
			got := ownership(t, db)
			if run == 0 {
				want = got
			} else if got != want {
				t.Fatalf("seed %d: order changed the result\nwant:\n%s\ngot:\n%s", seed, want, got)
			}
		}
	}
}

// ownership renders each LID's numbers and current number.
func ownership(t *testing.T, db *store.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT jl.jid, COALESCE(p.phone_e164, ''),
		COALESCE((SELECT group_concat(jid, ',') FROM (SELECT jid FROM jids WHERE person_id = p.id AND kind = 'pn' ORDER BY jid)), '')
		FROM jids jl JOIN persons p ON p.id = jl.person_id WHERE jl.kind = 'lid' ORDER BY jl.jid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var l, phone, nums string
		if err := rows.Scan(&l, &phone, &nums); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&b, l, phone, nums)
	}
	return b.String()
}
