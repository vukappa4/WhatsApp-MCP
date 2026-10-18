package mcpserver

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// formerPhoneRef is the former-number lookup as a plain query: persons whose
// LID once had the number ?1 that they do not hold now.
const formerPhoneRef = `SELECT DISTINCT jl.person_id FROM number_links n JOIN jids jl ON jl.jid = n.lid
	WHERE n.pn = ?1 AND jl.person_id NOT IN (SELECT person_id FROM jids WHERE jid = ?1)`

// TestPersonsByNumberMatchesReference looks up every phone number, former
// number and LID of the fixture, and requires store.PersonsByNumber to return
// exactly the persons and match kinds of the separate phone, former-number
// and LID lookups.
func TestPersonsByNumberMatchesReference(t *testing.T) {
	fdb, _ := fixtureDB(t)
	ctx := context.Background()
	db, err := fdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Rollback()
	// The fixture has no number that one person's LID had and another person
	// holds now. Give a few LIDs the current number of the next person, and
	// one LID a number nobody holds.
	for _, q := range []string{
		`INSERT INTO number_links(pn, lid, seen_at) SELECT pn.jid, lid.jid, 0
			FROM jids lid JOIN jids pn ON pn.kind = 'pn' AND pn.person_id = lid.person_id + 1
			WHERE lid.kind = 'lid' AND lid.person_id % 29 = 0`,
		`INSERT INTO number_links(pn, lid, seen_at) SELECT '390000099999@s.whatsapp.net', jid, 0
			FROM jids WHERE kind = 'lid' ORDER BY jid LIMIT 1`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT jid FROM jids WHERE kind IN ('pn', 'lid') UNION SELECT pn FROM number_links`)
	if err != nil {
		t.Fatal(err)
	}
	var pns, lids []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			t.Fatal(err)
		}
		user, server, _ := strings.Cut(j, "@")
		if server == "lid" {
			lids = append(lids, user)
		} else {
			pns = append(pns, user)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	type pair struct{ digits, lid string }
	var cases []pair
	for _, d := range append(append([]string{"390000000000", "1"}, pns...), lids...) {
		cases = append(cases, pair{d, d})
	}
	// A phone number and a LID of different persons in one call.
	for i := range min(len(pns), len(lids)) {
		cases = append(cases, pair{pns[i], lids[len(lids)-1-i]})
	}

	type hit struct {
		store.PersonCandidate
		match store.NumberMatch
	}
	order := func(a, b hit) int { return cmp.Or(cmp.Compare(a.match, b.match), cmp.Compare(a.ID, b.ID)) }
	kinds := map[store.NumberMatch]int{}
	var mixed int
	for _, c := range cases {
		var want []hit
		phone, err := store.PersonsByPhone(ctx, db, c.digits)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range phone {
			want = append(want, hit{p, store.MatchPhone})
		}
		rows, err := db.QueryContext(ctx, formerPhoneRef, c.digits+"@s.whatsapp.net")
		if err != nil {
			t.Fatal(err)
		}
		var former []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			former = append(former, id)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, id := range former {
			p, ok, err := store.PersonCandidateByID(ctx, db, id)
			if err != nil || !ok {
				t.Fatalf("p_%d: found %v, err %v", id, ok, err)
			}
			want = append(want, hit{p, store.MatchFormer})
		}
		lid, err := store.PersonsByJID(ctx, db, c.lid+"@lid")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range lid {
			want = append(want, hit{p, store.MatchLID})
		}

		ns, err := store.PersonsByNumber(ctx, db, c.digits, c.lid)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]hit, len(ns))
		for i, n := range ns {
			got[i] = hit{n.PersonCandidate, n.Match}
		}
		slices.SortFunc(want, order)
		slices.SortFunc(got, order)
		if !slices.Equal(got, want) {
			t.Fatalf("PersonsByNumber(%q, %q):\n got %+v\nwant %+v", c.digits, c.lid, got, want)
		}
		seen := map[store.NumberMatch]bool{}
		for _, h := range got {
			kinds[h.match]++
			seen[h.match] = true
		}
		if len(seen) > 1 {
			mixed++
		}
	}
	if kinds[store.MatchPhone] == 0 || kinds[store.MatchFormer] == 0 || kinds[store.MatchLID] == 0 || mixed == 0 {
		t.Fatalf("fixture lacks cases: matches per kind %v, %d lookups with several kinds", kinds, mixed)
	}
}
