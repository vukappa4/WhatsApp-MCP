package wa

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// LegacyStats summarizes an import of the old bridge's messages.db.
type LegacyStats struct {
	LegacyChats, LegacyMessages       int
	LegacyUnnamedChats                int // legacy chats whose name is empty, a bare number or "Group <id>"
	LegacyDMJIDs                      int // legacy DM chats (phone and LID JIDs)
	MergedDMChats                     int // legacy DM chats folded into another JID's chat of the same person
	ArchivedNew                       int // raw events added by this run
	ChatsImported, MessagesImported   int // rows added to data.db by this run
	MessagesSkipped                   int // legacy messages not stored (status broadcasts, bad rows)
	Persons, Chats, Messages, Unnamed int // totals in data.db after the import
	LIDLinks, Contacts                int
}

// legacyChat and legacyMessage are the raw_events payloads of legacy rows.
type legacyChat struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

type legacyMessage struct {
	ID        string `json:"id"`
	Chat      string `json:"chat"`
	Sender    string `json:"sender"`
	Text      string `json:"text"`
	Timestamp string `json:"ts"`
	FromMe    bool   `json:"from_me"`
	MediaType string `json:"media_type,omitempty"`
	Filename  string `json:"filename,omitempty"`
	URL       string `json:"url,omitempty"`
	MediaKey  []byte `json:"media_key,omitempty"`
	SHA256    []byte `json:"sha256,omitempty"`
	EncSHA256 []byte `json:"enc_sha256,omitempty"`
	Length    int64  `json:"length,omitempty"`
}

var (
	digitsName   = regexp.MustCompile(`^[+\d\s]*$`)
	groupIDName  = regexp.MustCompile(`^Group \d+$`)
	legacyLayout = []string{"2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05.999999999-07:00", time.RFC3339Nano, "2006-01-02 15:04:05"}
)

// fallbackName reports names the legacy bridge invented when it had none.
func fallbackName(name string) bool {
	return digitsName.MatchString(name) || groupIDName.MatchString(name)
}

func parseLegacyTime(s string) (time.Time, error) {
	for _, l := range legacyLayout {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", s)
}

// legacyBatch is how many legacy rows go into one archive transaction.
var legacyBatch = 500

// ImportLegacy imports the old bridge's messages.db plus the identities of
// its whatsmeow session DB, both opened read-only. Every legacy row is first
// archived in raw_events (in batches, each its own transaction), then the
// archive is ingested, so reingest can rebuild it later. Running it twice
// imports nothing new.
func ImportLegacy(ctx context.Context, db *store.DB, messagesPath, sessionPath string) (*LegacyStats, error) {
	st := &LegacyStats{}
	sess, err := sql.Open("sqlite", store.ReadOnlyDSN(sessionPath))
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	ids, err := ReadSession(ctx, sess)
	if err != nil {
		return nil, fmt.Errorf("legacy session %s: %w", sessionPath, err)
	}
	st.LIDLinks, st.Contacts = len(ids.LIDMap), len(ids.Contacts)
	payload, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	items := []store.RawItem{{Kind: store.RawLegacySession, Key: "legacy_session:" + hex.EncodeToString(sum[:]), Payload: payload}}

	msgs, err := sql.Open("sqlite", store.ReadOnlyDSN(messagesPath))
	if err != nil {
		return nil, err
	}
	defer msgs.Close()
	var chats []legacyChat
	rows, err := msgs.QueryContext(ctx, `SELECT jid, COALESCE(name, '') FROM chats`)
	if err != nil {
		return nil, fmt.Errorf("legacy chats: %w", err)
	}
	for rows.Next() {
		var c legacyChat
		if err := rows.Scan(&c.JID, &c.Name); err != nil {
			rows.Close()
			return nil, err
		}
		chats = append(chats, c)
	}
	rows.Close()
	st.LegacyChats = len(chats)
	for _, c := range chats {
		if j, err := types.ParseJID(c.JID); err == nil && (c.Name == "" || c.Name == j.User || fallbackName(c.Name)) {
			st.LegacyUnnamedChats++
		}
		b, _ := json.Marshal(c)
		items = append(items, store.RawItem{Kind: store.RawLegacyChat, Key: "legacy_chat:" + c.JID, Payload: b})
	}

	rows, err = msgs.QueryContext(ctx, `SELECT id, chat_jid, COALESCE(sender, ''), COALESCE(content, ''), timestamp,
		COALESCE(is_from_me, 0), COALESCE(media_type, ''), COALESCE(filename, ''), COALESCE(url, ''),
		media_key, file_sha256, file_enc_sha256, COALESCE(file_length, 0)
		FROM messages ORDER BY timestamp`)
	if err != nil {
		return nil, fmt.Errorf("legacy messages: %w", err)
	}
	for rows.Next() {
		var m legacyMessage
		if err := rows.Scan(&m.ID, &m.Chat, &m.Sender, &m.Text, &m.Timestamp, &m.FromMe, &m.MediaType, &m.Filename, &m.URL,
			&m.MediaKey, &m.SHA256, &m.EncSHA256, &m.Length); err != nil {
			rows.Close()
			return nil, err
		}
		st.LegacyMessages++
		b, _ := json.Marshal(m)
		items = append(items, store.RawItem{Kind: store.RawLegacyMessage, Key: "legacy:" + m.Chat + ":" + m.ID, Payload: b})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var chatsBefore, msgsBefore int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM chats), (SELECT COUNT(*) FROM messages)`).Scan(&chatsBefore, &msgsBefore); err != nil {
		return nil, err
	}
	now := time.Now()
	for i := 0; i < len(items); i += legacyBatch {
		n, err := db.ArchiveRawBatch(ctx, items[i:min(i+legacyBatch, len(items))], now)
		if err != nil {
			return nil, fmt.Errorf("archive legacy rows: %w", err)
		}
		st.ArchivedNew += n
	}
	in := &Ingestor{DB: db, Self: func() (types.JID, types.JID) { return ids.OwnPN, ids.OwnLID }}
	if _, err := in.ReplayPending(ctx); err != nil {
		return nil, err
	}

	// How the legacy DM JIDs map onto chats now (LID and phone of one person share one).
	dmChatOf := map[int64]int{}
	for _, c := range chats {
		j, err := types.ParseJID(c.JID)
		if err != nil || !people.IsUser(j) {
			continue
		}
		st.LegacyDMJIDs++
		chat, err := in.findChat(ctx, db, types.MessageSource{Chat: j})
		if err != nil {
			return nil, err
		}
		if chat.id != 0 {
			dmChatOf[chat.id]++
		}
	}
	for _, n := range dmChatOf {
		st.MergedDMChats += n - 1
	}
	if err := totals(ctx, db, st); err != nil {
		return nil, err
	}
	st.ChatsImported, st.MessagesImported = st.Chats-chatsBefore, st.Messages-msgsBefore
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events WHERE kind = 'legacy_message' AND error LIKE 'skipped:%'`).Scan(&st.MessagesSkipped); err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("%s: %d messages imported from %s", time.Now().Format(time.RFC3339), st.MessagesImported, messagesPath)
	return st, store.SetState(ctx, db, store.KeyImportLegacy, summary)
}

// legacyChat keeps a legacy chat's name when it is a real one: a group's
// name on its chat, a DM's name as a weak name of the person. A DM chat is
// created only by its first message.
func (in *Ingestor) legacyChat(ctx context.Context, tx *sql.Tx, c legacyChat) error {
	jid, err := types.ParseJID(c.JID)
	if err != nil {
		return nil
	}
	named := c.Name != "" && c.Name != jid.User && !fallbackName(c.Name)
	if people.IsUser(jid) {
		if !named {
			return nil
		}
		pid, err := in.person(ctx, tx, jid, types.JID{}, time.Time{})
		if err != nil {
			return err
		}
		return in.weakName(ctx, tx, pid, c.Name)
	}
	chat, err := in.chatFor(ctx, tx, types.MessageSource{Chat: jid}, time.Time{})
	if err != nil || chat.id == 0 || !named {
		return err
	}
	return store.SetChatName(ctx, tx, chat.id, c.Name)
}

// weakName keeps the legacy chat name as a push name when the person has no
// better name yet.
func (in *Ingestor) weakName(ctx context.Context, tx *sql.Tx, pid int64, name string) error {
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT name_source FROM persons WHERE id = ?`, pid).Scan(&source); err != nil {
		return err
	}
	if source != "" && source != people.SourcePhone {
		return nil
	}
	return people.SetNames(ctx, tx, pid, people.Names{PushName: name})
}

// errSkip marks an event that is deliberately not stored. It is recorded on
// the raw event but counts as ingested.
type errSkip string

func (e errSkip) Error() string { return "skipped: " + string(e) }

// legacyMessage stores one legacy row as a source=legacy message.
func (in *Ingestor) legacyMessage(ctx context.Context, tx *sql.Tx, m legacyMessage) error {
	jid, err := types.ParseJID(m.Chat)
	if err != nil {
		return errSkip("bad chat jid")
	}
	when, err := parseLegacyTime(m.Timestamp)
	if err != nil {
		return errSkip(err.Error())
	}
	kind := "text"
	if m.MediaType != "" {
		kind = m.MediaType
	}
	switch kind {
	case "text", "image", "video", "audio", "document", "sticker":
	default:
		return errSkip("unknown media type " + kind)
	}
	chat, err := in.chatFor(ctx, tx, types.MessageSource{Chat: jid}, time.Time{})
	if err != nil {
		return err
	}
	if chat.id == 0 {
		return errSkip("chat not stored")
	}
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jid, IsFromMe: m.FromMe, IsGroup: jid.Server == types.GroupServer},
		ID:            m.ID,
		Timestamp:     when,
	}
	var pid int64
	switch {
	case m.FromMe:
		if pid, err = in.me(ctx, tx); err != nil {
			return err
		}
		info.Sender, _ = in.self()
	case chat.kind == "dm":
		pid, info.Sender = chat.personID, jid.ToNonAD()
	case chat.kind == "group" && m.Sender != "":
		if info.Sender, err = legacySender(ctx, tx, m.Sender); err != nil {
			return err
		}
		if pid, err = in.person(ctx, tx, info.Sender, types.JID{}, time.Time{}); err != nil {
			return err
		}
	default:
		info.Sender = jid.ToNonAD()
	}
	c := content{Kind: kind, Text: m.Text}
	if m.MediaType != "" {
		c.Media = &mediaInfo{
			Mimetype: guessMime(m.MediaType, m.Filename), Filename: m.Filename, Size: uint64(m.Length), URL: m.URL,
			DirectPath: directPathFromURL(m.URL), MediaKey: m.MediaKey, FileSHA256: m.SHA256, FileEncSHA256: m.EncSHA256,
		}
	}
	_, err = in.upsert(ctx, tx, chat, pid, info, c, store.SourceLegacy, 0, true)
	return err
}

// legacySender turns the bare user part the old bridge stored into a JID.
// A known LID wins, then a known phone number; unknown values are phone
// numbers when they parse as one and LIDs otherwise.
func legacySender(ctx context.Context, e store.Execer, user string) (types.JID, error) {
	lid := types.NewJID(user, types.HiddenUserServer)
	pn := types.NewJID(user, types.DefaultUserServer)
	for _, j := range []types.JID{lid, pn} {
		id, err := people.Lookup(ctx, e, j)
		if err != nil {
			return types.JID{}, err
		}
		if id != 0 {
			return j, nil
		}
	}
	if people.IsPlausiblePhone(user) {
		return pn, nil
	}
	return lid, nil
}

func directPathFromURL(u string) string {
	if _, rest, ok := strings.Cut(u, ".net/"); ok {
		return "/" + rest
	}
	return ""
}

func guessMime(kind, filename string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); t != "" {
		return t
	}
	switch kind {
	case "image":
		return "image/jpeg"
	case "video":
		return "video/mp4"
	case "audio":
		return "audio/ogg; codecs=opus"
	}
	return ""
}

// totals counts what data.db holds, including chats still without a human
// name: DMs whose person is named only by number, and unnamed groups.
func totals(ctx context.Context, db *store.DB, st *LegacyStats) error {
	s, err := store.GetStats(ctx, db)
	if err != nil {
		return err
	}
	st.Persons, st.Chats, st.Messages, st.Unnamed = s.Persons, s.Chats, s.Messages, s.UnnamedChats
	return nil
}
