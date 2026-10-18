package store

import (
	"context"
	"database/sql"
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

const nameColumns = `p.id, p.display_name, p.name_source, COALESCE(p.phone_e164, ''), p.is_me, p.saved_contact,
	p.full_name, p.first_name, p.business_name, p.push_name`

// lastAtColumn is PersonCandidate.LastAt of the person p.
const lastAtColumn = `MAX(COALESCE((SELECT last_message_at FROM chats WHERE person_id = p.id AND kind = 'dm'), 0),
	    COALESCE((SELECT MAX(ts) FROM messages WHERE sender_person_id = p.id), 0))`

func queryCandidates(ctx context.Context, e Execer, where string, args ...any) ([]PersonCandidate, error) {
	return queryPersons(ctx, e, nameColumns+`, `+lastAtColumn, where, args...)
}

// queryPersons scans nameColumns and, when columns has it, lastAtColumn.
func queryPersons(ctx context.Context, e Execer, columns, where string, args ...any) ([]PersonCandidate, error) {
	rows, err := e.QueryContext(ctx, `SELECT `+columns+` FROM persons p WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonCandidate
	withLastAt := strings.HasSuffix(columns, lastAtColumn)
	for rows.Next() {
		var c PersonCandidate
		if err := scanCandidate(rows, &c, withLastAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanCandidate scans nameColumns, then lastAtColumn when withLastAt, after
// the destinations in lead.
func scanCandidate(rows *sql.Rows, c *PersonCandidate, withLastAt bool, lead ...any) error {
	dest := append(lead, &c.ID, &c.Name, &c.Source, &c.Phone, &c.IsMe, &c.Saved,
		&c.FullName, &c.FirstName, &c.BusinessName, &c.PushName)
	if withLastAt {
		dest = append(dest, &c.LastAt)
	}
	return rows.Scan(dest...)
}

// AllPersonNames returns every person that has a name or a phone number,
// without LastAt: name search scans them all and needs LastAt only for the
// matches, which FillLastAt adds.
func AllPersonNames(ctx context.Context, e Execer) ([]PersonCandidate, error) {
	return queryPersons(ctx, e, nameColumns, `p.display_name != '' OR p.phone_e164 IS NOT NULL`)
}

// FillLastAt sets LastAt on every candidate. A short query can match most
// of a large account, so it asks in chunks well below SQLite's limit of
// 32766 variables per statement.
func FillLastAt(ctx context.Context, e Execer, cs []PersonCandidate) error {
	const chunk = 500
	for len(cs) > 0 {
		part := cs[:min(len(cs), chunk)]
		cs = cs[len(part):]
		ids := make([]int64, len(part))
		for i := range part {
			ids[i] = part[i].ID
		}
		in, args := idList(ids)
		last := make(map[int64]int64, len(part))
		err := eachRow(ctx, e, `SELECT p.id, `+lastAtColumn+` FROM persons p WHERE p.id IN (`+in+`)`, args, func(rows *sql.Rows) error {
			var id, at int64
			err := rows.Scan(&id, &at)
			last[id] = at
			return err
		})
		if err != nil {
			return err
		}
		for i := range part {
			part[i].LastAt = last[part[i].ID]
		}
	}
	return nil
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

// NumberMatch says how a person matched a whole number. The values are the
// literals PersonsByNumber selects.
type NumberMatch int

const (
	MatchPhone  NumberMatch = 1 // the primary number or a phone JID
	MatchFormer NumberMatch = 2 // a number their LID once had that another person holds now
	MatchLID    NumberMatch = 3 // the LID
)

// NumberCandidate is a person matched by a whole number.
type NumberCandidate struct {
	PersonCandidate
	Match NumberMatch
}

// PersonsByNumber matches a whole number in one query: persons whose
// primary number is +digits or who own the phone JID digits@s.whatsapp.net
// (MatchPhone), persons whose LID once had that number while another person
// holds it now (MatchFormer), and the owner of the LID lid@lid (MatchLID).
// A person appears once per way it matches.
func PersonsByNumber(ctx context.Context, e Execer, digits, lid string) ([]NumberCandidate, error) {
	cols := nameColumns + `, ` + lastAtColumn
	rows, err := e.QueryContext(ctx, `SELECT 1, `+cols+` FROM persons p
		WHERE p.phone_e164 = ?1 OR p.id IN (SELECT person_id FROM jids WHERE jid = ?2)
		UNION ALL SELECT 2, `+cols+` FROM persons p
		WHERE p.id IN (SELECT jl.person_id FROM number_links n JOIN jids jl ON jl.jid = n.lid WHERE n.pn = ?2)
		AND p.id NOT IN (SELECT person_id FROM jids WHERE jid = ?2)
		UNION ALL SELECT 3, `+cols+` FROM persons p
		WHERE p.id IN (SELECT person_id FROM jids WHERE jid = ?3)`,
		"+"+digits, digits+"@s.whatsapp.net", lid+"@lid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NumberCandidate
	for rows.Next() {
		var c NumberCandidate
		if err := scanCandidate(rows, &c.PersonCandidate, true, &c.Match); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
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
	as, err := GetPeopleActivity(ctx, e, []int64{personID}, maxGroups)
	return as[personID], err
}

// GetPeopleActivity is GetPersonActivity for many persons in a fixed number
// of queries. Persons without any activity are missing from the map.
func GetPeopleActivity(ctx context.Context, e Execer, personIDs []int64, maxGroups int) (map[int64]PersonActivity, error) {
	out := make(map[int64]PersonActivity, len(personIDs))
	if len(personIDs) == 0 {
		return out, nil
	}
	in, args := idList(personIDs)
	// Map values are not addressable: work on pointers, copy out at the end.
	acts := make(map[int64]*PersonActivity, len(personIDs))
	act := func(id int64) *PersonActivity {
		a := acts[id]
		if a == nil {
			a = &PersonActivity{}
			acts[id] = a
		}
		return a
	}

	err := eachRow(ctx, e, `SELECT person_id, jid, kind FROM jids WHERE person_id IN (`+in+`) ORDER BY person_id, kind DESC, jid`, args,
		func(rows *sql.Rows) error {
			var id int64
			var j JIDRow
			if err := rows.Scan(&id, &j.JID, &j.Kind); err != nil {
				return err
			}
			a := act(id)
			a.JIDs = append(a.JIDs, j)
			return nil
		})
	if err != nil {
		return nil, err
	}

	err = eachRow(ctx, e, `SELECT jl.person_id, n.pn, jp.person_id FROM number_links n
		JOIN jids jl ON jl.jid = n.lid
		JOIN jids jp ON jp.jid = n.pn AND jp.person_id != jl.person_id
		WHERE jl.person_id IN (`+in+`) ORDER BY jl.person_id, n.seen_at DESC, n.pn`, args,
		func(rows *sql.Rows) error {
			var id int64
			var pn string
			var f FormerPhone
			if err := rows.Scan(&id, &pn, &f.Owner); err != nil {
				return err
			}
			user, _, _ := strings.Cut(pn, "@")
			f.Phone = "+" + user
			a := act(id)
			a.Former = append(a.Former, f)
			return nil
		})
	if err != nil {
		return nil, err
	}

	err = eachRow(ctx, e, `SELECT `+chatColumns+`, c.person_id, (SELECT COUNT(*) FROM messages WHERE chat_id = c.id)
		FROM chats c LEFT JOIN persons p ON p.id = c.person_id WHERE c.kind = 'dm' AND c.person_id IN (`+in+`)`, args,
		func(rows *sql.Rows) error {
			var id int64
			var dm ChatActivity
			c, err := scanChat(scanTail{rows, []any{&id, &dm.Messages}})
			if err != nil {
				return err
			}
			dm.Chat, dm.LastAt = c, c.LastMessageAt
			a := act(id)
			a.DM = &dm
			a.LastAt, a.LastChatID = dm.LastAt, c.ID
			return nil
		})
	if err != nil {
		return nil, err
	}

	var chatIDs []int64
	err = eachRow(ctx, e, `SELECT m.sender_person_id, m.chat_id, COUNT(*), MAX(m.ts) FROM messages m JOIN chats c ON c.id = m.chat_id
		WHERE m.sender_person_id IN (`+in+`) AND c.kind != 'dm'
		GROUP BY m.sender_person_id, m.chat_id ORDER BY m.sender_person_id, MAX(m.ts) DESC, m.chat_id DESC`, args,
		func(rows *sql.Rows) error {
			var id int64
			var g ChatActivity
			if err := rows.Scan(&id, &g.Chat.ID, &g.Messages, &g.LastAt); err != nil {
				return err
			}
			a := act(id)
			if a.GroupsTotal == 0 && g.LastAt > a.LastAt {
				a.LastAt, a.LastChatID = g.LastAt, g.Chat.ID
			}
			a.GroupsTotal++
			if len(a.Groups) < maxGroups {
				a.Groups = append(a.Groups, g)
				chatIDs = append(chatIDs, g.Chat.ID)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	if len(chatIDs) > 0 {
		in, args := idList(chatIDs)
		chats := map[int64]ChatRow{}
		err = eachRow(ctx, e, `SELECT `+chatColumns+` FROM chats c LEFT JOIN persons p ON p.id = c.person_id WHERE c.id IN (`+in+`)`, args,
			func(rows *sql.Rows) error {
				c, err := scanChat(rows)
				chats[c.ID] = c
				return err
			})
		if err != nil {
			return nil, err
		}
		for _, a := range acts {
			for i := range a.Groups {
				if c, ok := chats[a.Groups[i].Chat.ID]; ok {
					a.Groups[i].Chat = c
				}
			}
		}
	}
	for id, a := range acts {
		out[id] = *a
	}
	return out, nil
}

// idList returns the placeholders and arguments for "IN (...)" over ids.
func idList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// eachRow runs a query and calls fn for every row.
func eachRow(ctx context.Context, e Execer, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := e.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
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
// With only a Match it counts the index alone: the triggers keep exactly
// one messages_fts row per message, with rowid = messages.id.
func CountMatches(ctx context.Context, e Execer, q SearchQuery) (int, error) {
	where, args := q.where()
	from := q.from()
	if where == "messages_fts MATCH ?" {
		from = "FROM messages_fts"
	}
	var n int
	err := e.QueryRowContext(ctx, `SELECT COUNT(*) `+from+` WHERE `+where, args...).Scan(&n)
	return n, err
}

// SearchMessages returns one page of matches, best match first (or newest
// first with Newest, and always without a Match). more is true when another
// page exists.
func SearchMessages(ctx context.Context, e Execer, q SearchQuery) (hits []SearchHit, more bool, err error) {
	where, args := q.where()
	paging := ""
	if q.Match == "" || q.Newest {
		if q.HasAnchor {
			where += " AND (m.ts < ? OR (m.ts = ? AND m.id < ?))"
			args = append(args, q.AnchorTS, q.AnchorTS, q.AnchorID)
		}
	} else if q.Offset > 0 {
		paging = " OFFSET ?"
	}
	args = append(args, q.Limit+1)
	if paging != "" {
		args = append(args, q.Offset)
	}
	// SQLite runs the joins for every match but computes result columns
	// only for rows that enter the LIMIT's top-N sort. So the page is picked
	// on ids, times and scores, and sender, media and quote are joined for
	// its rows only. Excerpts come from a second walk of the matches, kept
	// only for the page: computed in the pick they cost up to twice the
	// pick itself (in newest order nearly every match enters the sort, since
	// FTS walks matches by id, which mostly rises with time). The walk is
	// materialized on its own: inlined into the page query, SQLite scans the
	// whole page for every match instead of seeking it.
	var query string
	if q.Match == "" {
		query = `SELECT ` + messageColumns + `, '', '' FROM messages m ` + messageJoinsOnM +
			` WHERE ` + where + ` ORDER BY m.ts DESC, m.id DESC LIMIT ?`
	} else {
		pick, order, pageOrder := `m.id, m.ts`, `m.ts DESC, m.id DESC`, `hit.ts DESC, hit.id DESC`
		if !q.Newest {
			pick, order, pageOrder = `m.id, m.ts, bm25(messages_fts) AS score`, `score, `+order, `hit.score, `+pageOrder
		}
		query = `WITH hit AS (SELECT ` + pick + ` ` + q.from() + ` WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ?` + paging + `),
			excerpt AS MATERIALIZED (SELECT hit.id, snippet(messages_fts, 0, '', '', '…', 40) AS text, snippet(messages_fts, 1, '', '', '…', 40) AS transcript
				FROM messages_fts CROSS JOIN hit WHERE messages_fts MATCH ? AND hit.id = messages_fts.rowid)
			SELECT ` + messageColumns + `, excerpt.text, excerpt.transcript FROM hit CROSS JOIN excerpt CROSS JOIN messages m ` + messageJoinsOnM +
			` WHERE excerpt.id = hit.id AND m.id = hit.id ORDER BY ` + pageOrder
		args = append(args, q.Match)
	}
	rows, err := e.QueryContext(ctx, query, args...)
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
