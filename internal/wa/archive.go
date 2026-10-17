package wa

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waVnameCert"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// WhatsApp protos are proto2 with required fields; archived copies must
// round-trip whatever arrived, complete or not.
var (
	marshalPartial   = proto.MarshalOptions{AllowPartial: true, Deterministic: true}
	unmarshalPartial = proto.UnmarshalOptions{AllowPartial: true, DiscardUnknown: false}
)

// archivedMessage is the raw_events payload of a live message: the message
// info and the decrypted message proto exactly as received.
type archivedMessage struct {
	Chat           string `json:"chat"`
	Sender         string `json:"sender"`
	SenderAlt      string `json:"sender_alt,omitempty"`
	RecipientAlt   string `json:"recipient_alt,omitempty"`
	IsFromMe       bool   `json:"from_me,omitempty"`
	IsGroup        bool   `json:"group,omitempty"`
	AddressingMode string `json:"addressing,omitempty"`
	ID             string `json:"id"`
	PushName       string `json:"push_name,omitempty"`
	TimestampMS    int64  `json:"ts_ms"`
	Edit           string `json:"edit,omitempty"`
	BusinessName   string `json:"business_name,omitempty"`
	Message        []byte `json:"message"`             // waE2E.Message proto (raw, still wrapped)
	PollVote       []byte `json:"poll_vote,omitempty"` // decrypted waE2E.PollVoteMessage proto
}

func jidString(j types.JID) string {
	if j.IsEmpty() {
		return ""
	}
	return j.String()
}

func parseJIDOrEmpty(s string) types.JID {
	if s == "" {
		return types.JID{}
	}
	j, err := types.ParseJID(s)
	if err != nil {
		return types.JID{}
	}
	return j
}

func encodeMessage(evt *events.Message, vote *waE2E.PollVoteMessage) (payload []byte, key string, err error) {
	raw := evt.RawMessage
	if raw == nil {
		raw = evt.Message
	}
	am := archivedMessage{
		Chat: jidString(evt.Info.Chat), Sender: jidString(evt.Info.Sender), SenderAlt: jidString(evt.Info.SenderAlt),
		RecipientAlt: jidString(evt.Info.RecipientAlt), IsFromMe: evt.Info.IsFromMe, IsGroup: evt.Info.IsGroup,
		AddressingMode: string(evt.Info.AddressingMode), ID: evt.Info.ID, PushName: evt.Info.PushName,
		TimestampMS: evt.Info.Timestamp.UnixMilli(), Edit: string(evt.Info.Edit),
	}
	if evt.Info.VerifiedName != nil {
		am.BusinessName = evt.Info.VerifiedName.Details.GetVerifiedName()
	}
	if am.Message, err = marshalPartial.Marshal(raw); err != nil {
		return nil, "", err
	}
	if vote != nil {
		if am.PollVote, err = marshalPartial.Marshal(vote); err != nil {
			return nil, "", err
		}
	}
	payload, err = json.Marshal(am)
	key = "msg:" + am.Chat + ":" + evt.Info.Sender.ToNonAD().String() + ":" + am.ID
	return payload, key, err
}

func decodeMessage(b []byte) (types.MessageInfo, *waE2E.Message, *waE2E.PollVoteMessage, error) {
	var am archivedMessage
	if err := json.Unmarshal(b, &am); err != nil {
		return types.MessageInfo{}, nil, nil, err
	}
	info := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat: parseJIDOrEmpty(am.Chat), Sender: parseJIDOrEmpty(am.Sender), SenderAlt: parseJIDOrEmpty(am.SenderAlt),
			RecipientAlt: parseJIDOrEmpty(am.RecipientAlt), IsFromMe: am.IsFromMe, IsGroup: am.IsGroup,
			AddressingMode: types.AddressingMode(am.AddressingMode),
		},
		ID: am.ID, PushName: am.PushName, Timestamp: time.UnixMilli(am.TimestampMS), Edit: types.EditAttribute(am.Edit),
	}
	if am.BusinessName != "" {
		info.VerifiedName = &types.VerifiedName{Details: &waVnameCert.VerifiedNameCertificate_Details{VerifiedName: &am.BusinessName}}
	}
	var msg waE2E.Message
	if err := unmarshalPartial.Unmarshal(am.Message, &msg); err != nil {
		return info, nil, nil, err
	}
	var vote *waE2E.PollVoteMessage
	if len(am.PollVote) > 0 {
		vote = &waE2E.PollVoteMessage{}
		if err := unmarshalPartial.Unmarshal(am.PollVote, vote); err != nil {
			return info, nil, nil, err
		}
	}
	return info, &msg, vote, nil
}

// LiveMessage archives a live message in its own committed transaction,
// then ingests it. archived is false only when the archive write failed:
// the caller must then withhold the ack so WhatsApp redelivers. An
// ingestion error is recorded on the raw event and retried at startup.
func (in *Ingestor) LiveMessage(ctx context.Context, evt *events.Message, vote *waE2E.PollVoteMessage) (archived bool, err error) {
	ev, err := in.ArchiveLive(ctx, evt)
	if err != nil {
		return false, err
	}
	if vote != nil {
		if err := in.AddPollVote(ctx, &ev, evt, vote); err != nil {
			return true, err
		}
	}
	return true, in.IngestArchived(ctx, ev)
}

// ArchiveLive stores a live message in raw_events in its own committed
// transaction, before anything decodes it further. Archiving the same
// message again returns the existing row.
func (in *Ingestor) ArchiveLive(ctx context.Context, evt *events.Message) (store.RawEvent, error) {
	payload, key, err := encodeMessage(evt, nil)
	if err != nil {
		return store.RawEvent{}, err
	}
	ev, err := in.archive(ctx, store.RawMessage, key, payload)
	if err != nil {
		return ev, fmt.Errorf("message %s: %w", evt.Info.ID, err)
	}
	return ev, nil
}

// AddPollVote adds a vote decrypted after archiving to the archived event
// and marks it for ingestion again when the stored payload lacked it.
func (in *Ingestor) AddPollVote(ctx context.Context, ev *store.RawEvent, evt *events.Message, vote *waE2E.PollVoteMessage) error {
	if _, _, old, err := decodeMessage(ev.Payload); err == nil && old != nil {
		return nil
	}
	payload, _, err := encodeMessage(evt, vote)
	if err != nil {
		return err
	}
	if err := in.DB.UpdateRawPayload(ctx, ev.ID, payload); err != nil {
		return err
	}
	ev.Payload, ev.Ingested = payload, false
	return nil
}

// IngestArchived ingests an archived event unless it already was.
func (in *Ingestor) IngestArchived(ctx context.Context, ev store.RawEvent) error {
	if ev.Ingested {
		return nil
	}
	return in.applyAndMark(ctx, ev)
}

// errPanic is an ingestion that panicked. It is recorded on the raw event
// as final: replaying it would panic again. reingest retries it.
type errPanic struct{ msg string }

func (e errPanic) Error() string { return e.msg }

// safely runs fn, turning a panic into an errPanic with the stack logged:
// one bad event never takes the daemon down or blocks the events after it.
func (in *Ingestor) safely(ev store.RawEvent, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			in.log().Error("ingestion panicked", "raw_event", ev.ID, "kind", ev.Kind, "panic", r, "stack", string(debug.Stack()))
			err = errPanic{fmt.Sprintf("panic while ingesting: %v", r)}
		}
	}()
	return fn()
}

// GroupEvent is the archived form of a live group change.
type GroupEvent struct {
	JID         string   `json:"jid"`
	Sender      string   `json:"sender,omitempty"`
	SenderPN    string   `json:"sender_pn,omitempty"`
	TimestampMS int64    `json:"ts_ms"`
	Name        string   `json:"name,omitempty"`
	Topic       *string  `json:"topic,omitempty"`
	Join        []string `json:"join,omitempty"`
	Leave       []string `json:"leave,omitempty"`
	Promote     []string `json:"promote,omitempty"`
	Demote      []string `json:"demote,omitempty"`
}

func jidStrings(js []types.JID) []string {
	var out []string
	for _, j := range js {
		out = append(out, j.String())
	}
	return out
}

// LiveGroupInfo archives and ingests a live group change.
func (in *Ingestor) LiveGroupInfo(ctx context.Context, v *events.GroupInfo) error {
	g := GroupEvent{JID: v.JID.String(), TimestampMS: v.Timestamp.UnixMilli(),
		Join: jidStrings(v.Join), Leave: jidStrings(v.Leave), Promote: jidStrings(v.Promote), Demote: jidStrings(v.Demote)}
	if v.Sender != nil {
		g.Sender = v.Sender.String()
	}
	if v.SenderPN != nil {
		g.SenderPN = v.SenderPN.String()
	}
	if v.Name != nil {
		g.Name = v.Name.Name
	}
	if v.Topic != nil {
		t := v.Topic.Topic
		g.Topic = &t
	}
	if g.Name == "" && g.Topic == nil && len(g.Join)+len(g.Leave)+len(g.Promote)+len(g.Demote) == 0 {
		return nil
	}
	payload, err := json.Marshal(g)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	ev, err := in.archive(ctx, store.RawGroupEvent, "group:"+hex.EncodeToString(sum[:16]), payload)
	if err != nil || ev.Ingested {
		return err
	}
	return in.applyAndMark(ctx, ev)
}

// groupEvent turns a group change into system messages and refreshes the
// group's name and description.
func (in *Ingestor) groupEvent(ctx context.Context, tx *sql.Tx, g GroupEvent, key string) error {
	jid := parseJIDOrEmpty(g.JID)
	if jid.Server != types.GroupServer {
		return nil
	}
	chat, err := in.chatFor(ctx, tx, types.MessageSource{Chat: jid, IsGroup: true}, time.Time{})
	if err != nil || chat.id == 0 {
		return err
	}
	at := time.UnixMilli(g.TimestampMS)
	info := types.MessageInfo{MessageSource: types.MessageSource{Chat: jid, IsGroup: true, Sender: parseJIDOrEmpty(g.Sender),
		SenderAlt: parseJIDOrEmpty(g.SenderPN)}, Timestamp: at}
	if pn, lid := in.self(); !info.Sender.IsEmpty() && (info.Sender.ToNonAD() == pn || info.Sender.ToNonAD() == lid) {
		info.IsFromMe = true
	}
	sender, err := in.sender(ctx, tx, info, chat)
	if err != nil {
		return err
	}
	names := func(js []string) string {
		var out []string
		for _, s := range js {
			if j := parseJIDOrEmpty(s); people.IsUser(j) {
				out = append(out, in.describeJID(ctx, tx, j))
			}
		}
		return strings.Join(out, ", ")
	}
	var lines []string
	if g.Name != "" {
		if err := store.SetChatName(ctx, tx, chat.id, g.Name); err != nil {
			return err
		}
		lines = append(lines, "group name changed to "+g.Name)
	}
	if g.Topic != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE chats SET description = ? WHERE id = ?`, *g.Topic, chat.id); err != nil {
			return err
		}
		lines = append(lines, "group description changed")
	}
	for _, x := range []struct{ verb, who string }{
		{"added", names(g.Join)}, {"left or removed", names(g.Leave)}, {"made admin", names(g.Promote)}, {"no longer admin", names(g.Demote)},
	} {
		if x.who != "" {
			lines = append(lines, x.verb+": "+x.who)
		}
	}
	for i, line := range lines {
		info.ID = fmt.Sprintf("sys:%s:%d", strings.TrimPrefix(key, "group:"), i)
		if _, err := in.upsert(ctx, tx, chat, sender, info, content{Kind: "system", Text: line}, store.SourceLive, 0, true); err != nil {
			return err
		}
	}
	return nil
}

// replayKinds are the raw event kinds the ingestor can apply by itself.
var replayKinds = []string{store.RawMessage, store.RawHistory, store.RawGroupEvent, store.RawLegacySession, store.RawLegacyChat, store.RawLegacyMessage, store.RawChatState}

// contentKinds are replayKinds without chat state: Reingest applies chat
// state after everything else, so it finds the chats and messages it acts on.
var contentKinds = replayKinds[:len(replayKinds)-1]

// batchKinds are small events applied many per transaction.
var batchKinds = map[string]bool{store.RawMessage: true, store.RawGroupEvent: true, store.RawLegacyChat: true, store.RawLegacyMessage: true, store.RawChatState: true}

const maxAttempts = 5

// applyAndMark ingests one raw event in its own transaction(s) and records the outcome.
func (in *Ingestor) applyAndMark(ctx context.Context, ev store.RawEvent) error {
	var err error
	if batchKinds[ev.Kind] {
		err = store.Retry(ctx, func() error {
			return in.DB.Tx(ctx, func(tx *sql.Tx) error {
				return in.safely(ev, func() error { return in.applyTx(ctx, tx, ev) })
			})
		})
	} else {
		err = in.safely(ev, func() error { return in.applyOwn(ctx, ev) })
	}
	var skip errSkip
	var panicked errPanic
	// A blob's failures are per conversation and logged; a panic would repeat.
	final := ev.Attempts+1 >= maxAttempts || ev.Kind == store.RawHistory || errors.As(err, &skip) || errors.As(err, &panicked)
	if merr := store.Retry(ctx, func() error { return store.MarkRaw(ctx, in.DB, ev.ID, err, final) }); merr != nil {
		return errors.Join(err, merr)
	}
	if errors.As(err, &skip) {
		return nil
	}
	return err
}

// applyTx applies a small raw event inside tx.
func (in *Ingestor) applyTx(ctx context.Context, tx *sql.Tx, ev store.RawEvent) error {
	switch ev.Kind {
	case store.RawMessage:
		info, msg, vote, err := decodeMessage(ev.Payload)
		if err != nil {
			return err
		}
		_, err = in.ingest(ctx, tx, info, msg, store.SourceLive, vote)
		return err
	case store.RawGroupEvent:
		var g GroupEvent
		if err := json.Unmarshal(ev.Payload, &g); err != nil {
			return err
		}
		return in.groupEvent(ctx, tx, g, ev.Key)
	case store.RawLegacyChat:
		var c legacyChat
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return err
		}
		return in.legacyChat(ctx, tx, c)
	case store.RawLegacyMessage:
		var m legacyMessage
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return err
		}
		return in.legacyMessage(ctx, tx, m)
	case store.RawChatState:
		var cs ChatState
		if err := json.Unmarshal(ev.Payload, &cs); err != nil {
			return err
		}
		return in.chatState(ctx, tx, cs)
	}
	return fmt.Errorf("raw event kind %q cannot be applied in a batch", ev.Kind)
}

// applyOwn applies a large raw event that manages its own transactions.
func (in *Ingestor) applyOwn(ctx context.Context, ev store.RawEvent) error {
	switch ev.Kind {
	case store.RawHistory:
		var hs waHistorySync.HistorySync
		if err := unmarshalPartial.Unmarshal(ev.Payload, &hs); err != nil {
			return err
		}
		return in.historyBlob(ctx, &hs, ev.ReceivedAt)
	case store.RawLegacySession:
		var s SessionIdentities
		if err := json.Unmarshal(ev.Payload, &s); err != nil {
			return err
		}
		return in.ApplySession(ctx, &s, ev.ReceivedAt)
	}
	return fmt.Errorf("unknown raw event kind %q", ev.Kind)
}

// ArchiveHistory stores a downloaded history blob in its own committed
// transaction, then ingests it. archiveErr means the blob is not safe yet;
// ingestErr lists conversations that failed (the blob stays archived).
func (in *Ingestor) ArchiveHistory(ctx context.Context, key string, hs *waHistorySync.HistorySync) (archiveErr, ingestErr error) {
	payload, err := marshalPartial.Marshal(hs)
	if err != nil {
		return err, nil
	}
	ev, err := in.archive(ctx, store.RawHistory, key, payload)
	if err != nil || ev.Ingested {
		return err, nil
	}
	return nil, in.applyAndMark(ctx, ev)
}

// ReplayPending ingests every archived event not yet ingested, in arrival
// order. Small events go in batches of up to 500 per transaction; a batch
// that fails is retried one event per transaction to isolate the bad one.
// It returns the number of events applied.
func (in *Ingestor) ReplayPending(ctx context.Context) (int, error) {
	return in.replay(ctx, replayKinds)
}

func (in *Ingestor) replay(ctx context.Context, kinds []string) (int, error) {
	applied := 0
	after := int64(0)
	for {
		evs, err := store.ListRaw(ctx, in.DB, store.RawFilter{Kinds: kinds, PendingOnly: true, AfterID: after, Limit: 500, MaxAttempts: maxAttempts})
		if err != nil || len(evs) == 0 {
			return applied, err
		}
		after = evs[len(evs)-1].ID
		for i := 0; i < len(evs); {
			if !batchKinds[evs[i].Kind] {
				_ = in.applyAndMark(ctx, evs[i])
				applied++
				i++
				continue
			}
			j := i
			for j < len(evs) && batchKinds[evs[j].Kind] {
				j++
			}
			if err := in.applyBatch(ctx, evs[i:j]); err != nil {
				for _, ev := range evs[i:j] {
					if err := in.applyAndMark(ctx, ev); err != nil {
						in.log().Error("raw event failed", "id", ev.ID, "kind", ev.Kind, "err", err)
					}
				}
			}
			applied += j - i
			i = j
		}
		if ctx.Err() != nil {
			return applied, ctx.Err()
		}
	}
}

func (in *Ingestor) applyBatch(ctx context.Context, evs []store.RawEvent) error {
	return store.Retry(ctx, func() error {
		return in.DB.Tx(ctx, func(tx *sql.Tx) error {
			for _, ev := range evs {
				err := in.safely(ev, func() error { return in.applyTx(ctx, tx, ev) })
				var panicked errPanic
				if errors.As(err, &panicked) {
					return err // retried one event per transaction, which records it
				}
				var skip errSkip
				if err != nil && !errors.As(err, &skip) {
					return err
				}
				if err := store.MarkRaw(ctx, tx, ev.ID, err, true); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// Reingest rebuilds every derived table from raw_events: content first, then
// the identities of the profile's own session DB when one is given, then
// chat state, so every archive, pin, mute and read finds its chat. Media
// rows are kept and relinked. A marker set with the reset and cleared at the
// end makes an interrupted reingest visible (ResumeReingest).
func (in *Ingestor) Reingest(ctx context.Context, sessionDB *sql.DB) (int, error) {
	if err := in.DB.Tx(ctx, func(tx *sql.Tx) error { return store.ResetDerived(ctx, tx) }); err != nil {
		return 0, err
	}
	n, err := in.replay(ctx, contentKinds)
	if err != nil {
		return n, err
	}
	if sessionDB != nil {
		if err := SyncSessionIdentities(ctx, in, sessionDB); err != nil {
			return n, err
		}
	}
	m, err := in.replay(ctx, []string{store.RawChatState})
	if err != nil {
		return n + m, err
	}
	if _, err = ExpirePending(ctx, in.DB, in.now()); err != nil {
		return n + m, err
	}
	return n + m, store.FinishReingest(ctx, in.DB)
}

// ResumeReingest finishes a reingest that was interrupted (crash, power
// loss, killed process): the derived tables are incomplete until it does.
// It reports whether one was resumed.
func (in *Ingestor) ResumeReingest(ctx context.Context, sessionDB *sql.DB) (bool, error) {
	pending, err := store.ReingestInterrupted(ctx, in.DB)
	if err != nil || !pending {
		return false, err
	}
	in.log().Warn("data.db holds an interrupted reingest: rebuilding it before serving")
	n, err := in.Reingest(ctx, sessionDB)
	if err != nil {
		return true, fmt.Errorf("resume interrupted reingest: %w", err)
	}
	in.log().Info("interrupted reingest finished", "raw_events", n)
	return true, nil
}

// HistorySync archives and ingests an already downloaded history blob, keyed
// by its content. The daemon downloads blobs itself (see historyworker.go);
// this entry point serves replays of saved blobs and tests.
func (in *Ingestor) HistorySync(ctx context.Context, evt *events.HistorySync) error {
	payload, err := marshalPartial.Marshal(evt.Data)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	archiveErr, ingestErr := in.ArchiveHistory(ctx, "history:sha256:"+hex.EncodeToString(sum[:]), evt.Data)
	return errors.Join(archiveErr, ingestErr)
}
