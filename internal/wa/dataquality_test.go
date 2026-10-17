package wa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/logx"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// peerLog records the messages a test client sends to the phone.
type peerLog struct {
	mu   sync.Mutex
	msgs []*waE2E.Message
}

func (p *peerLog) send(_ context.Context, m *waE2E.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, m)
	return nil
}

func (p *peerLog) sent() []*waE2E.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*waE2E.Message(nil), p.msgs...)
}

// newTestClient builds an unconnected client on fresh data and session DBs.
// Its event handler runs exactly as in the daemon; sends to the phone are recorded.
func newTestClient(t *testing.T, dir string) (*Client, *peerLog) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(dir, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(ctx, filepath.Join(dir, "session.db"), db, logx.New(io.Discard, false))
	if err != nil {
		t.Fatal(err)
	}
	c.ing.Self = func() (types.JID, types.JID) { return myPN, myLID }
	peers := &peerLog{}
	c.sendPeer = peers.send
	t.Cleanup(func() { c.Stop(); db.Close() })
	return c, peers
}

func clientOne[T any](t *testing.T, c *Client, q string, args ...any) T {
	t.Helper()
	var v T
	if err := c.db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// Finding 1: full syncs and recoveries emit their mutations as events.
func TestClientEmitsAppStateEventsOnFullSync(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	cli := whatsmeow.NewClient(c.device, nil)
	configure(cli)
	if !cli.EmitAppStateEventsOnFullSync {
		t.Fatal("EmitAppStateEventsOnFullSync must be on: archive, pin and read state come from full syncs")
	}
}

// Finding 1: a broken full snapshot asks the phone for a recovery copy, once
// per backoff period, persisted across restarts; never the fatal exception.
func TestAppStateRecoveryRequestedWithBackoff(t *testing.T) {
	dir := t.TempDir()
	c, peers := newTestClient(t, dir)
	broken := &events.AppStateSyncError{Name: appstate.WAPatchRegularLow, FullSync: true,
		Error: fmt.Errorf("failed to verify patch v0: %w", appstate.ErrMismatchingLTHash)}
	c.handle(broken)
	c.wg.Wait()
	c.handle(broken) // within the backoff: no second request
	c.wg.Wait()
	c.handle(&events.AppStateSyncError{Name: appstate.WAPatchRegularHigh, FullSync: true, Error: errors.New("timeout")})
	c.wg.Wait()

	sent := peers.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages to the phone, want 1", len(sent))
	}
	pm := sent[0].GetProtocolMessage()
	if pm.GetAppStateFatalExceptionNotification() != nil || pm.GetType() == waE2E.ProtocolMessage_APP_STATE_FATAL_EXCEPTION_NOTIFICATION {
		t.Fatal("sent the fatal exception notification: it logs out every linked device")
	}
	req := pm.GetPeerDataOperationRequestMessage()
	if req.GetPeerDataOperationRequestType() != waE2E.PeerDataOperationRequestType_COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY ||
		req.GetSyncdCollectionFatalRecoveryRequest().GetCollectionName() != "regular_low" {
		t.Fatalf("request = %v", req)
	}
	if n := clientOne[int](t, c, `SELECT COUNT(*) FROM connection_log WHERE event = ?`, store.EvAppStateAsk); n != 1 {
		t.Fatalf("recovery requests logged = %d", n)
	}

	// After the backoff the next mismatch asks again.
	if sent, err := c.requestRecovery(context.Background(), appstate.WAPatchRegularLow, time.Now().Add(recoveryBackoff(1)+time.Minute)); err != nil || !sent {
		t.Fatalf("after backoff: sent=%v err=%v", sent, err)
	}
	st, err := loadRecovery(context.Background(), c.db, appstate.WAPatchRegularLow)
	if err != nil || st.Attempts != 2 {
		t.Fatalf("persisted attempts = %d (%v), want 2", st.Attempts, err)
	}

	// The phone answered: recovery recorded, backoff kept (the server copy is still broken).
	c.handle(&events.AppStateSyncComplete{Name: appstate.WAPatchRegularLow, Version: 42, Recovery: true})
	c.wg.Wait()
	st, _ = loadRecovery(context.Background(), c.db, appstate.WAPatchRegularLow)
	if !st.Recovered || st.Version != 42 || st.Attempts != 2 {
		t.Fatalf("after recovery: %+v", st)
	}
	if n := clientOne[int](t, c, `SELECT COUNT(*) FROM connection_log WHERE event = ?`, store.EvAppStateRecov); n != 1 {
		t.Fatal("recovery not logged")
	}
	// A server full sync that succeeds proves the collection healthy again.
	c.handle(&events.AppStateSyncComplete{Name: appstate.WAPatchRegularLow, Version: 43})
	c.wg.Wait()
	st, _ = loadRecovery(context.Background(), c.db, appstate.WAPatchRegularLow)
	if st.Attempts != 0 || st.Recovered {
		t.Fatalf("after healthy full sync: %+v", st)
	}
}

// Finding 1: archive, pin, mute, mark read and own read receipts apply to
// chats in the order of their action times, never raise unread counts by
// replay, never create chats, and survive a reingest.
func TestChatStateEvents(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	ctx := context.Background()
	for i, id := range []string{"A", "B", "C"} {
		if _, err := c.ing.LiveMessage(ctx, &events.Message{Info: dmFromMarco(id, t0.Add(time.Duration(i)*time.Minute)), Message: textMsg(id)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		return clientOne[string](t, c, `SELECT archived || ':' || pinned || ':' || COALESCE(muted_until, 0) || ':' || unread_count FROM chats`)
	}
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	read := func(read bool, when time.Time, upTo time.Time, full bool) *events.MarkChatAsRead {
		return &events.MarkChatAsRead{JID: marcoPN, Timestamp: when, FromFullSync: full, Action: &waSyncAction.MarkChatAsReadAction{
			Read: proto.Bool(read), MessageRange: &waSyncAction.SyncActionMessageRange{LastMessageTimestamp: proto.Int64(upTo.Unix())}}}
	}
	mutedUntil := at(48 * 60)
	steps := []any{
		&events.Archive{JID: marcoPN, Timestamp: at(10), FromFullSync: true, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}},
		&events.Archive{JID: marcoPN, Timestamp: at(5), Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(false)}}, // older: ignored
		&events.Pin{JID: marcoL, Timestamp: at(3), Action: &waSyncAction.PinAction{Pinned: proto.Bool(true)}},                 // unknown LID: no chat
		&events.Pin{JID: marcoPN, Timestamp: at(3), Action: &waSyncAction.PinAction{Pinned: proto.Bool(true)}},
		&events.Mute{JID: marcoPN, Timestamp: at(3), Action: &waSyncAction.MuteAction{Muted: proto.Bool(true), MuteEndTimestamp: proto.Int64(mutedUntil.UnixMilli())}},
		read(true, at(3), at(1), false), // read up to B: C stays unread
		read(true, at(4), at(0), true),  // full-sync replay of an older read: never raises the count
		&events.Receipt{MessageSource: types.MessageSource{Chat: marcoPN, Sender: marcoPN}, MessageIDs: []string{"X"}, Timestamp: at(4), Type: types.ReceiptTypeRead},
		&events.Archive{JID: types.NewJID("393330000000", types.DefaultUserServer), Timestamp: at(3), Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}},
	}
	for _, s := range steps {
		c.handle(s)
	}
	want := fmt.Sprintf("1:1:%d:1", mutedUntil.Unix())
	if got := state(); got != want {
		t.Fatalf("state = %s, want %s", got, want)
	}
	if n := clientOne[int](t, c, `SELECT COUNT(*) FROM chats`); n != 1 {
		t.Fatalf("chats = %d: chat state created a chat", n)
	}
	// The user reads C on the phone: its own read receipt clears the count.
	c.handle(&events.Receipt{MessageSource: types.MessageSource{Chat: marcoPN, Sender: myPN, IsFromMe: true},
		MessageIDs: []string{"C"}, Timestamp: at(6), Type: types.ReceiptTypeReadSelf})
	if got := clientOne[int](t, c, `SELECT unread_count FROM chats`); got != 0 {
		t.Fatalf("unread after own read receipt = %d", got)
	}
	c.handle(read(false, at(7), at(2), false)) // marked unread on the phone
	if got := clientOne[int](t, c, `SELECT unread_count FROM chats`); got != 1 {
		t.Fatalf("unread after mark unread = %d", got)
	}
	// Receipts from other people are not chat state and are not archived.
	if n := clientOne[int](t, c, `SELECT COUNT(*) FROM raw_events WHERE kind = ?`, store.RawChatState); n != len(steps)-1+2 {
		t.Fatalf("archived chat state events = %d, want %d", n, len(steps)-1+2)
	}

	before := state()
	if _, err := c.ing.Reingest(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != before {
		t.Fatalf("after reingest state = %s, want %s", got, before)
	}
}

// Finding 2: run and pair share one lifecycle: start, heartbeats, one clean stop.
func TestLifecycleRecordsSession(t *testing.T) {
	in := newIngestor(t)
	defer func(d time.Duration) { heartbeatEvery = d }(heartbeatEvery)
	heartbeatEvery = 10 * time.Millisecond
	ctx := context.Background()
	if err := store.Heartbeat(ctx, in.DB, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	life, err := BeginLifecycle(ctx, in.DB, logx.New(io.Discard, false), "pair", "test")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	life.End(nil, "pairing done")
	life.End(nil, "again")
	if got := one[string](t, in, `SELECT detail FROM connection_log WHERE event = ?`, store.EvDaemonStart); !strings.HasPrefix(got, "pair version test pid ") {
		t.Fatalf("start detail = %q", got)
	}
	if got := one[string](t, in, `SELECT group_concat(detail) FROM connection_log WHERE event = ?`, store.EvDaemonStop); got != "clean stop (pairing done)" {
		t.Fatalf("stops = %q, want exactly one clean stop", got)
	}
	alive, err := store.GetStateTS(ctx, in.DB, store.KeyAliveAt)
	if err != nil || time.Since(alive) > time.Minute {
		t.Fatalf("heartbeat not recorded: %v %v", alive, err)
	}
}

func stubMsg(id string, at time.Time, participant string, st waWeb.WebMessageInfo_StubType) *waHistorySync.HistorySyncMsg {
	w := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(false)},
		MessageTimestamp: proto.Uint64(uint64(at.Unix())), MessageStubType: st.Enum()}
	if participant != "" {
		w.Participant = proto.String(participant)
	}
	return &waHistorySync.HistorySyncMsg{Message: w}
}

// Findings 3 and 6: a history conversation holding only stubs links the
// identities but creates no DM chat; undecryptable stubs and placeholders
// are counted in the blob summary; the chat appears with its first message
// and gets the snapshot state then.
func TestStubOnlyConversationCreatesNoChat(t *testing.T) {
	in := newIngestor(t)
	var logs bytes.Buffer
	in.Log = logx.New(&logs, true)
	ctx := context.Background()
	conv := &waHistorySync.Conversation{
		ID: proto.String(marcoPN.String()), LidJID: proto.String(marcoL.String()), Archived: proto.Bool(true),
		Messages: []*waHistorySync.HistorySyncMsg{
			stubMsg("S3", t0.Add(2*time.Minute), "", waWeb.WebMessageInfo_CIPHERTEXT),
			webMsg("S2", false, "", t0.Add(time.Minute), &waE2E.Message{PlaceholderMessage: &waE2E.PlaceholderMessage{}}),
			stubMsg("S1", t0, "", waWeb.WebMessageInfo_E2E_ENCRYPTED),
		},
	}
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, conv)); err != nil {
		t.Fatal(err)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM chats`); n != 0 {
		t.Fatalf("chats = %d, want none for a conversation without stored messages", n)
	}
	if n := one[int](t, in, `SELECT COUNT(DISTINCT person_id) FROM jids WHERE jid IN (?, ?)`, marcoPN.String(), marcoL.String()); n != 1 {
		t.Fatalf("identity link lost: %d persons", n)
	}
	for _, want := range []string{"dropped_ciphertext=1", "dropped_placeholder=1", "dropped_stub=1", "reason=ciphertext"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, logs.String())
		}
	}
	conv.Messages = append([]*waHistorySync.HistorySyncMsg{webMsg("M1", false, "", t0.Add(3*time.Minute), textMsg("ciao"))}, conv.Messages...)
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_FULL, conv)); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT kind || ':' || archived FROM chats`); got != "dm:1" {
		t.Fatalf("chat = %s, want the DM with the snapshot state", got)
	}
}

// Finding 4: album items (associatedChildMessage) are stored, live and from
// history, with the album link; HD copies are not stored twice; reingest
// rebuilds them from the raw archive.
func TestAlbumItemsStored(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	item := func(caption string, kind waE2E.MessageAssociation_AssociationType) *waE2E.Message {
		return &waE2E.Message{AssociatedChildMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{Caption: proto.String(caption), Mimetype: proto.String("image/jpeg"),
				DirectPath: proto.String("/d/" + caption), MediaKey: []byte{1}},
			MessageContextInfo: &waE2E.MessageContextInfo{MessageAssociation: &waE2E.MessageAssociation{
				AssociationType: kind.Enum(), ParentMessageKey: &waCommon.MessageKey{ID: proto.String("ALB")}}},
		}}}
	}
	live(t, in, dmFromMarco("ALB", t0), &waE2E.Message{AlbumMessage: &waE2E.AlbumMessage{ExpectedImageCount: proto.Uint32(2)}})
	live(t, in, dmFromMarco("IMG1", t0.Add(time.Second)), item("uno", waE2E.MessageAssociation_MEDIA_ALBUM))
	live(t, in, dmFromMarco("HD1", t0.Add(2*time.Second)), item("uno-hd", waE2E.MessageAssociation_HD_IMAGE_DUAL_UPLOAD))
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_RECENT, &waHistorySync.Conversation{
		ID:       proto.String(marcoPN.String()),
		Messages: []*waHistorySync.HistorySyncMsg{webMsg("IMG2", false, "", t0.Add(3*time.Second), item("due", waE2E.MessageAssociation_MEDIA_ALBUM))},
	})); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		got := one[string](t, in, `SELECT group_concat(m.wa_id || ':' || m.kind || ':' || m.text || ':' || COALESCE(m.album_wa_id, '') || ':' || md.direct_path, ' ')
			FROM (SELECT * FROM messages ORDER BY ts) m JOIN media md ON md.id = m.media_id`)
		if got != "IMG1:image:uno:ALB:/d/uno IMG2:image:due:ALB:/d/due" {
			t.Fatalf("album items = %q", got)
		}
	}
	check()
	if _, err := in.Reingest(ctx, nil); err != nil {
		t.Fatal(err)
	}
	check()
}

// Finding 5: a history DM title is a weak name; it never beats a real one.
func TestHistoryDMNameIsWeak(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	stranger := types.NewJID("393339999999", types.DefaultUserServer)
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_RECENT,
		&waHistorySync.Conversation{ID: proto.String(marcoPN.String()), Name: proto.String("Marco Rossi"),
			Messages: []*waHistorySync.HistorySyncMsg{webMsg("M1", false, "", t0, textMsg("ciao"))}},
		&waHistorySync.Conversation{ID: proto.String(stranger.String()), DisplayName: proto.String("Luca"),
			Messages: []*waHistorySync.HistorySyncMsg{stubMsg("S1", t0, "", waWeb.WebMessageInfo_E2E_ENCRYPTED)}},
		&waHistorySync.Conversation{ID: proto.String(myPN.String()), Name: proto.String("393700000001")},
	)); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT display_name || '/' || name_source FROM persons p JOIN jids j ON j.person_id = p.id WHERE j.jid = ?`, marcoPN.String()); got != "Marco Rossi/push_name" {
		t.Fatalf("marco = %q", got)
	}
	if got := one[string](t, in, `SELECT display_name FROM persons p JOIN jids j ON j.person_id = p.id WHERE j.jid = ?`, stranger.String()); got != "Luca" {
		t.Fatalf("display name of a chat without messages = %q", got)
	}
	// A real profile name replaces the weak one; a later weak one does not come back.
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoPN, Sender: marcoPN}, ID: "M2", Timestamp: t0.Add(time.Hour), PushName: "Marchino"}, textMsg("x"))
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_RECENT,
		&waHistorySync.Conversation{ID: proto.String(marcoPN.String()), Name: proto.String("Marco Rossi")})); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT display_name FROM persons p JOIN jids j ON j.person_id = p.id WHERE j.jid = ?`, marcoPN.String()); got != "Marchino" {
		t.Fatalf("marco after push name = %q", got)
	}
}

// fakeGroups serves joined groups, group info and newsletters from maps.
type fakeGroups struct {
	joined      []*types.GroupInfo
	info        map[types.JID]*types.GroupInfo
	errs        map[types.JID]error
	newsletters []*types.NewsletterMetadata
	calls       []types.JID
}

func (f *fakeGroups) GetJoinedGroups(context.Context) ([]*types.GroupInfo, error) {
	return f.joined, nil
}
func (f *fakeGroups) GetSubscribedNewsletters(context.Context) ([]*types.NewsletterMetadata, error) {
	return f.newsletters, nil
}
func (f *fakeGroups) GetGroupInfo(_ context.Context, j types.JID) (*types.GroupInfo, error) {
	f.calls = append(f.calls, j)
	if err := f.errs[j]; err != nil {
		return nil, err
	}
	return f.info[j], nil
}

// Finding 5: unnamed groups are looked up with failure tolerance and backoff,
// newsletter names come from the subscription list, and WhatsApp's own
// account is named.
func TestNameRefresh(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	left := types.NewJID("120363000000000002", types.GroupServer)
	old := types.NewJID("120363000000000003", types.GroupServer)
	limited := types.NewJID("120363000000000004", types.GroupServer)
	nl := types.NewJID("120363000000000009", types.NewsletterServer)
	unseen := types.NewJID("120363000000000010", types.NewsletterServer)
	for i, j := range []types.JID{left, old, limited} {
		live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: j, IsGroup: true, Sender: marcoPN}, ID: fmt.Sprint("G", i), Timestamp: t0.Add(time.Duration(3-i) * time.Minute)}, textMsg("x"))
	}
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: nl, Sender: nl}, ID: "N1", Timestamp: t0}, textMsg("news"))
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: types.NewJID("0", types.DefaultUserServer), Sender: types.NewJID("0", types.DefaultUserServer)}, ID: "W1", Timestamp: t0}, textMsg("codice di sicurezza"))

	fake := &fakeGroups{
		joined: []*types.GroupInfo{{JID: group, GroupName: types.GroupName{Name: "Calcetto"}}},
		info:   map[types.JID]*types.GroupInfo{old: {JID: old, GroupName: types.GroupName{Name: "Gita 2019"}, GroupTopic: types.GroupTopic{Topic: "foto"}}},
		errs:   map[types.JID]error{left: fmt.Errorf("x: %w", whatsmeow.ErrNotInGroup), limited: fmt.Errorf("x: %w", whatsmeow.ErrIQRateOverLimit)},
		newsletters: []*types.NewsletterMetadata{
			{ID: nl, ThreadMeta: types.NewsletterThreadMetadata{Name: types.NewsletterText{Text: "Il Post"}}},
			{ID: unseen, ThreadMeta: types.NewsletterThreadMetadata{Name: types.NewsletterText{Text: "Unseen"}}},
		},
	}
	now := t0.Add(time.Hour)
	r := nameRefresh{db: in.DB, api: fake, now: func() time.Time { return now }, log: func(string, ...any) {}}
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	name := func(j types.JID) string {
		return one[string](t, in, `SELECT name FROM chats WHERE jid = ?`, j.String())
	}
	if name(group) != "Calcetto" || name(old) != "Gita 2019" || name(nl) != "Il Post" {
		t.Fatalf("names: joined=%q old=%q newsletter=%q", name(group), name(old), name(nl))
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM chats WHERE jid = ?`, unseen.String()); n != 0 {
		t.Fatal("a newsletter without messages got a chat")
	}
	if got := one[string](t, in, `SELECT info_attempts || ':' || info_error FROM chats WHERE jid = ?`, left.String()); got != "1:not in group (left or removed)" {
		t.Fatalf("left group = %q", got)
	}
	if got := one[string](t, in, `SELECT display_name || '/' || name_source FROM persons WHERE phone_e164 = '+0'`); got != "WhatsApp/system" {
		t.Fatalf("system account = %q", got)
	}
	// Order is most recent first: left, old, then limited stops the run.
	if len(fake.calls) != 3 || fake.calls[2] != limited {
		t.Fatalf("lookups = %v", fake.calls)
	}

	// Within the backoff the left group is not asked again; after it, it is.
	fake.calls = nil
	fake.errs[limited] = nil
	fake.info[limited] = &types.GroupInfo{JID: limited, GroupName: types.GroupName{Name: "Condominio"}}
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != limited || name(limited) != "Condominio" {
		t.Fatalf("second run lookups = %v, name %q", fake.calls, name(limited))
	}
	fake.calls = nil
	now = now.Add(infoBackoff(1) + time.Minute)
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != left {
		t.Fatalf("after backoff lookups = %v", fake.calls)
	}
	if got := one[int](t, in, `SELECT info_attempts FROM chats WHERE jid = ?`, left.String()); got != 2 {
		t.Fatalf("left group attempts = %d", got)
	}
}

// Finding 6: an admin revoke stub in history marks its target deleted.
func TestHistoryAdminRevokeStub(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: group, IsGroup: true, Sender: marcoPN}, ID: "M1", Timestamp: t0}, textMsg("spam"))
	if err := in.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_RECENT, &waHistorySync.Conversation{
		ID:       proto.String(group.String()),
		Messages: []*waHistorySync.HistorySyncMsg{stubMsg("M1", t0.Add(time.Minute), marcoPN.String(), waWeb.WebMessageInfo_ADMIN_REVOKE)},
	})); err != nil {
		t.Fatal(err)
	}
	if got := one[string](t, in, `SELECT text || ':' || (deleted_at IS NOT NULL) FROM messages WHERE wa_id = 'M1'`); got != "spam:1" {
		t.Fatalf("admin-revoked message = %q, want the original text kept and marked deleted", got)
	}
}

// Finding 1: an app-state action on a chat does not stop the first history
// snapshot from setting the flags no action set, nor the phone's unread count.
func TestSnapshotAfterAppStateAction(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	ctx := context.Background()
	if _, err := c.ing.LiveMessage(ctx, &events.Message{Info: dmFromMarco("A", t0), Message: textMsg("a")}, nil); err != nil {
		t.Fatal(err)
	}
	c.handle(&events.Archive{JID: marcoPN, Timestamp: t0.Add(time.Minute), FromFullSync: true, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}})
	if err := c.ing.HistorySync(ctx, historyEvent(waHistorySync.HistorySync_INITIAL_BOOTSTRAP, &waHistorySync.Conversation{
		ID: proto.String(marcoPN.String()), Archived: proto.Bool(false), Pinned: proto.Uint32(uint32(t0.Unix())), UnreadCount: proto.Uint32(5),
	})); err != nil {
		t.Fatal(err)
	}
	if got := clientOne[string](t, c, `SELECT archived || ':' || pinned || ':' || unread_count FROM chats`); got != "1:1:5" {
		t.Fatalf("state = %s, want the action's archive, the snapshot's pin and unread", got)
	}
}
