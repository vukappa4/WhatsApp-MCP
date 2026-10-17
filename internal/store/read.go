package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// PersonRef is a person as shown next to chats and messages.
type PersonRef struct {
	ID     int64
	Name   string
	Source string // name_source
	Phone  string // E.164, may be empty for LID-only persons
	IsMe   bool
}

// ChatRow is one chat with its counterpart (dm) and newest message.
type ChatRow struct {
	ID            int64
	JID           string
	Kind          string
	Name          string // group/newsletter/broadcast name
	Person        *PersonRef
	LastMessageAt int64
	Unread        int
	Archived      bool
	MutedUntil    int64
	Last          *MessageRow
}

// MessageRow is one stored message with its sender, media and reactions.
type MessageRow struct {
	ID           int64
	ChatID       int64
	WAID         string
	Sender       *PersonRef
	SenderJID    string
	FromMe       bool
	TS           int64
	Kind         string
	Text         string
	ReplyToWAID  string
	ReplyToID    int64 // 0 when the quoted message is not stored
	ReplyPreview string
	ReplySender  *PersonRef
	EditedAt     int64
	DeletedAt    int64
	Forwarded    bool
	Media        *MediaRow
	Reactions    []ReactionRow
}

// Transcript is the media transcript, "" without media.
func (m *MessageRow) Transcript() string {
	if m.Media == nil {
		return ""
	}
	return m.Media.Transcript
}

// MediaRow is the downloadable part of a message.
type MediaRow struct {
	ID         int64
	Mimetype   string
	Filename   string
	Size       int64
	Duration   int64
	State      string
	LocalPath  string
	Transcript string
}

// ReactionRow is one person's reaction.
type ReactionRow struct {
	Emoji  string
	Person PersonRef
	TS     int64
}

func likeArg(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
	return "%" + s + "%"
}

func scanPerson(id sql.NullInt64, name, source, phone sql.NullString, isMe sql.NullBool) *PersonRef {
	if !id.Valid {
		return nil
	}
	return &PersonRef{ID: id.Int64, Name: name.String, Source: source.String, Phone: phone.String, IsMe: isMe.Bool}
}

const chatColumns = `c.id, c.jid, c.kind, c.name, c.unread_count, c.archived, COALESCE(c.muted_until, 0), COALESCE(c.last_message_at, 0),
	p.id, p.display_name, p.name_source, p.phone_e164, p.is_me`

func scanChat(rows interface{ Scan(...any) error }) (ChatRow, error) {
	var c ChatRow
	var pid sql.NullInt64
	var pname, psrc, pphone sql.NullString
	var pme sql.NullBool
	err := rows.Scan(&c.ID, &c.JID, &c.Kind, &c.Name, &c.Unread, &c.Archived, &c.MutedUntil, &c.LastMessageAt,
		&pid, &pname, &psrc, &pphone, &pme)
	c.Person = scanPerson(pid, pname, psrc, pphone, pme)
	return c, err
}

// ChatFilter selects chats for ListChats.
type ChatFilter struct {
	Kind       string // "", "dm", "group"
	UnreadOnly bool
	Query      string // matches chat name, person names, phone
	// Cursor: return chats strictly after (LastMessageAt, ID) in recency order.
	AfterTS, AfterID int64
	HasCursor        bool
	Limit            int
}

// ListChats returns chats by recency (newest first). more is true when
// another page exists.
func ListChats(ctx context.Context, e Execer, f ChatFilter) (chats []ChatRow, more bool, err error) {
	where := []string{"1 = 1"}
	var args []any
	switch f.Kind {
	case "dm", "group", "newsletter", "broadcast":
		where = append(where, fmt.Sprintf("c.kind = ?%d", len(args)+1))
		args = append(args, f.Kind)
	}
	if f.UnreadOnly {
		where = append(where, "c.unread_count > 0")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		cond := `(c.name LIKE ?1 ESCAPE '\' OR p.display_name LIKE ?1 ESCAPE '\' OR p.full_name LIKE ?1 ESCAPE '\'
			OR p.first_name LIKE ?1 ESCAPE '\' OR p.push_name LIKE ?1 ESCAPE '\' OR p.business_name LIKE ?1 ESCAPE '\'`
		digits := onlyDigits(q)
		if len(digits) >= 4 {
			cond += ` OR p.phone_e164 LIKE ?2`
		}
		cond += ")"
		// Number placeholders after the positional ones built so far, in one
		// pass: a renumbered ?1 must not be renumbered again as ?2.
		n := len(args)
		cond = strings.NewReplacer("?1", fmt.Sprintf("?%d", n+1), "?2", fmt.Sprintf("?%d", n+2)).Replace(cond)
		where = append(where, cond)
		args = append(args, likeArg(q), "%"+digits+"%")
	}
	if f.HasCursor {
		where = append(where, fmt.Sprintf(`(COALESCE(c.last_message_at, 0) < ?%d OR (COALESCE(c.last_message_at, 0) = ?%d AND c.id < ?%d))`,
			len(args)+1, len(args)+1, len(args)+2))
		args = append(args, f.AfterTS, f.AfterID)
	}
	args = append(args, f.Limit+1)
	q := fmt.Sprintf(`SELECT %s FROM chats c LEFT JOIN persons p ON p.id = c.person_id
		WHERE %s ORDER BY COALESCE(c.last_message_at, 0) DESC, c.id DESC LIMIT ?%d`, chatColumns, strings.Join(where, " AND "), len(args))
	rows, err := e.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, false, err
		}
		chats = append(chats, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(chats) > f.Limit {
		chats, more = chats[:f.Limit], true
	}
	return chats, more, attachLast(ctx, e, chats)
}

// attachLast sets each chat's Last in one query: the newest message at or
// before the LastMessageAt read with the chat, the one the shown time refers
// to, never a later poll vote (votes do not bump recency).
func attachLast(ctx context.Context, e Execer, chats []ChatRow) error {
	if len(chats) == 0 {
		return nil
	}
	idx := make(map[int64]int, len(chats))
	ph := make([]string, len(chats))
	args := make([]any, 0, 2*len(chats))
	for i, c := range chats {
		idx[c.ID] = i
		ph[i] = "(?, ?)"
		args = append(args, c.ID, c.LastMessageAt)
	}
	rows, err := e.QueryContext(ctx, `WITH page(chat_id, last_at) AS (VALUES `+strings.Join(ph, ", ")+`)
		SELECT `+messageColumns+` FROM page JOIN messages m ON m.id = (SELECT l.id FROM messages l
			WHERE l.chat_id = page.chat_id AND l.ts <= page.last_at ORDER BY l.ts DESC, l.id DESC LIMIT 1)
		`+messageJoinsOnM, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var msgs []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := attachReactions(ctx, e, msgs); err != nil {
		return err
	}
	for i := range msgs {
		chats[idx[msgs[i].ChatID]].Last = &msgs[i]
	}
	return nil
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ChatByID returns one chat; ok is false when it does not exist.
func ChatByID(ctx context.Context, e Execer, id int64) (ChatRow, bool, error) {
	row := e.QueryRowContext(ctx, `SELECT `+chatColumns+` FROM chats c LEFT JOIN persons p ON p.id = c.person_id WHERE c.id = ?`, id)
	c, err := scanChat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// FindChats looks chats up by exact identity: person id, phone digits, or an
// exact (case-insensitive) person or group name. Only chats that exist are returned.
func FindChats(ctx context.Context, e Execer, personID int64, phoneDigits, exactName string) ([]ChatRow, error) {
	var where string
	var args []any
	switch {
	case personID != 0:
		where, args = `c.kind = 'dm' AND c.person_id = ?`, []any{personID}
	case phoneDigits != "":
		where, args = `c.kind = 'dm' AND p.phone_e164 = ?`, []any{"+" + phoneDigits}
	default:
		where = `(c.kind != 'dm' AND c.name = ?1 COLLATE NOCASE) OR (c.kind = 'dm' AND (p.display_name = ?1 COLLATE NOCASE
			OR p.full_name = ?1 COLLATE NOCASE OR p.first_name = ?1 COLLATE NOCASE OR p.push_name = ?1 COLLATE NOCASE
			OR p.business_name = ?1 COLLATE NOCASE))`
		args = []any{exactName}
	}
	return queryChats(ctx, e, where, args...)
}

// SuggestChats returns up to 5 chats whose names contain s, for "did you mean" errors.
func SuggestChats(ctx context.Context, e Execer, s string) ([]ChatRow, error) {
	chats, _, err := ListChats(ctx, e, ChatFilter{Query: s, Limit: 5})
	return chats, err
}

func queryChats(ctx context.Context, e Execer, where string, args ...any) ([]ChatRow, error) {
	rows, err := e.QueryContext(ctx, `SELECT `+chatColumns+` FROM chats c LEFT JOIN persons p ON p.id = c.person_id
		WHERE `+where+` ORDER BY COALESCE(c.last_message_at, 0) DESC, c.id DESC LIMIT 20`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatRow
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MessageQuery selects a window of one chat's messages.
type MessageQuery struct {
	ChatID int64
	After  int64 // ts >= After when not 0
	Before int64 // ts < Before when not 0
	// Newer pages forward from the anchor (or After); otherwise pages backwards
	// from the anchor (or Before / the newest message).
	Newer     bool
	HasAnchor bool
	AnchorTS  int64
	AnchorID  int64
	Inclusive bool // include the anchor itself
	Limit     int
}

const messageColumns = `m.id, m.chat_id, m.wa_id, m.sender_jid, m.from_me, m.ts, m.kind, m.text,
	COALESCE(m.reply_to_wa_id, ''), COALESCE(m.reply_preview, ''), COALESCE(m.edited_at, 0), COALESCE(m.deleted_at, 0), m.forwarded,
	p.id, p.display_name, p.name_source, p.phone_e164, p.is_me,
	md.id, COALESCE(md.mimetype, ''), COALESCE(md.filename, ''), COALESCE(md.size, 0), COALESCE(md.duration, 0),
	COALESCE(md.state, ''), COALESCE(md.local_path, ''), COALESCE(md.transcript, ''),
	COALESCE(r.id, 0), rp.id, rp.display_name, rp.name_source, rp.phone_e164, rp.is_me`

const messageJoins = `FROM messages m ` + messageJoinsOnM

// messageJoinsOnM joins sender, media and quoted message onto a messages row aliased m.
const messageJoinsOnM = `LEFT JOIN persons p ON p.id = m.sender_person_id
	LEFT JOIN media md ON md.id = m.media_id
	LEFT JOIN messages r ON r.chat_id = m.chat_id AND r.wa_id = m.reply_to_wa_id
	LEFT JOIN persons rp ON rp.id = r.sender_person_id`

func scanMessage(rows interface{ Scan(...any) error }) (MessageRow, error) {
	var m MessageRow
	var pid, mid, rpid sql.NullInt64
	var pname, psrc, pphone, rpname, rpsrc, rpphone sql.NullString
	var pme, rpme sql.NullBool
	var md MediaRow
	err := rows.Scan(&m.ID, &m.ChatID, &m.WAID, &m.SenderJID, &m.FromMe, &m.TS, &m.Kind, &m.Text,
		&m.ReplyToWAID, &m.ReplyPreview, &m.EditedAt, &m.DeletedAt, &m.Forwarded,
		&pid, &pname, &psrc, &pphone, &pme,
		&mid, &md.Mimetype, &md.Filename, &md.Size, &md.Duration, &md.State, &md.LocalPath, &md.Transcript,
		&m.ReplyToID, &rpid, &rpname, &rpsrc, &rpphone, &rpme)
	m.Sender = scanPerson(pid, pname, psrc, pphone, pme)
	m.ReplySender = scanPerson(rpid, rpname, rpsrc, rpphone, rpme)
	if mid.Valid {
		md.ID = mid.Int64
		m.Media = &md
	}
	return m, err
}

// ReadMessages returns a window of messages in chronological order. more is
// true when further messages exist in the paging direction.
func ReadMessages(ctx context.Context, e Execer, q MessageQuery) (msgs []MessageRow, more bool, err error) {
	where := []string{"m.chat_id = ?"}
	args := []any{q.ChatID}
	if q.After != 0 {
		where = append(where, "m.ts >= ?")
		args = append(args, q.After)
	}
	if q.Before != 0 {
		where = append(where, "m.ts < ?")
		args = append(args, q.Before)
	}
	if q.HasAnchor {
		op := "<"
		if q.Newer {
			op = ">"
		}
		if q.Inclusive {
			op += "="
		}
		where = append(where, fmt.Sprintf("(m.ts %s ? OR (m.ts = ? AND m.id %s ?))", op[:1], op))
		args = append(args, q.AnchorTS, q.AnchorTS, q.AnchorID)
	}
	order := "DESC"
	if q.Newer {
		order = "ASC"
	}
	args = append(args, q.Limit+1)
	rows, err := e.QueryContext(ctx, fmt.Sprintf(`SELECT %s %s WHERE %s ORDER BY m.ts %s, m.id %s LIMIT ?`,
		messageColumns, messageJoins, strings.Join(where, " AND "), order, order), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, false, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(msgs) > q.Limit {
		msgs, more = msgs[:q.Limit], true
	}
	if !q.Newer {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}
	return msgs, more, attachReactions(ctx, e, msgs)
}

// Anchor is a message to read around: its chat and its (TS, ID) position.
type Anchor struct{ ChatID, TS, ID int64 }

// ReadAround returns, for each anchor, up to n messages of its chat before it
// and up to n after it, each in chronological order, the same windows
// ReadMessages gives from the anchor. It runs one query plus one reactions
// query for all anchors.
func ReadAround(ctx context.Context, e Execer, anchors []Anchor, n int) (before, after [][]MessageRow, err error) {
	before, after = make([][]MessageRow, len(anchors)), make([][]MessageRow, len(anchors))
	if len(anchors) == 0 || n <= 0 {
		return before, after, nil
	}
	ph := make([]string, len(anchors))
	args := make([]any, 0, 4*len(anchors)+1)
	for i, a := range anchors {
		ph[i] = "(?, ?, ?, ?)"
		args = append(args, i, a.ChatID, a.TS, a.ID)
	}
	args = append(args, n)
	window := func(dir int, op, order string) string {
		return fmt.Sprintf(`SELECT a.k, %d, m.id FROM a JOIN messages m ON m.id IN (SELECT x.id FROM messages x
			WHERE x.chat_id = a.chat_id AND (x.ts %s a.ts OR (x.ts = a.ts AND x.id %s a.id))
			ORDER BY x.ts %s, x.id %s LIMIT ?%d)`, dir, op, op, order, order, len(args))
	}
	type keyed struct {
		k, dir int
		m      MessageRow
	}
	var out []keyed
	err = eachRow(ctx, e, `WITH a(k, chat_id, ts, id) AS (VALUES `+strings.Join(ph, ", ")+`),
		w(k, dir, id) AS (`+window(0, "<", "DESC")+` UNION ALL `+window(1, ">", "ASC")+`)
		SELECT w.k, w.dir, `+messageColumns+` FROM w JOIN messages m ON m.id = w.id `+messageJoinsOnM, args,
		func(rows *sql.Rows) error {
			var r keyed
			m, err := scanMessage(scanHead{rows, []any{&r.k, &r.dir}})
			if err != nil {
				return err
			}
			r.m = m
			out = append(out, r)
			return nil
		})
	if err != nil {
		return nil, nil, err
	}
	// A message can sit in several windows: load its reactions once.
	uniq := make([]MessageRow, 0, len(out))
	seen := make(map[int64]int, len(out))
	for _, r := range out {
		if _, ok := seen[r.m.ID]; !ok {
			seen[r.m.ID] = len(uniq)
			uniq = append(uniq, r.m)
		}
	}
	if err := attachReactions(ctx, e, uniq); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(out, func(x, y keyed) int {
		return cmp.Or(cmp.Compare(x.m.TS, y.m.TS), cmp.Compare(x.m.ID, y.m.ID))
	})
	for _, r := range out {
		r.m.Reactions = uniq[seen[r.m.ID]].Reactions
		if r.dir == 0 {
			before[r.k] = append(before[r.k], r.m)
		} else {
			after[r.k] = append(after[r.k], r.m)
		}
	}
	return before, after, nil
}

// scanHead puts leading columns into extra before the message columns.
type scanHead struct {
	rows  *sql.Rows
	extra []any
}

func (s scanHead) Scan(dest ...any) error { return s.rows.Scan(append(s.extra, dest...)...) }

// ChatsByID returns the chats with these ids that exist, by id.
func ChatsByID(ctx context.Context, e Execer, ids []int64) (map[int64]ChatRow, error) {
	out := make(map[int64]ChatRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	in, args := idList(ids)
	err := eachRow(ctx, e, `SELECT `+chatColumns+` FROM chats c LEFT JOIN persons p ON p.id = c.person_id WHERE c.id IN (`+in+`)`, args,
		func(rows *sql.Rows) error {
			c, err := scanChat(rows)
			if err != nil {
				return err
			}
			out[c.ID] = c
			return nil
		})
	return out, err
}

// MessageByID returns one message; ok is false when it does not exist.
func MessageByID(ctx context.Context, e Execer, id int64) (MessageRow, bool, error) {
	row := e.QueryRowContext(ctx, `SELECT `+messageColumns+` `+messageJoins+` WHERE m.id = ?`, id)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

func attachReactions(ctx context.Context, e Execer, msgs []MessageRow) error {
	if len(msgs) == 0 {
		return nil
	}
	idx := make(map[int64]int, len(msgs))
	ph := make([]string, len(msgs))
	args := make([]any, len(msgs))
	for i, m := range msgs {
		idx[m.ID] = i
		ph[i] = "?"
		args[i] = m.ID
	}
	rows, err := e.QueryContext(ctx, `SELECT r.message_id, r.emoji, r.ts, p.id, p.display_name, p.name_source, COALESCE(p.phone_e164, ''), p.is_me
		FROM reactions r JOIN persons p ON p.id = r.person_id
		WHERE r.message_id IN (`+strings.Join(ph, ",")+`) ORDER BY r.ts`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var mid int64
		var r ReactionRow
		if err := rows.Scan(&mid, &r.Emoji, &r.TS, &r.Person.ID, &r.Person.Name, &r.Person.Source, &r.Person.Phone, &r.Person.IsMe); err != nil {
			return err
		}
		i := idx[mid]
		msgs[i].Reactions = append(msgs[i].Reactions, r)
	}
	return rows.Err()
}

// Stats summarizes what data.db holds.
type Stats struct {
	Persons, Chats, Messages, Media int
	OldestTS, NewestTS              int64
	UnnamedChats                    int
}

// GetStats counts persons, chats and messages and the covered time range.
func GetStats(ctx context.Context, e Execer) (Stats, error) {
	var s Stats
	err := e.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM persons), (SELECT COUNT(*) FROM chats), (SELECT COUNT(*) FROM messages), (SELECT COUNT(*) FROM media),
		COALESCE((SELECT MIN(ts) FROM messages), 0), COALESCE((SELECT MAX(ts) FROM messages), 0),
		(SELECT COUNT(*) FROM chats c LEFT JOIN persons p ON p.id = c.person_id
			WHERE ((c.kind = 'dm' AND p.name_source IN ('', 'phone')) OR (c.kind != 'dm' AND c.name = ''))
			AND EXISTS (SELECT 1 FROM messages m WHERE m.chat_id = c.id))`).
		Scan(&s.Persons, &s.Chats, &s.Messages, &s.Media, &s.OldestTS, &s.NewestTS, &s.UnnamedChats)
	return s, err
}
