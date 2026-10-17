package mcpserver

import (
	"go.mau.fi/whatsmeow/types"
	"strings"
	"testing"
)

func TestBeforeSendGoesToLID(t *testing.T) {
	e := newSendEnv(t)
	// Marco A (+39 333 111 1111) has LID 555@lid and a newer number +39 333 999 9999.
	p := e.one(t, `SELECT person_id FROM jids WHERE jid = '393331111111@s.whatsapp.net'`)
	for _, q := range []string{
		`INSERT INTO jids(jid, person_id, kind) VALUES ('555555555555555@lid', ` + p + `, 'lid'), ('393339999999@s.whatsapp.net', ` + p + `, 'pn')`,
	} {
		if _, err := e.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "p_" + p, "text": "ciao"})
	if isErr {
		t.Fatal(text)
	}
	if to := e.fake.sends()[0].to.String(); to != "555555555555555@lid" {
		t.Fatalf("sent to %s, want the LID", to)
	}
	_ = strings.Contains
}

func TestBeforeSendReplyWithOtherLIDNoMerge(t *testing.T) {
	e := newSendEnv(t)
	if _, err := e.db.Exec(`INSERT INTO persons(id, display_name, name_source, updated_at) VALUES (99, 'Stranger', 'push_name', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO jids(jid, person_id, kind) VALUES ('444444444444444@lid', 99, 'lid')`); err != nil {
		t.Fatal(err)
	}
	e.fake.answer = map[types.JID]types.JID{types.NewJID("393332222222", types.DefaultUserServer): types.NewJID("444444444444444", types.HiddenUserServer)}
	text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 333 222 2222", "text": "ciao"})
	if isErr {
		t.Fatal(text)
	}
	if n := e.one(t, `SELECT COUNT(DISTINCT person_id) FROM jids WHERE jid IN ('393332222222@s.whatsapp.net', '444444444444444@lid')`); n != "2" {
		t.Fatalf("the contact and the stranger are %s person(s), want 2", n)
	}
}
