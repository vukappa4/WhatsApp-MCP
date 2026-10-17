package wa

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

var (
	myPN    = types.NewJID("393700000001", types.DefaultUserServer)
	myLID   = types.NewJID("900000000000001", types.HiddenUserServer)
	marcoPN = types.NewJID("393331234567", types.DefaultUserServer)
	marcoL  = types.NewJID("111111111111111", types.HiddenUserServer)
	group   = types.NewJID("120363000000000001", types.GroupServer)
	t0      = time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
)

type fakeLIDs map[types.JID]types.JID

func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return f[lid], nil
}

func newIngestor(t *testing.T) *Ingestor {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Ingestor{DB: db, LIDs: fakeLIDs{}, Self: func() (types.JID, types.JID) { return myPN, myLID }}
}

func one[T any](t *testing.T, in *Ingestor, q string, args ...any) T {
	t.Helper()
	var v T
	if err := in.DB.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func textMsg(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func live(t *testing.T, in *Ingestor, info types.MessageInfo, m *waE2E.Message) {
	t.Helper()
	if err := in.Message(context.Background(), &events.Message{Info: info, Message: m}); err != nil {
		t.Fatal(err)
	}
}

func dmFromMarco(id string, at time.Time) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{Chat: marcoPN, Sender: marcoPN},
		ID:            id, Timestamp: at, PushName: "Marchino",
	}
}

func TestGroupMessageFromLIDSenderLinksPhone(t *testing.T) {
	in := newIngestor(t)
	live(t, in, dmFromMarco("DM1", t0), textMsg("ciao"))

	info := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat: group, IsGroup: true,
			Sender:         types.JID{User: marcoL.User, Server: types.HiddenUserServer, Device: 3},
			SenderAlt:      marcoPN,
			AddressingMode: types.AddressingModeLID,
		},
		ID: "G1", Timestamp: t0.Add(time.Minute), PushName: "Marchino",
	}
	live(t, in, info, textMsg("ciao gruppo"))

	if n := one[int](t, in, `SELECT COUNT(DISTINCT person_id) FROM jids WHERE jid IN (?, ?)`, marcoPN.String(), marcoL.String()); n != 1 {
		t.Fatalf("LID and phone map to %d persons, want 1", n)
	}
	if got := one[string](t, in, `SELECT p.phone_e164 FROM messages m JOIN persons p ON p.id = m.sender_person_id WHERE m.wa_id = 'G1'`); got != "+393331234567" {
		t.Fatalf("group sender phone = %q", got)
	}
	if got := one[string](t, in, `SELECT sender_jid FROM messages WHERE wa_id = 'G1'`); got != marcoL.String() {
		t.Fatalf("sender_jid = %q, want device-less LID", got)
	}
	if got := one[string](t, in, `SELECT display_name FROM persons p JOIN jids j ON j.person_id = p.id WHERE j.jid = ?`, marcoL.String()); got != "Marchino" {
		t.Fatalf("display name = %q, want push name", got)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM chats WHERE kind = 'group'`); n != 1 {
		t.Fatalf("group chats = %d", n)
	}
}

func TestLIDDMMergesIntoPhoneDM(t *testing.T) {
	in := newIngestor(t)
	live(t, in, dmFromMarco("P1", t0), textMsg("dal numero"))
	// Same person writes from its LID; whatsmeow gives the phone as SenderAlt.
	live(t, in, types.MessageInfo{
		MessageSource: types.MessageSource{Chat: marcoL, Sender: marcoL, SenderAlt: marcoPN},
		ID:            "L1", Timestamp: t0.Add(time.Hour),
	}, textMsg("dal lid"))
	if n := one[int](t, in, `SELECT COUNT(*) FROM chats WHERE kind = 'dm'`); n != 1 {
		t.Fatalf("dm chats = %d, want 1", n)
	}
	if n := one[int](t, in, `SELECT COUNT(DISTINCT chat_id) FROM messages`); n != 1 {
		t.Fatalf("messages spread over %d chats", n)
	}
}

func TestLIDResolvedThroughStore(t *testing.T) {
	in := newIngestor(t)
	in.LIDs = fakeLIDs{marcoL: marcoPN}
	live(t, in, dmFromMarco("P1", t0), textMsg("dal numero"))
	live(t, in, types.MessageInfo{
		MessageSource: types.MessageSource{Chat: marcoL, Sender: marcoL},
		ID:            "L1", Timestamp: t0.Add(time.Hour),
	}, textMsg("dal lid senza alt"))
	if n := one[int](t, in, `SELECT COUNT(*) FROM chats WHERE kind = 'dm'`); n != 1 {
		t.Fatalf("dm chats = %d, want 1 via Store.LIDs", n)
	}
}

func TestEditRevokeReaction(t *testing.T) {
	in := newIngestor(t)
	live(t, in, dmFromMarco("M1", t0), textMsg("prima versione"))

	editInfo := dmFromMarco("E1", t0.Add(time.Minute))
	editInfo.Edit = types.EditAttributeMessageEdit
	live(t, in, editInfo, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{RemoteJID: proto.String(marcoPN.String()), ID: proto.String("M1")},
		EditedMessage: textMsg("seconda versione"),
	}})
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'M1'`); got != "seconda versione" {
		t.Fatalf("edited text = %q", got)
	}
	if got := one[int64](t, in, `SELECT edited_at FROM messages WHERE wa_id = 'M1'`); got != t0.Add(time.Minute).Unix() {
		t.Fatalf("edited_at = %d", got)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("edit must not add a message, have %d", n)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'seconda'`); n != 1 {
		t.Fatal("FTS not updated by edit")
	}

	// Reaction from me, then removal.
	reactInfo := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: marcoPN, Sender: myPN, IsFromMe: true},
		ID:            "R1", Timestamp: t0.Add(2 * time.Minute),
	}
	reaction := func(emoji string, at time.Time) *waE2E.Message {
		return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key:               &waCommon.MessageKey{ID: proto.String("M1")},
			Text:              proto.String(emoji),
			SenderTimestampMS: proto.Int64(at.UnixMilli()),
		}}
	}
	live(t, in, reactInfo, reaction("👍", t0.Add(2*time.Minute)))
	if got := one[string](t, in, `SELECT r.emoji FROM reactions r JOIN persons p ON p.id = r.person_id WHERE p.is_me = 1`); got != "👍" {
		t.Fatalf("reaction = %q", got)
	}
	reactInfo.ID = "R2"
	live(t, in, reactInfo, reaction("", t0.Add(3*time.Minute)))
	if n := one[int](t, in, `SELECT COUNT(*) FROM reactions`); n != 0 {
		t.Fatalf("reaction not removed, have %d", n)
	}

	live(t, in, dmFromMarco("X1", t0.Add(4*time.Minute)), &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("M1")},
	}})
	if got := one[int64](t, in, `SELECT deleted_at FROM messages WHERE wa_id = 'M1'`); got != t0.Add(4*time.Minute).Unix() {
		t.Fatalf("deleted_at = %d", got)
	}
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'M1'`); got != "seconda versione" {
		t.Fatal("revoke must keep the text")
	}
	// Edits, reactions and revokes are not messages: recency stays on M1.
	if got := one[int64](t, in, `SELECT last_message_at FROM chats`); got != t0.Unix() {
		t.Fatalf("last_message_at = %d, want %d", got, t0.Unix())
	}
}

func TestImageWithCaptionAndReply(t *testing.T) {
	in := newIngestor(t)
	key, sha, enc := []byte{1, 2, 3}, []byte{4, 5}, []byte{6, 7}
	live(t, in, dmFromMarco("IMG", t0), &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption:       proto.String("guarda che tramonto"),
			Mimetype:      proto.String("image/jpeg"),
			URL:           proto.String("https://mmg.whatsapp.net/v/t62/abc.enc?x=1"),
			DirectPath:    proto.String("/v/t62/abc.enc?x=1"),
			MediaKey:      key,
			FileSHA256:    sha,
			FileEncSHA256: enc,
			FileLength:    proto.Uint64(123456),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String("ORIG"),
				Participant:   proto.String(myPN.String()),
				QuotedMessage: textMsg("dove sei?"),
				IsForwarded:   proto.Bool(true),
			},
		},
	}}})
	var (
		kind, text, replyTo, preview, mime, dpath, url string
		size                                           int64
		mk, fs, fe                                     []byte
		fwd                                            bool
	)
	err := in.DB.QueryRow(`SELECT m.kind, m.text, m.reply_to_wa_id, m.reply_preview, m.forwarded, md.mimetype, md.direct_path,
		md.url, md.size, md.media_key, md.file_sha256, md.file_enc_sha256
		FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = 'IMG'`).
		Scan(&kind, &text, &replyTo, &preview, &fwd, &mime, &dpath, &url, &size, &mk, &fs, &fe)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "image" || text != "guarda che tramonto" || replyTo != "ORIG" || preview != "dove sei?" || !fwd {
		t.Fatalf("message = %q %q %q %q fwd=%v", kind, text, replyTo, preview, fwd)
	}
	if mime != "image/jpeg" || dpath != "/v/t62/abc.enc?x=1" || url == "" || size != 123456 ||
		!bytes.Equal(mk, key) || !bytes.Equal(fs, sha) || !bytes.Equal(fe, enc) {
		t.Fatalf("media = %q %q %q %d %x %x %x", mime, dpath, url, size, mk, fs, fe)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'tramonto'`); n != 1 {
		t.Fatal("caption not indexed")
	}
}

func webMsg(id string, fromMe bool, participant string, at time.Time, m *waE2E.Message) *waHistorySync.HistorySyncMsg {
	key := &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe)}
	w := &waWeb.WebMessageInfo{Key: key, Message: m, MessageTimestamp: proto.Uint64(uint64(at.Unix()))}
	if participant != "" {
		w.Participant = proto.String(participant)
	}
	return &waHistorySync.HistorySyncMsg{Message: w}
}

func TestHistorySyncUnwrapsAndKeepsRecency(t *testing.T) {
	in := newIngestor(t)
	// A live message newer than everything in the history blob.
	live(t, in, dmFromMarco("LIVE", t0.Add(24*time.Hour)), textMsg("nuovo"))

	reacted := webMsg("H2", false, "", t0.Add(2*time.Minute), &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
		Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			FileName: proto.String("offerta.pdf"), Caption: proto.String("ecco il pdf"), Mimetype: proto.String("application/pdf"),
			DirectPath: proto.String("/d/1"), MediaKey: []byte{9},
		}},
	}})
	reacted.Message.Reactions = []*waWeb.Reaction{{
		Key:               &waCommon.MessageKey{FromMe: proto.Bool(true), ID: proto.String("RX")},
		Text:              proto.String("❤️"),
		SenderTimestampMS: proto.Int64(t0.Add(3 * time.Minute).UnixMilli()),
	}}
	hs := &events.HistorySync{Data: &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{
			{PnJID: proto.String(marcoPN.String()), LidJID: proto.String(marcoL.String())},
		},
		Pushnames: []*waHistorySync.Pushname{{ID: proto.String(marcoL.String()), Pushname: proto.String("Marco R.")}},
		Conversations: []*waHistorySync.Conversation{
			{
				// The DM arrives under the LID; it must land in the phone DM chat.
				ID:          proto.String(marcoL.String()),
				UnreadCount: proto.Uint32(2),
				Messages: []*waHistorySync.HistorySyncMsg{ // newest first, like WhatsApp sends them
					webMsg("H3", false, "", t0.Add(4*time.Minute), &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{
						Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("una volta sola"), Mimetype: proto.String("image/jpeg")}},
					}}),
					reacted,
					webMsg("H1", true, "", t0.Add(time.Minute), &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
						Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("messaggio effimero")}},
					}}),
				},
			},
			{
				ID:   proto.String(group.String()),
				Name: proto.String("Calcetto"),
				Messages: []*waHistorySync.HistorySyncMsg{
					webMsg("HG1", false, marcoL.String(), t0, &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
						Message: textMsg("partita alle 21"),
					}}),
				},
			},
		},
	}}
	if err := in.HistorySync(context.Background(), hs); err != nil {
		t.Fatal(err)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM chats WHERE kind = 'dm'`); n != 1 {
		t.Fatalf("dm chats = %d, want 1", n)
	}
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'H1'`); got != "messaggio effimero" {
		t.Fatalf("ephemeral history text = %q", got)
	}
	if got := one[bool](t, in, `SELECT from_me FROM messages WHERE wa_id = 'H1'`); !got {
		t.Fatal("H1 must be from me")
	}
	if got := one[string](t, in, `SELECT m.kind || ':' || m.text || ':' || md.filename FROM messages m JOIN media md ON md.id = m.media_id WHERE wa_id = 'H2'`); got != "document:ecco il pdf:offerta.pdf" {
		t.Fatalf("documentWithCaption = %q", got)
	}
	if got := one[string](t, in, `SELECT kind || ':' || text FROM messages WHERE wa_id = 'H3'`); got != "image:una volta sola" {
		t.Fatalf("view-once = %q", got)
	}
	if got := one[string](t, in, `SELECT emoji FROM reactions r JOIN messages m ON m.id = r.message_id WHERE m.wa_id = 'H2'`); got != "❤️" {
		t.Fatalf("history reaction = %q", got)
	}
	if got := one[string](t, in, `SELECT name FROM chats WHERE kind = 'group'`); got != "Calcetto" {
		t.Fatalf("group name = %q", got)
	}
	if got := one[string](t, in, `SELECT p.phone_e164 FROM messages m JOIN persons p ON p.id = m.sender_person_id WHERE m.wa_id = 'HG1'`); got != "+393331234567" {
		t.Fatalf("group LID sender phone = %q", got)
	}
	if got := one[int64](t, in, `SELECT last_message_at FROM chats WHERE kind = 'dm'`); got != t0.Add(24*time.Hour).Unix() {
		t.Fatalf("last_message_at moved backwards to %d", got)
	}
	if got := one[int](t, in, `SELECT unread_count FROM chats WHERE kind = 'dm'`); got != 2 {
		t.Fatalf("unread = %d", got)
	}

	// Ingesting the same blob again changes nothing.
	before := one[int](t, in, `SELECT COUNT(*) FROM messages`)
	if err := in.HistorySync(context.Background(), hs); err != nil {
		t.Fatal(err)
	}
	if after := one[int](t, in, `SELECT COUNT(*) FROM messages`); after != before {
		t.Fatalf("re-ingest added messages: %d -> %d", before, after)
	}
}

func TestHistoryEditOfMissingOriginalKeepsCurrentText(t *testing.T) {
	in := newIngestor(t)
	hs := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID: proto.String(marcoPN.String()),
		Messages: []*waHistorySync.HistorySyncMsg{
			webMsg("EDIT", false, "", t0.Add(time.Minute), &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
					Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
					Key:           &waCommon.MessageKey{ID: proto.String("ORIG")},
					EditedMessage: textMsg("testo corretto"),
				}},
			}}),
		},
	}}}}
	if err := in.HistorySync(context.Background(), hs); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'ORIG' AND edited_at IS NOT NULL`); got != "testo corretto" {
		t.Fatalf("text = %q", got)
	}
}
