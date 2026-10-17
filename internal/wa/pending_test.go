package wa

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func editOf(id, text string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key: &waCommon.MessageKey{ID: proto.String(id)}, EditedMessage: textMsg(text)}}
}

func revokeOf(id string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key: &waCommon.MessageKey{ID: proto.String(id)}}}
}

func reactionTo(id, emoji string, at time.Time) *waE2E.Message {
	return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String(id)},
		Text: proto.String(emoji), SenderTimestampMS: proto.Int64(at.UnixMilli())}}
}

// Edits, revokes, reactions and poll votes that arrive before their message
// wait for it and apply when it is stored, live and in a reingest.
func TestMutationsBeforeTheirMessage(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	in.Now = func() time.Time { return t0.Add(time.Hour) }
	mine := types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoPN, Sender: myPN, IsFromMe: true}, ID: "R1", Timestamp: t0.Add(3 * time.Minute)}

	// All four arrive first: nothing is stored, nothing is created.
	live(t, in, dmFromMarco("E1", t0.Add(time.Minute)), editOf("M1", "versione corretta"))
	live(t, in, mine, reactionTo("M1", "👍", t0.Add(3*time.Minute)))
	live(t, in, dmFromMarco("X1", t0.Add(4*time.Minute)), revokeOf("M2"))
	h := sha256.Sum256([]byte("No"))
	if _, err := in.LiveMessage(ctx, &events.Message{Info: dmFromMarco("V1", t0.Add(5*time.Minute)), Message: &waE2E.Message{
		PollUpdateMessage: &waE2E.PollUpdateMessage{PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("POLL")}}}},
		&waE2E.PollVoteMessage{SelectedOptions: [][]byte{h[:]}}); err != nil {
		t.Fatal(err)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("mutations created %d messages", n)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM pending_mutations`); n != 4 {
		t.Fatalf("pending = %d, want 4", n)
	}
	// A redelivered copy is kept once.
	live(t, in, mine, reactionTo("M1", "👍", t0.Add(3*time.Minute)))
	if n := one[int](t, in, `SELECT COUNT(*) FROM pending_mutations`); n != 4 {
		t.Fatalf("duplicate pending: %d", n)
	}

	// The messages arrive: everything applies and the queue empties.
	live(t, in, dmFromMarco("M1", t0), textMsg("versione sbagliata"))
	live(t, in, dmFromMarco("M2", t0.Add(30*time.Second)), textMsg("da cancellare"))
	live(t, in, dmFromMarco("POLL", t0.Add(40*time.Second)), &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
		Name: proto.String("Pizza?"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Sì")}, {OptionName: proto.String("No")}}}})
	check := func(when string) {
		t.Helper()
		if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'M1'`); got != "versione corretta" {
			t.Fatalf("%s: edit not applied: %q", when, got)
		}
		if got := one[string](t, in, `SELECT emoji FROM reactions`); got != "👍" {
			t.Fatalf("%s: reaction = %q", when, got)
		}
		if got := one[int64](t, in, `SELECT COALESCE(deleted_at, 0) FROM messages WHERE wa_id = 'M2'`); got != t0.Add(4*time.Minute).Unix() {
			t.Fatalf("%s: revoke not applied: %d", when, got)
		}
		if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'V1'`); got != "voted: No" {
			t.Fatalf("%s: vote = %q", when, got)
		}
		if n := one[int](t, in, `SELECT COUNT(*) FROM pending_mutations`); n != 0 {
			t.Fatalf("%s: %d mutations still pending", when, n)
		}
	}
	check("live")

	// reingest replays the archive in arrival order, mutations first: same result.
	n, err := in.Reingest(ctx, nil)
	if err != nil || n == 0 {
		t.Fatalf("reingest: %d %v", n, err)
	}
	check("reingest")
}

// A mutation whose message never arrives expires after 30 days.
func TestPendingMutationsExpire(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	in.Now = func() time.Time { return t0 }
	live(t, in, dmFromMarco("E1", t0), editOf("GONE", "x"))
	if n, err := ExpirePending(ctx, in.DB, t0.Add(PendingTTL-time.Hour)); err != nil || n != 0 {
		t.Fatalf("expired early: %d %v", n, err)
	}
	if n, err := ExpirePending(ctx, in.DB, t0.Add(PendingTTL+time.Hour)); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	// One already older than the TTL is not kept at all.
	in.Now = func() time.Time { return t0.Add(PendingTTL + 24*time.Hour) }
	live(t, in, dmFromMarco("E2", t0), editOf("GONE2", "x"))
	if n := one[int](t, in, `SELECT COUNT(*) FROM pending_mutations`); n != 0 {
		t.Fatalf("stale mutation kept: %d", n)
	}
}
