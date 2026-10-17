package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"
)

// PersonCandidate is a person with every name candidate and the time of the
// last interaction, for people search.
type PersonCandidate struct {
	PersonRef
	Saved                                       bool
	FullName, FirstName, BusinessName, PushName string
	LastAt                                      int64 // newest DM message or message they sent, 0 when none
}

const candidateColumns = `p.id, p.display_name, p.name_source, COALESCE(p.phone_e164, ''), p.is_me, p.saved_contact,
	p.full_name, p.first_name, p.business_name, p.push_name,
	MAX(COALESCE((SELECT last_message_at FROM chats WHERE person_id = p.id AND kind = 'dm'), 0),
	    COALESCE((SELECT MAX(ts) FROM messages WHERE sender_person_id = p.id), 0))`

func queryCandidates(ctx context.Context, e Execer, where string, args ...any) ([]PersonCandidate, error) {
	rows, err := e.QueryContext(ctx, `SELECT `+candidateColumns+` FROM persons p WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonCandidate
	for rows.Next() {
		var c PersonCandidate
		if err := rows.Scan(&c.ID, &c.Name, &c.Source, &c.Phone, &c.IsMe, &c.Saved,
			&c.FullName, &c.FirstName, &c.BusinessName, &c.PushName, &c.LastAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AllPersonCandidates returns every person that has a name or a phone number.
func AllPersonCandidates(ctx context.Context, e Execer) ([]PersonCandidate, error) {
	return queryCandidates(ctx, e, `p.display_name != '' OR p.phone_e164 IS NOT NULL`)
}

// PersonCandidateByID returns one person; ok is false when it does not exist.
func PersonCandidateByID(ctx context.Context, e Execer, id int64) (PersonCandidate, bool, error) {
	cs, err := queryCandidates(ctx, e, `p.id = ?`, id)
	if err != nil || len(cs) == 0 {
		return PersonCandidate{}, false, err
	}
	return cs[0], true, nil
}

// PersonsByJID returns the persons owning exactly these JIDs (phone or LID).
func PersonsByJID(ctx context.Context, e Execer, jids ...string) ([]PersonCandidate, error) {
	if len(jids) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(jids)), ",")
	args := make([]any, len(jids))
	for i, j := range jids {
		args[i] = j
	}
	return queryCandidates(ctx, e, `p.id IN (SELECT person_id FROM jids WHERE jid IN (`+ph+`))`, args...)
}

// PersonsByPhone returns persons whose primary number is exactly +digits or
// who own the phone JID digits@s.whatsapp.net.
func PersonsByPhone(ctx context.Context, e Execer, digits string) ([]PersonCandidate, error) {
	return queryCandidates(ctx, e, `p.phone_e164 = ?1 OR p.id IN (SELECT person_id FROM jids WHERE jid = ?2)`,
		"+"+digits, digits+"@s.whatsapp.net")
}

// PersonsByFormerPhone returns persons whose LID once had the number +digits
// that another person holds now.
func PersonsByFormerPhone(ctx context.Context, e Execer, digits string) ([]PersonCandidate, error) {
	return queryCandidates(ctx, e, `p.id IN (SELECT jl.person_id FROM number_links n JOIN jids jl ON jl.jid = n.lid WHERE n.pn = ?1)
		AND p.id NOT IN (SELECT person_id FROM jids WHERE jid = ?1)`, digits+"@s.whatsapp.net")
}

// PersonsByPhonePart returns persons whose number contains digits.
func PersonsByPhonePart(ctx context.Context, e Execer, digits string) ([]PersonCandidate, error) {
	return queryCandidates(ctx, e, `p.phone_e164 LIKE ?1 OR p.id IN (SELECT person_id FROM jids WHERE kind = 'pn' AND jid LIKE ?1)`,
		"%"+digits+"%")
}

// JIDRow is one JID of a person.
type JIDRow struct {
	JID  string
	Kind string // pn | lid | other
}

// ChatActivity is a chat a person takes part in.
type ChatActivity struct {
	Chat     ChatRow
	Messages int   // messages in the DM; messages they wrote in a group
	LastAt   int64 // last message in the DM; their last message in a group
}

// FormerPhone is a number the person's LID had that another person holds now.
type FormerPhone struct {
	Phone string // +393331234567
	Owner int64  // the person holding it now
}

// PersonActivity is where a person talks and when they last did.
type PersonActivity struct {
	JIDs        []JIDRow
	Former      []FormerPhone // newest first
	DM          *ChatActivity
	Groups      []ChatActivity // most recent first, at most the requested number
	GroupsTotal int
	LastAt      int64
	LastChatID  int64
}

// GetPersonActivity returns the person's JIDs, former numbers, DM chat and
// the groups they wrote in, most recent first.
func GetPersonActivity(ctx context.Context, e Execer, personID int64, maxGroups int) (PersonActivity, error) {
	var a PersonActivity
	rows, err := e.QueryContext(ctx, `SELECT jid, kind FROM jids WHERE person_id = ? ORDER BY kind DESC, jid`, personID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var j JIDRow
		if err := rows.Scan(&j.JID, &j.Kind); err != nil {
			rows.Close()
			return a, err
		}
		a.JIDs = append(a.JIDs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, err
	}

	rows, err = e.QueryContext(ctx, `SELECT n.pn, jp.person_id FROM number_links n
		JOIN jids jl ON jl.jid = n.lid AND jl.person_id = ?1
		JOIN jids jp ON jp.jid = n.pn AND jp.person_id != ?1
		ORDER BY n.seen_at DESC, n.pn`, personID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var pn string
		var f FormerPhone
		if err := rows.Scan(&pn, &f.Owner); err != nil {
			rows.Close()
			return a, err
		}
		user, _, _ := strings.Cut(pn, "@")
		f.Phone = "+" + user
		a.Former = append(a.Former, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, err
	}

	dms, err := queryChats(ctx, e, `c.kind = 'dm' AND c.person_id = ?`, personID)
	if err != nil {
		return a, err
	}
	if len(dms) > 0 {
		dm := ChatActivity{Chat: dms[0], LastAt: dms[0].LastMessageAt}
		if err := e.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_id = ?`, dm.Chat.ID).Scan(&dm.Messages); err != nil {
			return a, err
		}
		a.DM = &dm
		a.LastAt, a.LastChatID = dm.LastAt, dm.Chat.ID
	}

	rows, err = e.QueryContext(ctx, `SELECT m.chat_id, COUNT(*), MAX(m.ts) FROM messages m JOIN chats c ON c.id = m.chat_id
		WHERE m.sender_person_id = ? AND c.kind != 'dm' GROUP BY m.chat_id ORDER BY MAX(m.ts) DESC, m.chat_id DESC`, personID)
	if err != nil {
		return a, err
	}
	var groups []ChatActivity
	for rows.Next() {
		var g ChatActivity
		if err := rows.Scan(&g.Chat.ID, &g.Messages, &g.LastAt); err != nil {
			rows.Close()
			return a, err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, err
	}
	a.GroupsTotal = len(groups)
	if len(groups) > 0 && groups[0].LastAt > a.LastAt {
		a.LastAt, a.LastChatID = groups[0].LastAt, groups[0].Chat.ID
	}
	if len(groups) > maxGroups {
		groups = groups[:maxGroups]
	}
	for i := range groups {
		c, ok, err := ChatByID(ctx, e, groups[i].Chat.ID)
		if err != nil {
			return a, err
		}
		if ok {
			groups[i].Chat = c
		}
	}
	a.Groups = groups
	return a, nil
}

// ---- message search ----

// FTSQuery turns free user input into an FTS5 MATCH expression that can never
// be a syntax error: every term becomes a double-quoted string holding only
// letters, numbers and spaces. Plain words are AND-ed, "quoted phrases" stay
// phrases, and a trailing * makes a prefix term. FTS operators (AND, OR, NOT,
// NEAR, column filters, parentheses, -) are taken as plain text. It returns
// "" when the input holds no searchable word.
func FTSQuery(input string) string {
	var terms []string
	add := func(term string, prefix bool) {
		words := ftsWords(term)
		if len(words) == 0 {
			return
		}
		t := `"` + strings.Join(words, " ") + `"`
		if prefix {
			t += "*"
		}
		terms = append(terms, t)
	}
	rs := []rune(input)
	for i := 0; i < len(rs); {
		switch {
		case unicode.IsSpace(rs[i]):
			i++
		case rs[i] == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			phrase := string(rs[i+1 : j])
			i = j + 1 // past the closing quote, or the end of an unbalanced phrase
			prefix := strings.HasSuffix(strings.TrimSpace(phrase), "*")
			if i < len(rs) && rs[i] == '*' {
				prefix = true
				i++
			}
			add(phrase, prefix)
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '"' {
				j++
			}
			word := string(rs[i:j])
			i = j
			add(word, strings.HasSuffix(word, "*"))
		}
	}
	return strings.Join(terms, " ")
}

// FTSPlainQuery is the fallback: every word AND-ed, no phrases, no prefixes.
func FTSPlainQuery(input string) string {
	words := ftsWords(input)
	for i, w := range words {
		words[i] = `"` + w + `"`
	}
	return strings.Join(words, " ")
}

// ftsWords splits s into runs of token characters (letters, numbers of any
// kind such as ² or ½, marks, private use), the characters FTS5's unicode61
// tokenizer indexes.
func ftsWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || unicode.Is(unicode.Co, r))
	})
}

// Message kinds that carry a downloadable file.
var MediaKinds = []string{"image", "video", "voice", "audio", "document", "sticker"}

// SearchQuery selects messages across chats.
type SearchQuery struct {
	Match    string // FTS5 expression from FTSQuery; "" searches by filters only
	PersonID int64  // sender
	ChatID   int64
	After    int64 // ts >= After when not 0
	Before   int64 // ts < Before when not 0
	Kinds    []string
	FromMe   int // 0 any, 1 only mine, 2 only others'
	Newest   bool
	// Paging: Offset for relevance order, the (AnchorTS, AnchorID) keyset for newest first.
	Offset             int
	HasAnchor          bool
	AnchorTS, AnchorID int64
	Limit              int
}

// SearchHit is a matching message with excerpts of its text and transcript
// around the match (empty without a Match).
type SearchHit struct {
	MessageRow
	TextExcerpt, TranscriptExcerpt string
}

func (q SearchQuery) where() (string, []any) {
	var where []string
	var args []any
	if q.Match != "" {
		where = append(where, "messages_fts MATCH ?")
		args = append(args, q.Match)
	}
	if q.PersonID != 0 {
		where = append(where, "m.sender_person_id = ?")
		args = append(args, q.PersonID)
	}
	if q.ChatID != 0 {
		where = append(where, "m.chat_id = ?")
		args = append(args, q.ChatID)
	}
	if q.After != 0 {
		where = append(where, "m.ts >= ?")
		args = append(args, q.After)
	}
	if q.Before != 0 {
		where = append(where, "m.ts < ?")
		args = append(args, q.Before)
	}
	if len(q.Kinds) > 0 {
		where = append(where, "m.kind IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Kinds)), ",")+")")
		for _, k := range q.Kinds {
			args = append(args, k)
		}
	}
	switch q.FromMe {
	case 1:
		where = append(where, "m.from_me = 1")
	case 2:
		where = append(where, "m.from_me = 0")
	}
	if len(where) == 0 {
		where = append(where, "1 = 1")
	}
	return strings.Join(where, " AND "), args
}

func (q SearchQuery) from() string {
	if q.Match != "" {
		return "FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid"
	}
	return "FROM messages m"
}

// IsFTSError reports a malformed MATCH expression.
func IsFTSError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "fts5") || strings.Contains(err.Error(), "syntax error"))
}

// CountMatches counts every message the query selects, ignoring paging.
func CountMatches(ctx context.Context, e Execer, q SearchQuery) (int, error) {
	where, args := q.where()
	var n int
	err := e.QueryRowContext(ctx, `SELECT COUNT(*) `+q.from()+` WHERE `+where, args...).Scan(&n)
	return n, err
}

// SearchMessages returns one page of matches, best match first (or newest
// first with Newest, and always without a Match). more is true when another
// page exists.
func SearchMessages(ctx context.Context, e Execer, q SearchQuery) (hits []SearchHit, more bool, err error) {
	where, args := q.where()
	excerpts := `'', ''`
	order := "m.ts DESC, m.id DESC"
	if q.Match != "" {
		excerpts = `snippet(messages_fts, 0, '', '', '…', 40), snippet(messages_fts, 1, '', '', '…', 40)`
		if !q.Newest {
			order = "bm25(messages_fts), m.ts DESC, m.id DESC"
		}
	}
	paging := ""
	if q.Match == "" || q.Newest {
		if q.HasAnchor {
			where += " AND (m.ts < ? OR (m.ts = ? AND m.id < ?))"
			args = append(args, q.AnchorTS, q.AnchorTS, q.AnchorID)
		}
	} else if q.Offset > 0 {
		paging = fmt.Sprintf(" OFFSET %d", q.Offset)
	}
	args = append(args, q.Limit+1)
	rows, err := e.QueryContext(ctx, `SELECT `+messageColumns+`, `+excerpts+` `+q.from()+` `+messageJoinsOnM+
		` WHERE `+where+` ORDER BY `+order+` LIMIT ?`+paging, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var h SearchHit
		m, err := scanMessage(scanTail{rows, []any{&h.TextExcerpt, &h.TranscriptExcerpt}})
		if err != nil {
			return nil, false, err
		}
		h.MessageRow = m
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(hits) > q.Limit {
		hits, more = hits[:q.Limit], true
	}
	msgs := make([]MessageRow, len(hits))
	for i := range hits {
		msgs[i] = hits[i].MessageRow
	}
	if err := attachReactions(ctx, e, msgs); err != nil {
		return nil, false, err
	}
	for i := range hits {
		hits[i].Reactions = msgs[i].Reactions
	}
	return hits, more, nil
}

// scanTail appends extra destinations after the message columns.
type scanTail struct {
	rows  *sql.Rows
	extra []any
}

func (s scanTail) Scan(dest ...any) error { return s.rows.Scan(append(dest, s.extra...)...) }
