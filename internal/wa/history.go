package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// testHookConversation lets tests fail one conversation.
var testHookConversation func(id string) error

// historyBlob ingests a decoded history sync blob: LID mappings and push
// names, then each conversation in its own transaction. A failing
// conversation is logged and skipped; the others still land. The returned
// error lists the failures (nil when all succeeded).
func (in *Ingestor) historyBlob(ctx context.Context, data *waHistorySync.HistorySync, receivedAt time.Time) error {
	if data == nil {
		return nil
	}
	var errs []error
	if err := in.historyIdentities(ctx, data, receivedAt); err != nil {
		errs = append(errs, fmt.Errorf("identities: %w", err))
	}
	// Chat state (archived, pinned, muted, unread) only comes from full snapshots.
	snapshot := data.GetSyncType() == waHistorySync.HistorySync_INITIAL_BOOTSTRAP || data.GetSyncType() == waHistorySync.HistorySync_FULL
	dropped := newDrops()
	stored := 0
	for _, conv := range data.GetConversations() {
		var n int
		var attempt *drops // counts of the committed attempt only
		err := store.Retry(ctx, func() error {
			var cctx context.Context
			cctx, attempt = withDrops(ctx)
			var err error
			n, err = in.conversation(cctx, conv, snapshot, receivedAt)
			return err
		})
		if err != nil {
			in.log().Error("history conversation failed; continuing", "chat", conv.GetID(), "err", err)
			errs = append(errs, fmt.Errorf("conversation %s: %w", conv.GetID(), err))
			continue
		}
		dropped.add(attempt)
		stored += n
	}
	in.log().Info("history sync ingested", append([]any{"type", data.GetSyncType().String(), "chunk", data.GetChunkOrder(),
		"progress", data.GetProgress(), "conversations", len(data.GetConversations()), "messages", stored, "failed", len(errs)},
		dropped.summary()...)...)
	if err := store.Retry(ctx, func() error {
		return in.DB.Tx(ctx, func(tx *sql.Tx) error {
			if err := store.SetState(ctx, tx, store.KeyHistorySyncAt, fmt.Sprint(receivedAt.Unix())); err != nil {
				return err
			}
			return store.SetState(ctx, tx, store.KeyHistorySyncState,
				fmt.Sprintf("%s chunk %d progress %d%%", data.GetSyncType(), data.GetChunkOrder(), data.GetProgress()))
		})
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// historyIdentities applies a blob's LID mappings, the phone's map when the
// blob arrived at receivedAt, and its push names. It writes only what
// data.db lacks, in short transactions (see identitySteps).
func (in *Ingestor) historyIdentities(ctx context.Context, data *waHistorySync.HistorySync, receivedAt time.Time) error {
	steps, err := in.newIdentitySteps(ctx)
	if err != nil {
		return err
	}
	pn, lid := in.self()
	steps.always(func(tx *sql.Tx) error { _, err := in.me(ctx, tx); return err }, pn, lid)
	for _, m := range data.GetPhoneNumberToLidMappings() {
		pn, err1 := types.ParseJID(m.GetPnJID())
		lid, err2 := types.ParseJID(m.GetLidJID())
		if err1 != nil || err2 != nil {
			continue
		}
		steps.add([]types.JID{lid, pn}, people.Names{}, func(tx *sql.Tx) error {
			_, err := people.LinkCurrent(ctx, tx, lid, pn, receivedAt)
			return err
		})
	}
	for _, p := range data.GetPushnames() {
		j, err := types.ParseJID(p.GetID())
		if err != nil || !people.IsUser(j) || p.GetPushname() == "" {
			continue
		}
		names := people.Names{PushName: p.GetPushname()}
		steps.add(in.personJIDs(ctx, j), names, func(tx *sql.Tx) error { return in.setNames(ctx, tx, j, names) })
	}
	return steps.apply(ctx, in.DB)
}

func (in *Ingestor) conversation(ctx context.Context, conv *waHistorySync.Conversation, snapshot bool, receivedAt time.Time) (int, error) {
	chatJID, err := types.ParseJID(conv.GetID())
	if err != nil || chatJID == types.StatusBroadcastJID {
		return 0, nil
	}
	if testHookConversation != nil {
		if err := testHookConversation(conv.GetID()); err != nil {
			return 0, err
		}
	}
	stored := 0
	err = in.DB.Tx(ctx, func(tx *sql.Tx) error {
		if people.IsUser(chatJID) {
			// The chat's other address, as the phone knew it when the blob arrived.
			for _, alt := range []string{conv.GetLidJID(), conv.GetPnJID()} {
				if a, err := types.ParseJID(alt); err == nil && people.IsUser(a) {
					if _, err := people.LinkCurrent(ctx, tx, chatJID, a, receivedAt); err != nil {
						return err
					}
				}
			}
			// A number change is a fact about the contact, not proof both
			// numbers are one person: record it, do not merge.
			for _, other := range []string{conv.GetOldJID(), conv.GetNewJID()} {
				if o, err := types.ParseJID(other); err == nil && people.IsUser(o) && o.ToNonAD() != chatJID.ToNonAD() {
					if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO identity_notes(jid, other_jid, note, ts) VALUES (?, ?, 'number change', ?)`,
						chatJID.ToNonAD().String(), o.ToNonAD().String(), receivedAt.Unix()); err != nil {
						return err
					}
				}
			}
		}
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		// Groups, newsletters and broadcasts are stored with their name even
		// without messages. A DM chat is created only by its first stored
		// message, so conversations holding only stubs leave no empty chat.
		var chat chatRef
		var err error
		if !people.IsUser(chatJID) {
			if chat, err = in.chatFor(ctx, tx, types.MessageSource{Chat: chatJID}, receivedAt); err != nil || chat.id == 0 {
				return err
			}
			if err := store.SetChatName(ctx, tx, chat.id, name); err != nil {
				return err
			}
			if d := conv.GetDescription(); d != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE chats SET description = ? WHERE id = ?`, d, chat.id); err != nil {
					return err
				}
			}
		} else if name != "" && name != chatJID.User && !fallbackName(name) {
			// The phone's chat title is a weak name: kept only while the
			// person has no name of its own, and replaced by any later one.
			pid, err := in.person(ctx, tx, chatJID, types.JID{}, time.Time{})
			if err != nil {
				return err
			}
			if err := in.weakName(ctx, tx, pid, name); err != nil {
				return err
			}
		}
		msgs := conv.GetMessages()
		for i := len(msgs) - 1; i >= 0; i-- { // WhatsApp sends newest first
			web := msgs[i].GetMessage()
			if web == nil {
				continue
			}
			ok, err := in.webMessage(ctx, tx, chatJID, web)
			if err != nil {
				return err
			}
			if ok {
				stored++
			}
		}
		if chat.id == 0 {
			if chat, err = in.findChat(ctx, tx, types.MessageSource{Chat: chatJID}); err != nil || chat.id == 0 {
				return err
			}
		}
		if snapshot {
			// The first snapshot of a chat sets its state (state_updated_at);
			// a flag an app-state action already set (*_set_at) stays. The
			// unread count is the phone's own count at snapshot time.
			var muted any
			if end := conv.GetMuteEndTime(); end > 0 {
				muted = int64(end)
			}
			unread := conv.GetUnreadCount()
			if unread == 0 && conv.GetMarkedAsUnread() {
				unread = 1
			}
			if _, err := tx.ExecContext(ctx, `UPDATE chats SET
				archived = CASE WHEN archived_set_at IS NULL THEN ?1 ELSE archived END,
				pinned = CASE WHEN pinned_set_at IS NULL THEN ?2 ELSE pinned END,
				muted_until = CASE WHEN muted_set_at IS NULL THEN ?3 ELSE muted_until END,
				unread_count = ?4, state_updated_at = ?5
				WHERE id = ?6 AND state_updated_at IS NULL`,
				conv.GetArchived(), conv.GetPinned() > 0, muted, unread, receivedAt.Unix(), chat.id); err != nil {
				return err
			}
		}
		return nil
	})
	return stored, err
}

// webMessage ingests one history message, its stub (system event), and the
// reactions and poll votes WhatsApp attached to it.
func (in *Ingestor) webMessage(ctx context.Context, tx *sql.Tx, chatJID types.JID, web *waWeb.WebMessageInfo) (bool, error) {
	info, err := in.parseWebInfo(chatJID, web)
	if err != nil {
		in.log().Debug("skip history message", "chat", chatJID, "err", err)
		return false, nil
	}
	stored := false
	switch {
	case web.GetMessage() != nil:
		if stored, err = in.ingest(ctx, tx, info, web.GetMessage(), store.SourceHistory, nil); err != nil {
			return false, err
		}
	case web.GetMessageStubType() != 0:
		if stored, err = in.stub(ctx, tx, info, web); err != nil {
			return false, err
		}
	}
	if len(web.GetReactions()) == 0 && len(web.GetPollUpdates()) == 0 {
		return stored, nil
	}
	chat, f, ok, err := in.target(ctx, tx, info.MessageSource, info.ID)
	if err != nil || !ok {
		return stored, err
	}
	for _, r := range web.GetReactions() {
		reactor, err := in.reactor(ctx, tx, chat, r.GetKey())
		if err != nil {
			return stored, err
		}
		if reactor == 0 {
			continue
		}
		at := time.UnixMilli(r.GetSenderTimestampMS())
		if r.GetSenderTimestampMS() == 0 {
			at = info.Timestamp
		}
		if err := setReaction(ctx, tx, f.ID, reactor, r.GetText(), at); err != nil {
			return stored, err
		}
	}
	for _, pu := range web.GetPollUpdates() {
		key := pu.GetPollUpdateMessageKey()
		vinfo := types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chatJID, IsFromMe: key.GetFromMe(), IsGroup: info.IsGroup},
			ID:            key.GetID(),
			Timestamp:     time.UnixMilli(pu.GetSenderTimestampMS()),
		}
		switch {
		case key.GetFromMe():
			vinfo.Sender, _ = in.self()
		case key.GetParticipant() != "":
			vinfo.Sender, _ = types.ParseJID(key.GetParticipant())
		default:
			vinfo.Sender = chatJID
		}
		if vinfo.ID == "" || pu.GetSenderTimestampMS() == 0 {
			continue
		}
		if _, err := in.pollVote(ctx, tx, vinfo, info.ID, pu.GetVote(), store.SourceHistory); err != nil {
			return stored, err
		}
	}
	return stored, nil
}

// parseWebInfo mirrors whatsmeow's ParseWebMessage without needing a logged-in client.
func (in *Ingestor) parseWebInfo(chatJID types.JID, web *waWeb.WebMessageInfo) (types.MessageInfo, error) {
	key := web.GetKey()
	info := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chatJID,
			IsFromMe: key.GetFromMe(),
			IsGroup:  chatJID.Server == types.GroupServer,
		},
		ID:        key.GetID(),
		PushName:  web.GetPushName(),
		Timestamp: time.Unix(int64(web.GetMessageTimestamp()), 0),
	}
	if info.ID == "" || web.GetMessageTimestamp() == 0 {
		return info, errors.New("missing id or timestamp")
	}
	var err error
	switch {
	case info.IsFromMe:
		if s := web.GetOriginalSelfAuthorUserJIDString(); s != "" {
			info.Sender, err = types.ParseJID(s)
		} else if pn, lid := in.self(); chatJID.Server == types.HiddenUserServer && !lid.IsEmpty() {
			info.Sender = lid
		} else {
			info.Sender = pn
		}
	case chatJID.Server == types.DefaultUserServer || chatJID.Server == types.HiddenUserServer || chatJID.Server == types.NewsletterServer:
		info.Sender = chatJID
	case web.GetParticipant() != "":
		info.Sender, err = types.ParseJID(web.GetParticipant())
	case key.GetParticipant() != "":
		info.Sender, err = types.ParseJID(key.GetParticipant())
	default:
		err = errors.New("no sender")
	}
	return info, err
}

// stub stores history system events (group changes, missed calls, deleted
// messages). A revoke stub, by the author or by a group admin, marks the
// stored message deleted. Undecryptable (CIPHERTEXT) and other stubs are
// counted and logged at debug level, never stored.
func (in *Ingestor) stub(ctx context.Context, tx *sql.Tx, info types.MessageInfo, web *waWeb.WebMessageInfo) (bool, error) {
	st := web.GetMessageStubType()
	name := st.String()
	revoke := st == waWeb.WebMessageInfo_REVOKE || st == waWeb.WebMessageInfo_ADMIN_REVOKE
	if revoke {
		_, f, ok, err := in.target(ctx, tx, info.MessageSource, info.ID)
		if err != nil {
			return false, err
		}
		if ok {
			_, err = tx.ExecContext(ctx, `UPDATE messages SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, info.Timestamp.Unix(), f.ID)
			return false, err
		}
	}
	var c content
	switch {
	case revoke:
		c = content{Kind: "text"}
	case st == waWeb.WebMessageInfo_CIPHERTEXT:
		in.drop(ctx, dropCiphertext, info, "")
		return false, nil
	case strings.HasPrefix(name, "CALL_MISSED"):
		c = content{Kind: "call", Text: strings.ToLower(strings.ReplaceAll(name, "_", " "))}
	case strings.HasPrefix(name, "GROUP_"):
		text := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, "GROUP_"), "_", " "))
		var params []string
		for _, p := range web.GetMessageStubParameters() {
			params = append(params, in.describeParam(ctx, tx, p))
		}
		if len(params) > 0 {
			text += ": " + strings.Join(params, ", ")
		}
		c = content{Kind: "system", Text: "group " + text}
	default:
		in.drop(ctx, dropStub, info, name)
		return false, nil
	}
	chat, err := in.chatFor(ctx, tx, info.MessageSource, info.Timestamp)
	if err != nil || chat.id == 0 {
		return false, err
	}
	sender, err := in.sender(ctx, tx, info, chat)
	if err != nil {
		return false, err
	}
	stored, err := in.upsert(ctx, tx, chat, sender, info, c, store.SourceHistory, 0, true)
	if err != nil || !revoke {
		return stored, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE messages SET deleted_at = ts WHERE chat_id = ? AND wa_id = ? AND deleted_at IS NULL`, chat.id, info.ID)
	return stored, err
}
