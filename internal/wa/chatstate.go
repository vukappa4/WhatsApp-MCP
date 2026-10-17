package wa

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// Chat state kinds.
const (
	StateArchive = "archive"
	StatePin     = "pin"
	StateMute    = "mute"
	StateRead    = "read"    // mark chat as read (On) or unread (!On)
	StateReceipt = "receipt" // own read receipt sent by another device of the account
)

// ChatState is the archived form of an app-state chat action (live, from a
// full sync or from a recovery) or of an own read receipt.
type ChatState struct {
	JID        string   `json:"jid"`
	Kind       string   `json:"kind"`
	On         bool     `json:"on,omitempty"`          // archived, pinned, muted, read
	MutedUntil int64    `json:"muted_until,omitempty"` // unix seconds, mute only
	ReadUpTo   int64    `json:"read_up_to,omitempty"`  // unix seconds of the newest message a read covers; 0 = action time
	IDs        []string `json:"ids,omitempty"`         // receipt: the message ids read
	TSMS       int64    `json:"ts_ms"`                 // action or receipt time
	FullSync   bool     `json:"full_sync,omitempty"`
}

// chatStateOf converts a whatsmeow chat event; ok is false for events that
// carry no chat state (receipts from other people, delivery receipts...).
func chatStateOf(evt any) (ChatState, bool) {
	switch v := evt.(type) {
	case *events.Archive:
		return ChatState{JID: v.JID.String(), Kind: StateArchive, On: v.Action.GetArchived(), TSMS: v.Timestamp.UnixMilli(), FullSync: v.FromFullSync}, true
	case *events.Pin:
		return ChatState{JID: v.JID.String(), Kind: StatePin, On: v.Action.GetPinned(), TSMS: v.Timestamp.UnixMilli(), FullSync: v.FromFullSync}, true
	case *events.Mute:
		cs := ChatState{JID: v.JID.String(), Kind: StateMute, On: v.Action.GetMuted(), TSMS: v.Timestamp.UnixMilli(), FullSync: v.FromFullSync}
		if cs.On {
			cs.MutedUntil = wastore.MutedForever.Unix()
			if end := v.Action.GetMuteEndTimestamp(); end > 0 {
				cs.MutedUntil = time.UnixMilli(end).Unix()
			}
		}
		return cs, true
	case *events.MarkChatAsRead:
		upTo := v.Action.GetMessageRange().GetLastMessageTimestamp()
		if upTo > 1e11 { // milliseconds
			upTo /= 1000
		}
		return ChatState{JID: v.JID.String(), Kind: StateRead, On: v.Action.GetRead(), ReadUpTo: upTo,
			TSMS: v.Timestamp.UnixMilli(), FullSync: v.FromFullSync}, true
	case *events.Receipt:
		if !v.IsFromMe || (v.Type != types.ReceiptTypeRead && v.Type != types.ReceiptTypeReadSelf) || len(v.MessageIDs) == 0 {
			return ChatState{}, false
		}
		ids := make([]string, len(v.MessageIDs))
		copy(ids, v.MessageIDs)
		return ChatState{JID: v.Chat.String(), Kind: StateReceipt, IDs: ids, TSMS: v.Timestamp.UnixMilli()}, true
	}
	return ChatState{}, false
}

// LiveChatState archives a chat state change in its own committed
// transaction, then applies it.
func (in *Ingestor) LiveChatState(ctx context.Context, cs ChatState) error {
	payload, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	ev, err := in.archive(ctx, store.RawChatState, "chat:"+hex.EncodeToString(sum[:16]), payload)
	if err != nil || ev.Ingested {
		return err
	}
	return in.applyAndMark(ctx, ev)
}

// unreadAfter counts the incoming messages of a chat newer than ts that
// WhatsApp counts as unread (not poll votes or system events).
const unreadAfter = `(SELECT COUNT(*) FROM messages WHERE chat_id = ?1 AND from_me = 0 AND ts > ?2 AND kind NOT IN ('poll_vote', 'system'))`

// chatState applies one chat state change to a stored chat. It never
// creates chats: a change for a chat without stored messages is dropped (the
// raw event stays archived, and reingest applies chat state last).
//
// Archive, pin and mute apply when the action is at least as new as the one
// that set the flag. A read never raises the unread count: it lowers it to
// the incoming messages newer than what was read, so replaying an old read
// from a full sync or a recovery cannot hide newer unread messages. A mark
// as unread raises it to at least 1. Actions stamp only their own flag's
// *_set_at, never state_updated_at, so a history snapshot still fills the
// fields no action has set (see conversation).
func (in *Ingestor) chatState(ctx context.Context, tx *sql.Tx, cs ChatState) error {
	jid := parseJIDOrEmpty(cs.JID)
	if jid.IsEmpty() {
		return errSkip("bad chat jid")
	}
	chat, err := in.findChat(ctx, tx, types.MessageSource{Chat: jid})
	if err != nil || chat.id == 0 {
		return err
	}
	at := cs.TSMS / 1000
	switch cs.Kind {
	case StateArchive, StatePin:
		col := "archived"
		if cs.Kind == StatePin {
			col = "pinned"
		}
		_, err = tx.ExecContext(ctx, `UPDATE chats SET `+col+` = ?1, `+col+`_set_at = ?2
			WHERE id = ?3 AND COALESCE(`+col+`_set_at, 0) <= ?2`, cs.On, at, chat.id)
	case StateMute:
		var until any
		if cs.On {
			until = cs.MutedUntil
		}
		_, err = tx.ExecContext(ctx, `UPDATE chats SET muted_until = ?1, muted_set_at = ?2
			WHERE id = ?3 AND COALESCE(muted_set_at, 0) <= ?2`, until, at, chat.id)
	case StateRead:
		if !cs.On {
			_, err = tx.ExecContext(ctx, `UPDATE chats SET unread_count = MAX(unread_count, 1), read_set_at = ?1
				WHERE id = ?2 AND COALESCE(read_set_at, 0) <= ?1`, at, chat.id)
			break
		}
		upTo := cs.ReadUpTo
		if upTo == 0 {
			upTo = at
		}
		_, err = tx.ExecContext(ctx, `UPDATE chats SET unread_count = MIN(unread_count, `+unreadAfter+`), read_set_at = ?3
			WHERE id = ?1 AND COALESCE(read_set_at, 0) <= ?3`, chat.id, upTo, at)
	case StateReceipt:
		upTo, err := newestOf(ctx, tx, chat.id, cs.IDs)
		if err != nil {
			return err
		}
		if upTo == 0 {
			upTo = at
		}
		_, err = tx.ExecContext(ctx, `UPDATE chats SET unread_count = MIN(unread_count, `+unreadAfter+`) WHERE id = ?1`, chat.id, upTo)
		return err
	default:
		return errSkip("unknown chat state " + cs.Kind)
	}
	return err
}

// newestOf returns the newest ts among the stored messages with these ids, 0 when none is stored.
func newestOf(ctx context.Context, tx *sql.Tx, chatID int64, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := []any{chatID}
	for _, id := range ids {
		args = append(args, id)
	}
	var ts int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts), 0) FROM messages WHERE chat_id = ? AND wa_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...).Scan(&ts)
	return ts, err
}
