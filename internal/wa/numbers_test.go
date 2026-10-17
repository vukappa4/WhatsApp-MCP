package wa

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Marco's number is known from the first history sync; he then writes from
// a new number, and WhatsApp later gives his old number to a stranger. Marco
// stays one person with the new number, the stranger is another person, and
// a reingest of the same events rebuilds exactly the same rows.
func TestNumberChangeAndReassignmentSurviveReingest(t *testing.T) {
	ctx := context.Background()
	in := newIngestor(t)
	in.Now = func() time.Time { return t0 } // the blob arrives at t0
	oldNum, newNum := marcoPN, types.NewJID("393335555555", types.DefaultUserServer)
	strangerL := types.NewJID("222222222222222", types.HiddenUserServer)

	hs := historyEvent(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, &waHistorySync.Conversation{
		ID:       proto.String(oldNum.String()),
		Messages: []*waHistorySync.HistorySyncMsg{webMsg("H1", false, "", t0.Add(-time.Hour), textMsg("vecchio numero"))},
	})
	hs.Data.PhoneNumberToLidMappings = []*waHistorySync.PhoneNumberToLIDMapping{{PnJID: proto.String(oldNum.String()), LidJID: proto.String(marcoL.String())}}
	if err := in.HistorySync(ctx, hs); err != nil {
		t.Fatal(err)
	}
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoL, Sender: marcoL, SenderAlt: newNum},
		ID: "N1", Timestamp: t0.Add(time.Hour), PushName: "Marchino"}, textMsg("nuovo numero"))

	marco := one[int64](t, in, `SELECT person_id FROM jids WHERE jid = ?`, marcoL.String())
	if got := one[string](t, in, `SELECT phone_e164 FROM persons WHERE id = ?`, marco); got != "+393335555555" {
		t.Fatalf("phone = %s, want the new number", got)
	}
	if n := one[int](t, in, `SELECT COUNT(DISTINCT chat_id) FROM messages WHERE wa_id IN ('H1', 'N1')`); n != 1 {
		t.Fatalf("the old and the new number's messages are in %d chats, want Marco's one DM", n)
	}

	// WhatsApp gives the old number to someone else, who writes.
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: oldNum, Sender: oldNum, SenderAlt: strangerL},
		ID: "S1", Timestamp: t0.Add(48 * time.Hour), PushName: "Sconosciuto"}, textMsg("chi sei?"))
	stranger := one[int64](t, in, `SELECT person_id FROM jids WHERE jid = ?`, strangerL.String())
	if stranger == marco {
		t.Fatal("the stranger was merged into Marco")
	}
	if got := one[int64](t, in, `SELECT person_id FROM jids WHERE jid = ?`, oldNum.String()); got != stranger {
		t.Fatal("the old number still belongs to Marco")
	}
	if got := one[string](t, in, `SELECT phone_e164 || ' ' || display_name FROM persons WHERE id = ?`, marco); got != "+393335555555 Marchino" {
		t.Fatalf("Marco = %q", got)
	}
	if one[int64](t, in, `SELECT chat_id FROM messages WHERE wa_id = 'S1'`) == one[int64](t, in, `SELECT chat_id FROM messages WHERE wa_id = 'N1'`) {
		t.Fatal("the stranger's message landed in Marco's chat")
	}

	before := identityDump(t, in)
	if _, err := in.Reingest(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if after := identityDump(t, in); after != before {
		t.Fatalf("reingest differs\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
