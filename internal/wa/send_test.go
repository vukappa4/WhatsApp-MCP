package wa

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// fakeMessenger stands in for the whatsmeow client: it never connects.
type fakeMessenger struct{ connected bool }

func (f fakeMessenger) IsConnected() bool { return f.connected }
func (f fakeMessenger) IsLoggedIn() bool  { return true }
func (f fakeMessenger) GenerateMessageID() types.MessageID {
	return "3EB0FAKE"
}
func (f fakeMessenger) SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return whatsmeow.SendResponse{}, errors.New("fake: never sends")
}
func (f fakeMessenger) IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
	return nil, nil
}
func (f fakeMessenger) Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return whatsmeow.UploadResponse{}, nil
}

// The history request goes to the phone as an ON_DEMAND peer request
// anchored at the given message, and only while connected.
func TestRequestHistory(t *testing.T) {
	c, peers := newTestClient(t, t.TempDir())
	anchor := types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoPN, IsFromMe: true}, ID: "OLDEST", Timestamp: t0}
	ctx := context.Background()
	if err := c.RequestHistory(ctx, anchor, 50); !errors.Is(err, ErrNotSendable) {
		t.Fatalf("unpaired: %v", err)
	}
	c.mu.Lock()
	c.st.AccountPN = myPN
	c.msgr = fakeMessenger{connected: false}
	c.mu.Unlock()
	if err := c.RequestHistory(ctx, anchor, 50); !errors.Is(err, ErrNotSendable) {
		t.Fatalf("disconnected: %v", err)
	}
	c.mu.Lock()
	c.msgr = fakeMessenger{connected: true}
	c.mu.Unlock()
	c.setState(StateConnected, "")
	if err := c.RequestHistory(ctx, anchor, 50); err != nil {
		t.Fatal(err)
	}
	sent := peers.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d peer messages", len(sent))
	}
	req := sent[0].GetProtocolMessage().GetPeerDataOperationRequestMessage()
	od := req.GetHistorySyncOnDemandRequest()
	if req.GetPeerDataOperationRequestType() != waE2E.PeerDataOperationRequestType_HISTORY_SYNC_ON_DEMAND ||
		od.GetChatJID() != marcoPN.String() || od.GetOldestMsgID() != "OLDEST" || !od.GetOldestMsgFromMe() ||
		od.GetOnDemandMsgCount() != 50 || od.GetOldestMsgTimestampMS() != t0.Unix() {
		t.Fatalf("request = %v", req)
	}
}

// The phone answers with a history sync notification of type ON_DEMAND. It
// is archived like any other notification, and its blob ingests the older
// messages without touching chat state.
func TestOnDemandHistoryIngested(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	notif := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: &waE2E.HistorySyncNotification{
			SyncType: waE2E.HistorySyncType_ON_DEMAND.Enum(), DirectPath: proto.String("/v/t62/x"),
		},
	}}
	ok := c.handle(&events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: myPN, Sender: myPN, IsFromMe: true}, ID: "HSN1", Timestamp: t0,
	}, Message: notif})
	if !ok {
		t.Fatal("notification not acked")
	}
	if n := clientOne[int](t, c, `SELECT COUNT(*) FROM raw_events WHERE kind = ? AND dedupe_key = 'hsn:HSN1'`, store.RawHistoryNotify); n != 1 {
		t.Fatalf("archived notifications = %d", n)
	}

	in := c.Ingestor()
	live(t, in, dmFromMarco("NEW", t0), textMsg("recente"))
	conv := &waHistorySync.Conversation{ID: proto.String(marcoPN.String()), UnreadCount: proto.Uint32(9), Archived: proto.Bool(true),
		Messages: []*waHistorySync.HistorySyncMsg{
			webMsg("OLD1", false, "", t0.AddDate(0, -6, 0), textMsg("vecchio 1")),
			webMsg("OLD2", true, "", t0.AddDate(0, -6, 1), textMsg("vecchio 2")),
		}}
	if err := in.HistorySync(context.Background(), historyEvent(waHistorySync.HistorySync_ON_DEMAND, conv)); err != nil {
		t.Fatal(err)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages WHERE source = 'history'`); n != 2 {
		t.Fatalf("on-demand messages stored = %d", n)
	}
	if got := one[string](t, in, `SELECT archived || ':' || unread_count || ':' || last_message_at FROM chats`); got != "0:1:"+strconv.FormatInt(t0.Unix(), 10) {
		t.Fatalf("chat state changed by on-demand history: %s", got)
	}
}

// A sent message is archived as a raw live message and ingested from it:
// from me, source live, its media row pending until the media folder keeps
// the file (media.SaveSent), and a reingest from raw_events rebuilds it
// identically. A local_path in an archived payload is ignored: no row ever
// points outside the media folder.
func TestStoreSentUsesLivePath(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	live(t, in, dmFromMarco("IN1", t0), textMsg("mi mandi il pdf?"))
	doc := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Mimetype: proto.String("application/pdf"), FileName: proto.String("offerta.pdf"), Caption: proto.String("eccolo"),
		DirectPath: proto.String("/v/t62/doc"), MediaKey: []byte{1, 2, 3}, FileLength: proto.Uint64(1234),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("IN1"), Participant: proto.String(marcoPN.String()),
			QuotedMessage: textMsg("mi mandi il pdf?")},
	}}
	// The server answered with Marco's LID: the sent message still lands in his one DM.
	resp := whatsmeow.SendResponse{ID: "3EB0SENT", Timestamp: t0.Add(time.Minute), Chat: marcoL, Sender: myLID}
	id, err := in.StoreSent(ctx, marcoPN, resp, doc)
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		got := one[string](t, in, `SELECT m.from_me || '|' || m.source || '|' || m.kind || '|' || m.text || '|' || m.reply_to_wa_id || '|' ||
			md.local_path || '|' || md.state || '|' || md.filename || '|' || (SELECT COUNT(*) FROM chats)
			FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = '3EB0SENT'`)
		if want := `1|live|document|eccolo|IN1||pending|offerta.pdf|1`; got != want {
			t.Fatalf("%s: stored %s, want %s", when, got, want)
		}
	}
	check("after send")
	if one[int64](t, in, `SELECT id FROM messages WHERE wa_id = '3EB0SENT'`) != id {
		t.Fatal("returned id is not the stored row")
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM raw_events WHERE kind = 'message' AND ingested_at IS NOT NULL`); n != 2 {
		t.Fatalf("archived live messages = %d, want 2", n)
	}
	if n := one[int](t, in, `SELECT unread_count FROM chats`); n != 0 {
		t.Fatalf("unread after my reply = %d", n)
	}
	if _, err := in.Reingest(ctx, nil); err != nil {
		t.Fatal(err)
	}
	check("after reingest")

	evs, err := store.ListRaw(ctx, in.DB, store.RawFilter{Kinds: []string{store.RawMessage}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		var am map[string]any
		if err := json.Unmarshal(ev.Payload, &am); err != nil {
			t.Fatal(err)
		}
		if am["id"] != "3EB0SENT" {
			continue
		}
		am["local_path"] = `C:\Users\me\Documents\offerta.pdf`
		b, _ := json.Marshal(am)
		if err := in.DB.UpdateRawPayload(ctx, ev.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := in.Reingest(ctx, nil); err != nil {
		t.Fatal(err)
	}
	check("archived local_path")
}
