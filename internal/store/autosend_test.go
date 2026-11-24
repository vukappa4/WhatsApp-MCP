package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// v13 adds the autosend tables to a database at v12, and a fresh database
// has them too.
func TestMigrateV13AddsAutosendTables(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	all := migrations
	migrations = all[:12]
	db, err := Open(ctx, path)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM autosend_rules`); err == nil {
		t.Fatal("v12 already has autosend_rules")
	}
	db.Close()
	if db, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AutosendRules(ctx, db); err != nil {
		t.Fatalf("after upgrade: %v", err)
	}
	fresh, _ := open(t)
	if rs, err := AutosendRules(ctx, fresh); err != nil || len(rs) != 0 {
		t.Fatalf("fresh: %v %v", rs, err)
	}
}

func TestAutosendRules(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	r := AutosendRule{TaskID: "follow-up", TaskName: "Follow up", Schedule: "cron 0 9 * * *", Fingerprint: "f1", ApprovedAt: 100,
		Chats: []AutosendChat{{"123@lid", "Marco Rossi (+393331234567)"}, {"1203@g.us", "group Calcetto (c_2)"}, {"123@lid", "dup"}}}
	if err := PutAutosendRule(ctx, db, r); err != nil {
		t.Fatal(err)
	}
	got, err := AutosendRuleFor(ctx, db, "follow-up")
	if err != nil || got.Fingerprint != "f1" || got.ApprovedAt != 100 || len(got.Chats) != 2 || got.Chats[0].Label != "Marco Rossi (+393331234567)" {
		t.Fatalf("rule %+v %v", got, err)
	}
	// Approving again replaces the chats.
	r.Fingerprint, r.ApprovedAt, r.Chats = "f2", 200, []AutosendChat{{"999@lid", "Lorenzo"}}
	if err := PutAutosendRule(ctx, db, r); err != nil {
		t.Fatal(err)
	}
	if got, _ := AutosendRuleFor(ctx, db, "follow-up"); got.Fingerprint != "f2" || len(got.Chats) != 1 || got.Chats[0].JID != "999@lid" {
		t.Fatalf("replaced %+v", got)
	}
	if err := PutAutosendRule(ctx, db, AutosendRule{TaskID: "x"}); err == nil {
		t.Fatal("rule without chats stored")
	}
	PutAutosendRule(ctx, db, AutosendRule{TaskID: "weekly", TaskName: "Weekly", Fingerprint: "f", ApprovedAt: 300, Chats: []AutosendChat{{"1@lid", "A"}}})
	PutAutosendRule(ctx, db, AutosendRule{TaskID: "weekly-2", TaskName: "weekly", Fingerprint: "f", ApprovedAt: 50, Chats: []AutosendChat{{"1@lid", "A"}}})
	rs, err := AutosendRules(ctx, db)
	if err != nil || len(rs) != 3 || rs[0].TaskID != "weekly" || rs[2].TaskID != "weekly-2" {
		t.Fatalf("rules %+v %v", rs, err)
	}
	if _, err := AutosendRuleFor(ctx, db, "nope"); !errors.Is(err, ErrNoRule) {
		t.Fatalf("missing: %v", err)
	}

	// ResetDerived (reingest) keeps the rules.
	if err := db.Tx(ctx, func(tx *sql.Tx) error { return ResetDerived(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	if rs, _ := AutosendRules(ctx, db); len(rs) != 3 {
		t.Fatalf("after reset: %d rules", len(rs))
	}

	// Delete by name (case ignored) when unique, by id always; the chats go with the rule.
	if _, err := DeleteAutosendRule(ctx, db, "WEEKLY"); err == nil {
		t.Fatal("ambiguous name deleted")
	}
	if d, err := DeleteAutosendRule(ctx, db, "follow up"); err != nil || d.TaskID != "follow-up" {
		t.Fatalf("delete by name: %+v %v", d, err)
	}
	if d, err := DeleteAutosendRule(ctx, db, "weekly-2"); err != nil || d.TaskID != "weekly-2" {
		t.Fatalf("delete by id: %+v %v", d, err)
	}
	if _, err := DeleteAutosendRule(ctx, db, "follow-up"); !errors.Is(err, ErrNoRule) {
		t.Fatalf("delete twice: %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM autosend_chats WHERE task_id IN ('follow-up', 'weekly-2')`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d chats left after delete", n)
	}
}

func TestSendJID(t *testing.T) {
	lid := func(j string) JIDRow { return JIDRow{JID: j, Kind: "lid"} }
	pn := func(j string) JIDRow { return JIDRow{JID: j, Kind: "pn"} }
	for _, tc := range []struct {
		jids     []JIDRow
		phone    string
		want     string
		viaPhone bool
		err      error
	}{
		{[]JIDRow{lid("1@lid"), pn("393331234567@s.whatsapp.net")}, "+393331234567", "1@lid", false, nil},
		{[]JIDRow{pn("393331234567@s.whatsapp.net")}, "+393331234567", "393331234567@s.whatsapp.net", true, nil},
		{nil, "+393331234567", "393331234567@s.whatsapp.net", true, nil},
		{[]JIDRow{lid("1@lid"), lid("2@lid")}, "+393331234567", "", false, ErrManyLIDs{2}},
		{[]JIDRow{{JID: "x@bot", Kind: "other"}}, "", "", false, ErrNoSendJID},
	} {
		got, via, err := SendJID(tc.jids, tc.phone)
		if got != tc.want || via != tc.viaPhone || !errors.Is(err, tc.err) && err != tc.err {
			t.Errorf("SendJID(%v, %q) = %q %v %v", tc.jids, tc.phone, got, via, err)
		}
	}
}
