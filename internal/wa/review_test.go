package wa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/logx"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

func historyEvent(sync waHistorySync.HistorySync_HistorySyncType, convs ...*waHistorySync.Conversation) *events.HistorySync {
	return &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: sync.Enum(), Conversations: convs}}
}

// Finding 1: acks wait for the handler and decrypted plaintext is buffered.
func TestClientConfiguresDurableAcks(t *testing.T) {
	ctx := context.Background()
	sdb, container, err := OpenSession(ctx, filepath.Join(t.TempDir(), "session.db"), logx.New(&bytes.Buffer{}, false))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	dev, err := container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cli := whatsmeow.NewClient(dev, nil)
	configure(cli)
	if !cli.SynchronousAck || !cli.EnableDecryptedEventBuffer || !cli.ManualHistorySyncDownload {
		t.Fatalf("SynchronousAck=%v EnableDecryptedEventBuffer=%v ManualHistorySyncDownload=%v",
			cli.SynchronousAck, cli.EnableDecryptedEventBuffer, cli.ManualHistorySyncDownload)
	}
}

// Finding 2: a failing conversation does not stop the rest of the blob, and
// the blob is archived before ingestion.
func TestHistoryBlobContinuesAfterFailedConversation(t *testing.T) {
	in := newIngestor(t)
	testHookConversation = func(id string) error {
		if id == group.String() {
			return errors.New("boom")
		}
		return nil
	}
	defer func() { testHookConversation = nil }()
	hs := historyEvent(waHistorySync.HistorySync_RECENT,
		&waHistorySync.Conversation{ID: proto.String(group.String()), Messages: []*waHistorySync.HistorySyncMsg{
			webMsg("G1", false, marcoPN.String(), t0, textMsg("persa")),
		}},
		&waHistorySync.Conversation{ID: proto.String(marcoPN.String()), Messages: []*waHistorySync.HistorySyncMsg{
			webMsg("D1", false, "", t0, textMsg("salvata")),
		}},
	)
	if err := in.HistorySync(context.Background(), hs); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the failed conversation in the error, got %v", err)
	}
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'D1'`); got != "salvata" {
		t.Fatalf("later conversation not stored: %q", got)
	}
	if got := one[string](t, in, `SELECT error FROM raw_events WHERE kind = 'history'`); !strings.Contains(got, "boom") {
		t.Fatalf("raw event error = %q", got)
	}
	// The archive keeps the blob: replaying it after the bug is fixed recovers the conversation.
	testHookConversation = nil
	n, err := in.Reingest(context.Background(), nil)
	if err != nil || n != 1 {
		t.Fatalf("reingest = %d, %v", n, err)
	}
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'G1'`); got != "persa" {
		t.Fatalf("reingest did not recover G1: %q", got)
	}
}

// Addition A: a live message archived but never ingested (crash between
// the two) is ingested by the startup replay.
func TestPendingRawMessageReplayed(t *testing.T) {
	in := newIngestor(t)
	payload, key, err := encodeMessage(&events.Message{Info: dmFromMarco("R1", t0), Message: textMsg("dopo il crash")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.DB.ArchiveRaw(context.Background(), store.RawMessage, key, payload, t0); err != nil {
		t.Fatal(err)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatal("archive alone must not ingest")
	}
	if n, err := in.ReplayPending(context.Background()); err != nil || n != 1 {
		t.Fatalf("replay = %d, %v", n, err)
	}
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'R1'`); got != "dopo il crash" {
		t.Fatalf("text = %q", got)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM raw_events WHERE ingested_at IS NULL`); n != 0 {
		t.Fatal("raw event still pending")
	}
}

// Finding 3: the pairing wait restarts on every arrival and drains pending work.
func TestWaitQuietRestartsOnArrivalAndDrains(t *testing.T) {
	arrivals := make(chan struct{}, 4)
	pending := 2
	start := time.Now()
	go func() {
		time.Sleep(60 * time.Millisecond)
		arrivals <- struct{}{} // arrives after more than half of idle: must extend the wait
	}()
	var out bytes.Buffer
	waitQuiet(context.Background(), arrivals, func() int { pending--; return max(pending, 0) }, 100*time.Millisecond, 10*time.Millisecond, time.Minute, &out)
	if el := time.Since(start); el < 160*time.Millisecond {
		t.Fatalf("returned after %s, before arrival+idle", el)
	}
	if pending > 0 || !strings.Contains(out.String(), "waiting for 1 history blobs") {
		t.Fatalf("did not drain pending work: pending=%d out=%q", pending, out.String())
	}
}

// Finding 4: a richer source upgrades a legacy row; a history revoke stub
// marks a stored message deleted.
func TestHistoryUpgradesLegacyRowAndRevokes(t *testing.T) {
	ctx := context.Background()
	in := newIngestor(t)
	legacyBatch = 2 // exercise batching
	defer func() { legacyBatch = 500 }()
	msgs, sess := legacyFixture(t)
	if _, err := ImportLegacy(ctx, in.DB, msgs, sess); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT source FROM messages WHERE wa_id = 'L2'`); got != "legacy" {
		t.Fatalf("source = %q", got)
	}
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: proto.String("didascalia vera"), Mimetype: proto.String("image/png"), DirectPath: proto.String("/v/new"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("L1"), QuotedMessage: textMsg("ciao dal lid")},
	}}
	revoked := webMsg("P1", false, "", t0, nil)
	revoked.Message.MessageStubType = waWeb.WebMessageInfo_REVOKE.Enum()
	hs := historyEvent(waHistorySync.HistorySync_RECENT, &waHistorySync.Conversation{
		ID: proto.String(marcoL.String()),
		Messages: []*waHistorySync.HistorySyncMsg{
			webMsg("L2", true, "", time.Date(2026, 10, 13, 7, 5, 0, 0, time.UTC), img),
			revoked,
		},
	})
	if err := in.HistorySync(ctx, hs); err != nil {
		t.Fatal(err)
	}
	got := one[string](t, in, `SELECT m.source || '|' || m.text || '|' || m.reply_to_wa_id || '|' || md.mimetype || '|' || md.direct_path || '|' || hex(md.media_key)
		FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = 'L2'`)
	if got != "history|didascalia vera|L1|image/png|/v/new|01" {
		t.Fatalf("upgraded row = %q", got)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages WHERE wa_id = 'P1' AND deleted_at IS NOT NULL AND text = 'ciao dal numero'`); n != 1 {
		t.Fatal("history revoke stub did not mark the stored message deleted")
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM media`); n != 1 {
		t.Fatalf("media rows = %d, want 1 (merged, not duplicated)", n)
	}
}

// Finding 6: only the author edits; only the author or an admin revokes;
// an older edit never overwrites a newer one.
func TestEditRevokeAuthorship(t *testing.T) {
	in := newIngestor(t)
	other := types.NewJID("393339999999", types.DefaultUserServer)
	inGroup := func(sender types.JID, id string, at time.Time) types.MessageInfo {
		return types.MessageInfo{MessageSource: types.MessageSource{Chat: group, Sender: sender, IsGroup: true}, ID: id, Timestamp: at}
	}
	edit := func(target, text string, at time.Time) *waE2E.Message {
		return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key: &waCommon.MessageKey{ID: proto.String(target)}, EditedMessage: textMsg(text), TimestampMS: proto.Int64(at.UnixMilli())}}
	}
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key: &waCommon.MessageKey{ID: proto.String("M1")}}}

	live(t, in, inGroup(marcoPN, "M1", t0), textMsg("originale"))
	live(t, in, inGroup(other, "X1", t0.Add(time.Minute)), textMsg("chiunque"))
	live(t, in, inGroup(other, "E1", t0.Add(2*time.Minute)), edit("M1", "falso", t0.Add(2*time.Minute)))
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'M1'`); got != "originale" {
		t.Fatalf("edit by non-author applied: %q", got)
	}
	live(t, in, inGroup(marcoPN, "E2", t0.Add(5*time.Minute)), edit("M1", "nuova", t0.Add(5*time.Minute)))
	live(t, in, inGroup(marcoPN, "E3", t0.Add(6*time.Minute)), edit("M1", "vecchia", t0.Add(3*time.Minute)))
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'M1'`); got != "nuova" {
		t.Fatalf("older edit overwrote newer: %q", got)
	}
	live(t, in, inGroup(other, "R1", t0.Add(7*time.Minute)), revoke)
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages WHERE wa_id = 'M1' AND deleted_at IS NOT NULL`); n != 0 {
		t.Fatal("revoke by a non-author non-admin applied")
	}
	admin := inGroup(other, "R2", t0.Add(8*time.Minute))
	admin.Edit = types.EditAttributeAdminRevoke
	live(t, in, admin, revoke)
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages WHERE wa_id = 'M1' AND deleted_at IS NOT NULL`); n != 1 {
		t.Fatal("admin revoke not applied")
	}
}

// Finding 9: chat state comes only from full snapshots and never overrides newer state.
func TestHistoryChatStateOnlyFromSnapshots(t *testing.T) {
	in := newIngestor(t)
	conv := func(archived bool, unread uint32) *waHistorySync.Conversation {
		return &waHistorySync.Conversation{ID: proto.String(marcoPN.String()), Archived: proto.Bool(archived), UnreadCount: proto.Uint32(unread),
			Messages: []*waHistorySync.HistorySyncMsg{webMsg("S1", false, "", t0, textMsg("x"))}}
	}
	ctx := context.Background()
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_RECENT, conv(true, 9))); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT archived || ':' || unread_count FROM chats`); got != "0:0" {
		t.Fatalf("RECENT chunk set chat state: %s", got)
	}
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, conv(true, 3))); err != nil {
		t.Fatal(err)
	}
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_FULL, conv(false, 7))); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT archived || ':' || unread_count FROM chats`); got != "1:3" {
		t.Fatalf("chat state = %s, want the first snapshot only", got)
	}
}

// Finding 11: events on unknown messages and history notifications create nothing.
func TestTargetEventsCreateNothing(t *testing.T) {
	in := newIngestor(t)
	stranger := types.NewJID("393330000000", types.DefaultUserServer)
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: stranger, Sender: stranger}, ID: "R1", Timestamp: t0},
		&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("NOPE")}, Text: proto.String("👍")}})
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: myPN, Sender: myPN, IsFromMe: true}, ID: "H1", Timestamp: t0},
		&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
			HistorySyncNotification: &waE2E.HistorySyncNotification{}}})
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: stranger, Sender: stranger}, ID: "E1", Timestamp: t0},
		&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key: &waCommon.MessageKey{ID: proto.String("NOPE")}, EditedMessage: textMsg("x")}})
	if n := one[int](t, in, `SELECT (SELECT COUNT(*) FROM chats) + (SELECT COUNT(*) FROM persons) + (SELECT COUNT(*) FROM messages)`); n != 0 {
		t.Fatalf("created %d rows for events without a stored target", n)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM raw_events`); n != 3 {
		t.Fatalf("raw events = %d, every event is still archived", n)
	}
}

// Low: decrypted poll votes become poll_vote rows naming the options.
func TestPollVote(t *testing.T) {
	in := newIngestor(t)
	live(t, in, dmFromMarco("POLL", t0), &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
		Name: proto.String("Pizza?"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Sì")}, {OptionName: proto.String("No")}},
	}})
	h := sha256.Sum256([]byte("No"))
	vote := &waE2E.PollVoteMessage{SelectedOptions: [][]byte{h[:]}}
	evt := &events.Message{Info: dmFromMarco("V1", t0.Add(time.Minute)), Message: &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
		PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("POLL")}}}}
	if _, err := in.LiveMessage(context.Background(), evt, vote); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT kind || ':' || text || ':' || reply_to_wa_id FROM messages WHERE wa_id = 'V1'`); got != "poll_vote:voted: No:POLL" {
		t.Fatalf("vote = %q", got)
	}
	if got := one[int64](t, in, `SELECT last_message_at FROM chats`); got != t0.Unix() {
		t.Fatal("a vote must not move chat recency")
	}
}

// Low: replies to buttons and lists are stored as the selected label.
func TestButtonResponseExtracted(t *testing.T) {
	c, ok := extract(&waE2E.Message{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{
		Response: &waE2E.ButtonsResponseMessage_SelectedDisplayText{SelectedDisplayText: "Confermo"}}})
	if !ok || c.Text != "[selected] Confermo" {
		t.Fatalf("got %+v %v", c, ok)
	}
}

// Low: a number change is noted, not merged.
func TestNumberChangeIsNotedNotMerged(t *testing.T) {
	in := newIngestor(t)
	newNum := types.NewJID("393335555555", types.DefaultUserServer)
	hs := historyEvent(waHistorySync.HistorySync_RECENT, &waHistorySync.Conversation{
		ID: proto.String(marcoPN.String()), NewJID: proto.String(newNum.String()),
		Messages: []*waHistorySync.HistorySyncMsg{webMsg("N1", false, "", t0, textMsg("x"))},
	})
	if err := in.HistorySync(context.Background(), hs); err != nil {
		t.Fatal(err)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM jids WHERE jid = ?`, newNum.String()); n != 0 {
		t.Fatal("new number was linked to the person")
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM identity_notes WHERE other_jid = ?`, newNum.String()); n != 1 {
		t.Fatal("number change not noted")
	}
}

// Low: live group changes become system messages and refresh name and description.
func TestLiveGroupInfo(t *testing.T) {
	in := newIngestor(t)
	topic := "Partite del giovedì"
	ev := &events.GroupInfo{JID: group, Sender: &marcoPN, Timestamp: t0,
		Name: &types.GroupName{Name: "Calcetto 2"}, Topic: &types.GroupTopic{Topic: topic}, Join: []types.JID{myPN}}
	for range 2 { // replays are idempotent
		if err := in.LiveGroupInfo(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if got := one[string](t, in, `SELECT name || '|' || description FROM chats`); got != "Calcetto 2|"+topic {
		t.Fatalf("chat = %q", got)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages WHERE kind = 'system'`); n != 3 {
		t.Fatalf("system messages = %d, want 3 (name, description, added)", n)
	}
	// WhatsApp does not count group changes as unread, nor reset the count
	// for one of mine.
	if n := one[int](t, in, `SELECT unread_count FROM chats`); n != 0 {
		t.Fatalf("unread after group changes = %d, want 0", n)
	}
	if _, err := in.DB.Exec(`UPDATE chats SET unread_count = 2`); err != nil {
		t.Fatal(err)
	}
	mine := &events.GroupInfo{JID: group, Sender: &myPN, Timestamp: t0.Add(time.Minute), Name: &types.GroupName{Name: "Calcetto 3"}}
	if err := in.LiveGroupInfo(context.Background(), mine); err != nil {
		t.Fatal(err)
	}
	if n := one[int](t, in, `SELECT unread_count FROM chats`); n != 2 {
		t.Fatalf("unread after my group change = %d, want 2", n)
	}
}
