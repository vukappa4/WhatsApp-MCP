// Package wa runs the whatsmeow linked device and ingests its events into data.db.
package wa

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// LIDLookup is the part of whatsmeow's LID store the ingestor uses.
type LIDLookup interface {
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
}

// Ingestor writes WhatsApp events into data.db. It does no network I/O, so
// tests drive it with constructed events.
type Ingestor struct {
	DB   *store.DB
	LIDs LIDLookup // optional: resolves LIDs to phone numbers
	// Self returns the account's own phone JID and LID; zero when unknown.
	Self func() (pn, lid types.JID)
	Log  *slog.Logger
	// Now stamps received_at on archived events. Tests replace it.
	Now func() time.Time
	// Storage, when set, hears whether archive writes work.
	Storage StorageReporter
}

func (in *Ingestor) log() *slog.Logger {
	if in.Log == nil {
		return slog.Default()
	}
	return in.Log
}

func (in *Ingestor) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// chatRef is the resolved chat of a message.
type chatRef struct {
	id       int64
	kind     string
	personID int64 // dm only
}

// Message archives and ingests one live message (see LiveMessage).
func (in *Ingestor) Message(ctx context.Context, evt *events.Message) error {
	_, err := in.LiveMessage(ctx, evt, nil)
	return err
}

func (in *Ingestor) self() (pn, lid types.JID) {
	if in.Self == nil {
		return types.JID{}, types.JID{}
	}
	pn, lid = in.Self()
	return pn.ToNonAD(), lid.ToNonAD()
}

// me returns the account owner's person, linking its phone JID and LID as
// the device store knows them now.
func (in *Ingestor) me(ctx context.Context, e store.Execer) (int64, error) {
	pn, lid := in.self()
	if pn.IsEmpty() && lid.IsEmpty() {
		return 0, nil
	}
	id, err := people.LinkCurrent(ctx, e, pn, lid, in.now())
	if err != nil {
		return 0, err
	}
	return id, people.MarkMe(ctx, e, id)
}

// person resolves a user JID and links its alternate address, seen together
// at time at (the message's). With only a LID, it asks the LID store for the
// number it holds now. With a LID and a number it returns the LID's person:
// the account the evidence is about.
func (in *Ingestor) person(ctx context.Context, e store.Execer, j, alt types.JID, at time.Time) (int64, error) {
	if people.IsUser(alt) {
		return people.Link(ctx, e, j, alt, at)
	}
	id, err := people.Resolve(ctx, e, j)
	if err != nil {
		return 0, err
	}
	if lid, ok := lidOf(j); ok && in.LIDs != nil {
		pn, lerr := in.LIDs.GetPNForLID(ctx, lid)
		if lerr == nil && !pn.IsEmpty() {
			return people.LinkCurrent(ctx, e, j, pn, in.now())
		}
	}
	return id, nil
}

// personJIDs lists the JIDs that person(j, no alt) links: j and, for a LID,
// the phone number the LID store knows. It is nil when the lookup fails, so
// a step judged from it always runs.
func (in *Ingestor) personJIDs(ctx context.Context, j types.JID) []types.JID {
	lid, ok := lidOf(j)
	if !ok || in.LIDs == nil {
		return []types.JID{j}
	}
	pn, err := in.LIDs.GetPNForLID(ctx, lid)
	if err != nil {
		return nil
	}
	if pn.IsEmpty() {
		return []types.JID{j}
	}
	return []types.JID{j, pn}
}

// lidOf returns the device-less @lid JID of a LID, also of a hosted device
// (user:dev@hosted.lid), which whatsmeow's LID store does not know by that server.
func lidOf(j types.JID) (types.JID, bool) {
	n, kind, ok := people.Normalize(j)
	return n, ok && kind == people.KindLID
}

func chatAlt(src types.MessageSource) types.JID {
	if src.IsFromMe {
		return src.RecipientAlt
	}
	return src.SenderAlt
}

var serverKind = map[string]string{
	types.GroupServer:      "group",
	types.NewsletterServer: "newsletter",
	types.BroadcastServer:  "broadcast",
}

// chatFor resolves the chat of a message sent at time at, creating chat and
// person when needed. Every user JID of a person maps to that person's single
// DM chat. id is 0 for chats that are not stored (status broadcasts, unknown
// servers).
func (in *Ingestor) chatFor(ctx context.Context, e store.Execer, src types.MessageSource, at time.Time) (chatRef, error) {
	chat := src.Chat
	switch {
	case chat == types.StatusBroadcastJID:
		return chatRef{}, nil
	case people.IsUser(chat):
		pid, err := in.person(ctx, e, chat, chatAlt(src), at)
		if err != nil {
			return chatRef{}, err
		}
		id, err := people.EnsureDMChat(ctx, e, pid, chat)
		return chatRef{id: id, kind: "dm", personID: pid}, err
	}
	kind := serverKind[chat.Server]
	if kind == "" {
		return chatRef{}, nil
	}
	id, err := store.EnsureChat(ctx, e, chat.ToNonAD().String(), kind)
	return chatRef{id: id, kind: kind}, err
}

// findChat resolves an existing chat without creating chats or persons.
// Events that act on another message (edits, revokes, reactions, votes) use
// it, so they never invent a chat for a message we do not have.
func (in *Ingestor) findChat(ctx context.Context, e store.Execer, src types.MessageSource) (chatRef, error) {
	chat := src.Chat
	if chat == types.StatusBroadcastJID {
		return chatRef{}, nil
	}
	if people.IsUser(chat) {
		for _, j := range []types.JID{chat, chatAlt(src)} {
			if !people.IsUser(j) {
				continue
			}
			pid, err := people.Lookup(ctx, e, j)
			if err != nil || pid == 0 {
				if err != nil {
					return chatRef{}, err
				}
				continue
			}
			id, err := people.DMChatID(ctx, e, pid)
			if err != nil || id != 0 {
				return chatRef{id: id, kind: "dm", personID: pid}, err
			}
		}
		return chatRef{}, nil
	}
	kind := serverKind[chat.Server]
	if kind == "" {
		return chatRef{}, nil
	}
	id, err := store.ChatIDByJID(ctx, e, chat.ToNonAD().String())
	return chatRef{id: id, kind: kind}, err
}

// sender resolves the author person of a message (0 for newsletters).
func (in *Ingestor) sender(ctx context.Context, e store.Execer, info types.MessageInfo, chat chatRef) (int64, error) {
	if info.IsFromMe {
		return in.me(ctx, e)
	}
	if chat.kind == "dm" && (info.Sender.IsEmpty() || info.Sender.ToNonAD() == info.Chat.ToNonAD()) {
		return chat.personID, nil
	}
	if !people.IsUser(info.Sender) {
		return 0, nil
	}
	return in.person(ctx, e, info.Sender, info.SenderAlt, info.Timestamp)
}

// ingest applies one message (live, history) of any kind. stored is true
// when a new message row was written. vote is the decrypted poll vote, when
// the message is one and the client could decrypt it.
func (in *Ingestor) ingest(ctx context.Context, tx *sql.Tx, info types.MessageInfo, msg *waE2E.Message, src string, vote *waE2E.PollVoteMessage) (stored bool, err error) {
	stored, err = in.ingestOne(ctx, tx, info, msg, src, vote)
	var nt noTarget
	if errors.As(err, &nt) {
		return false, in.deferMutation(ctx, tx, info, msg, vote, src, nt.id)
	}
	return stored, err
}

func (in *Ingestor) ingestOne(ctx context.Context, tx *sql.Tx, info types.MessageInfo, msg *waE2E.Message, src string, vote *waE2E.PollVoteMessage) (stored bool, err error) {
	msg, assoc := unwrapAssoc(msg)
	if msg == nil || info.ID == "" {
		return false, nil
	}
	if duplicateAssoc(assoc) {
		in.drop(ctx, dropDuplicateMedia, info, assoc.GetAssociationType().String())
		return false, nil
	}
	if msg.GetPlaceholderMessage() != nil {
		in.drop(ctx, dropPlaceholder, info, msg.GetPlaceholderMessage().GetType().String())
		return false, nil
	}
	// Events acting on another message: never create chats or persons for them.
	if pm := msg.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			return false, in.revoke(ctx, tx, info, pm.GetKey().GetID())
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			at := info.Timestamp
			if ms := pm.GetTimestampMS(); ms > 0 {
				at = time.UnixMilli(ms)
			}
			return in.edit(ctx, tx, info, pm.GetKey().GetID(), pm.GetEditedMessage(), at, src)
		}
		return false, nil // history sync notifications, key shares, ephemeral settings...
	}
	if r := msg.GetReactionMessage(); r != nil {
		at := info.Timestamp
		if r.GetSenderTimestampMS() != 0 {
			at = time.UnixMilli(r.GetSenderTimestampMS())
		}
		return false, in.react(ctx, tx, info, r.GetKey().GetID(), r.GetText(), at)
	}
	if pu := msg.GetPollUpdateMessage(); pu != nil {
		return in.pollVote(ctx, tx, info, pu.GetPollCreationMessageKey().GetID(), vote, src)
	}
	c, ok := extract(msg)
	if !ok {
		in.drop(ctx, dropNoContent, info, "")
		return false, nil
	}
	if assoc.GetAssociationType() == waE2E.MessageAssociation_MEDIA_ALBUM {
		c.Album = assoc.GetParentMessageKey().GetID()
	}
	chat, err := in.chatFor(ctx, tx, info.MessageSource, info.Timestamp)
	if err != nil || chat.id == 0 {
		return false, err
	}
	sender, err := in.sender(ctx, tx, info, chat)
	if err != nil {
		return false, err
	}
	if err := in.senderNames(ctx, tx, info, sender); err != nil {
		return false, err
	}
	return in.upsert(ctx, tx, chat, sender, info, c, src, 0, true)
}

func (in *Ingestor) senderNames(ctx context.Context, tx *sql.Tx, info types.MessageInfo, sender int64) error {
	if info.IsFromMe || sender == 0 {
		return nil
	}
	names := people.Names{PushName: info.PushName}
	if info.VerifiedName != nil {
		names.BusinessName = info.VerifiedName.Details.GetVerifiedName()
	}
	return people.SetNames(ctx, tx, sender, names)
}

// upsert stores a message row (and its media row). When the row exists, the
// two copies merge with store.MergeMessage: a richer source (legacy <
// history < live) upgrades the row, a poorer one only fills gaps. bump
// controls whether a new row moves the chat's recency. stored is true for a
// new row.
func (in *Ingestor) upsert(ctx context.Context, tx *sql.Tx, chat chatRef, sender int64, info types.MessageInfo, c content, src string, editedAt int64, bump bool) (bool, error) {
	var replyTo, replyPreview string
	forwarded := false
	if ci := c.Ctx; ci != nil {
		if id := ci.GetStanzaID(); id != "" {
			replyTo, replyPreview = id, preview(ci.GetQuotedMessage())
		}
		forwarded = ci.GetIsForwarded()
	}
	senderJID := ""
	if !info.Sender.IsEmpty() {
		senderJID = info.Sender.ToNonAD().String()
	}
	ts := info.Timestamp.Unix()
	nw := store.MessageFields{
		Source: src, Kind: c.Kind, Text: c.Text, SenderPerson: sender, SenderJID: senderJID, FromMe: info.IsFromMe,
		TS: ts, ReplyTo: replyTo, ReplyPreview: replyPreview, EditedAt: editedAt, Forwarded: forwarded,
	}
	var mf store.MediaFields
	if m := c.Media; m != nil {
		mf = store.MediaFields{Mimetype: m.Mimetype, Filename: m.Filename, Size: int64(m.Size), Duration: int64(m.Duration),
			DirectPath: m.DirectPath, URL: m.URL, MediaKey: m.MediaKey, SHA256: m.FileSHA256, EncSHA256: m.FileEncSHA256}
	}

	old, found, err := store.LoadMessage(ctx, tx, chat.id, info.ID)
	if err != nil {
		return false, err
	}
	if found {
		merged := store.MergeMessage(old, nw)
		if c.Media != nil {
			if old.MediaID != 0 {
				if err := store.UpdateMedia(ctx, tx, old.MediaID, mf, store.SourceRank(src) > store.SourceRank(old.Source)); err != nil {
					return false, err
				}
			} else if merged.MediaID, err = store.AttachMedia(ctx, tx, mf, chat.id, info.ID); err != nil {
				return false, err
			}
		}
		if err := store.SaveMessage(ctx, tx, merged); err != nil {
			return false, err
		}
		if c.Album != "" {
			_, err = tx.ExecContext(ctx, `UPDATE messages SET album_wa_id = ? WHERE id = ? AND album_wa_id IS NULL`, c.Album, old.ID)
		}
		return false, err
	}

	var mediaID any
	if c.Media != nil {
		if mediaID, err = store.AttachMedia(ctx, tx, mf, chat.id, info.ID); err != nil {
			return false, err
		}
	}
	var senderID, edited any
	if sender != 0 {
		senderID = sender
	}
	if editedAt != 0 {
		edited = editedAt
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(chat_id, wa_id, sender_person_id, sender_jid, from_me, ts, kind, text,
		reply_to_wa_id, reply_preview, edited_at, forwarded, media_id, source, album_wa_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		chat.id, info.ID, senderID, senderJID, info.IsFromMe, ts, c.Kind, c.Text, nullStr(replyTo), nullStr(replyPreview),
		edited, forwarded, mediaID, src, nullStr(c.Album))
	if err != nil {
		return false, err
	}
	// Edits, revokes, reactions and votes that arrived before this message.
	if err := in.applyPending(ctx, tx, info.ID); err != nil {
		return false, err
	}
	if !bump {
		return true, nil
	}
	if err := store.BumpChat(ctx, tx, chat.id, ts); err != nil {
		return false, err
	}
	// WhatsApp counts neither poll votes nor system events as unread, and
	// neither resets the count (unreadAfter counts the same way).
	if src == store.SourceLive && c.Kind != "poll_vote" && c.Kind != "system" {
		q := `UPDATE chats SET unread_count = unread_count + 1 WHERE id = ?`
		if info.IsFromMe {
			q = `UPDATE chats SET unread_count = 0 WHERE id = ?`
		}
		if _, err := tx.ExecContext(ctx, q, chat.id); err != nil {
			return false, err
		}
	}
	return true, store.BumpStateTS(ctx, tx, store.KeyLastEventAt, ts)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// target finds the stored message an event acts on.
func (in *Ingestor) target(ctx context.Context, tx *sql.Tx, src types.MessageSource, waID string) (chatRef, store.MessageFields, bool, error) {
	if waID == "" {
		return chatRef{}, store.MessageFields{}, false, nil
	}
	chat, err := in.findChat(ctx, tx, src)
	if err != nil || chat.id == 0 {
		return chat, store.MessageFields{}, false, err
	}
	f, ok, err := store.LoadMessage(ctx, tx, chat.id, waID)
	return chat, f, ok, err
}

// sameAuthor reports whether actor wrote message f.
func sameAuthor(f store.MessageFields, info types.MessageInfo, actor int64) bool {
	if info.IsFromMe || f.FromMe {
		return info.IsFromMe && f.FromMe
	}
	return f.SenderPerson != 0 && f.SenderPerson == actor
}

// revoke marks a message deleted and keeps its original text. Only the
// author may revoke, or a group admin (WhatsApp marks those with edit="8").
// A target not stored yet is a noTarget error: the revoke waits for it.
func (in *Ingestor) revoke(ctx context.Context, tx *sql.Tx, info types.MessageInfo, target string) error {
	chat, f, ok, err := in.target(ctx, tx, info.MessageSource, target)
	if err != nil {
		return err
	}
	if !ok {
		return missing(target)
	}
	actor, err := in.sender(ctx, tx, info, chat)
	if err != nil {
		return err
	}
	admin := info.Edit == types.EditAttributeAdminRevoke && chat.kind == "group"
	if !admin && !sameAuthor(f, info, actor) {
		in.log().Warn("ignoring revoke by someone other than the author", "chat", chat.id, "target", target)
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE messages SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, info.Timestamp.Unix(), f.ID)
	return err
}

// edit replaces the text of a message written by the same author. An older
// edit never overwrites a newer one. A history edit whose original is not
// stored is kept under the original id, creating its chat like any stored
// message; a live one is dropped.
func (in *Ingestor) edit(ctx context.Context, tx *sql.Tx, info types.MessageInfo, target string, edited *waE2E.Message, at time.Time, src string) (bool, error) {
	if target == "" || edited == nil {
		return false, nil
	}
	c, ok := extract(unwrap(edited))
	if !ok {
		return false, nil
	}
	chat, f, found, err := in.target(ctx, tx, info.MessageSource, target)
	if err != nil {
		return false, err
	}
	if !found && src == store.SourceLive {
		return false, missing(target) // waits for the original
	}
	if !found && chat.id == 0 {
		// The edit will be stored as the message: it creates the chat like one.
		if chat, err = in.chatFor(ctx, tx, info.MessageSource, info.Timestamp); err != nil || chat.id == 0 {
			return false, err
		}
	}
	actor, err := in.sender(ctx, tx, info, chat)
	if err != nil {
		return false, err
	}
	if found {
		if !sameAuthor(f, info, actor) {
			in.log().Warn("ignoring edit by someone other than the author", "chat", chat.id, "target", target)
			return false, nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE messages SET text = ?, edited_at = ? WHERE id = ? AND COALESCE(edited_at, 0) < ?`,
			c.Text, at.Unix(), f.ID, at.Unix())
		return false, err
	}
	orig := info
	orig.ID = target
	return in.upsert(ctx, tx, chat, actor, orig, c, src, at.Unix(), true)
}

// react sets or (with an empty emoji) removes the actor's reaction on a
// stored message. A reaction to a message not stored yet waits for it.
func (in *Ingestor) react(ctx context.Context, tx *sql.Tx, info types.MessageInfo, target, emoji string, at time.Time) error {
	chat, f, ok, err := in.target(ctx, tx, info.MessageSource, target)
	if err != nil {
		return err
	}
	if !ok {
		return missing(target)
	}
	actor, err := in.sender(ctx, tx, info, chat)
	if err != nil || actor == 0 {
		return err
	}
	return setReaction(ctx, tx, f.ID, actor, emoji, at)
}

func setReaction(ctx context.Context, tx *sql.Tx, msgID, person int64, emoji string, at time.Time) error {
	if emoji == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE message_id = ? AND person_id = ? AND ts <= ?`, msgID, person, at.Unix())
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO reactions(message_id, person_id, emoji, ts) VALUES (?, ?, ?, ?)
		ON CONFLICT(message_id, person_id) DO UPDATE SET emoji = excluded.emoji, ts = excluded.ts WHERE excluded.ts >= reactions.ts`,
		msgID, person, emoji, at.Unix())
	return err
}

// pollVote stores a decrypted vote as a poll_vote message replying to the
// poll, with the chosen option names. Votes that could not be decrypted are
// dropped; a vote whose poll is not stored yet waits for it. Votes do not
// move chat recency.
func (in *Ingestor) pollVote(ctx context.Context, tx *sql.Tx, info types.MessageInfo, pollID string, vote *waE2E.PollVoteMessage, src string) (bool, error) {
	if vote == nil {
		return false, nil
	}
	chat, poll, ok, err := in.target(ctx, tx, info.MessageSource, pollID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, missing(pollID)
	}
	if poll.Kind != "poll" {
		return false, nil
	}
	actor, err := in.sender(ctx, tx, info, chat)
	if err != nil {
		return false, err
	}
	text := "removed vote"
	if names := votedOptions(poll.Text, vote.GetSelectedOptions()); len(names) > 0 {
		text = "voted: " + strings.Join(names, ", ")
	}
	c := content{Kind: "poll_vote", Text: text, Ctx: &waE2E.ContextInfo{StanzaID: &pollID}}
	return in.upsert(ctx, tx, chat, actor, info, c, src, 0, false)
}

// votedOptions maps selected option hashes (SHA-256 of the option name) back
// to the names stored in the poll text ("Question\n- option\n- option").
func votedOptions(pollText string, selected [][]byte) []string {
	byHash := map[[32]byte]string{}
	for _, line := range strings.Split(pollText, "\n") {
		if name, ok := strings.CutPrefix(line, "- "); ok {
			byHash[sha256.Sum256([]byte(name))] = name
		}
	}
	var out []string
	for _, h := range selected {
		if len(h) != 32 {
			continue
		}
		if name, ok := byHash[[32]byte(h)]; ok {
			out = append(out, name)
		} else {
			out = append(out, "unknown option")
		}
	}
	return out
}

// reactor resolves who reacted, from a history reaction key.
func (in *Ingestor) reactor(ctx context.Context, tx *sql.Tx, chat chatRef, key *waCommon.MessageKey) (int64, error) {
	if key.GetFromMe() {
		return in.me(ctx, tx)
	}
	if p := key.GetParticipant(); p != "" {
		if j, err := types.ParseJID(p); err == nil && people.IsUser(j) {
			return in.person(ctx, tx, j, types.JID{}, time.Time{})
		}
	}
	if chat.kind == "dm" {
		return chat.personID, nil
	}
	return 0, nil
}

func (in *Ingestor) setNames(ctx context.Context, e store.Execer, j types.JID, n people.Names) error {
	pid, err := in.person(ctx, e, j, types.JID{}, time.Time{})
	if err != nil {
		return err
	}
	return people.SetNames(ctx, e, pid, n)
}

// describeParam renders a stub parameter, replacing user JIDs with names.
func (in *Ingestor) describeParam(ctx context.Context, tx *sql.Tx, p string) string {
	j, err := types.ParseJID(p)
	if err != nil || !people.IsUser(j) {
		return p
	}
	return in.describeJID(ctx, tx, j)
}

func (in *Ingestor) describeJID(ctx context.Context, tx *sql.Tx, j types.JID) string {
	pid, err := in.person(ctx, tx, j, types.JID{}, time.Time{})
	if err != nil {
		return j.User
	}
	var name, source string
	var phone sql.NullString
	if tx.QueryRowContext(ctx, `SELECT display_name, name_source, phone_e164 FROM persons WHERE id = ?`, pid).Scan(&name, &source, &phone) != nil || name == "" {
		return j.User
	}
	if source == people.SourcePushName || source == people.SourceBusinessName {
		return people.ProfileName(name)
	}
	return name
}
