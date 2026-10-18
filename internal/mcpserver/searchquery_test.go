package mcpserver

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// searchRef is one message in the reference order of a search.
type searchRef struct {
	id, ts                         int64
	score                          float64
	textExcerpt, transcriptExcerpt string
}

// referenceSearch selects every message q matches in one plain query, in the
// order store.SearchMessages promises, with the excerpts it should return.
func referenceSearch(t *testing.T, db store.Execer, q store.SearchQuery) []searchRef {
	t.Helper()
	cols, from, order := `m.id, m.ts, 0, '', ''`, `FROM messages m`, `m.ts DESC, m.id DESC`
	var where []string
	var args []any
	if q.Match != "" {
		cols = `m.id, m.ts, bm25(messages_fts), snippet(messages_fts, 0, '', '', '…', 40), snippet(messages_fts, 1, '', '', '…', 40)`
		from = `FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid`
		where, args = append(where, `messages_fts MATCH ?`), append(args, q.Match)
		if !q.Newest {
			order = `bm25(messages_fts), m.ts DESC, m.id DESC`
		}
	}
	add := func(cond string, arg ...any) { where, args = append(where, cond), append(args, arg...) }
	if q.PersonID != 0 {
		add(`m.sender_person_id = ?`, q.PersonID)
	}
	if q.ChatID != 0 {
		add(`m.chat_id = ?`, q.ChatID)
	}
	if q.After != 0 {
		add(`m.ts >= ?`, q.After)
	}
	if q.Before != 0 {
		add(`m.ts < ?`, q.Before)
	}
	if len(q.Kinds) > 0 {
		kinds := make([]any, len(q.Kinds))
		for i, k := range q.Kinds {
			kinds[i] = k
		}
		add(`m.kind IN (`+strings.TrimSuffix(strings.Repeat(`?,`, len(kinds)), `,`)+`)`, kinds...)
	}
	switch q.FromMe {
	case 1:
		add(`m.from_me = 1`)
	case 2:
		add(`m.from_me = 0`)
	}
	if len(where) == 0 {
		where = []string{`1 = 1`}
	}
	rows, err := db.QueryContext(context.Background(), `SELECT `+cols+` `+from+` WHERE `+strings.Join(where, ` AND `)+` ORDER BY `+order, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []searchRef
	for rows.Next() {
		var r searchRef
		if err := rows.Scan(&r.id, &r.ts, &r.score, &r.textExcerpt, &r.transcriptExcerpt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSearchMessagesMatchesReference walks every page of many searches on the
// fixture and checks each hit, its excerpts, its order and the more flag
// against one plain query, and each hit's message against MessageByID.
func TestSearchMessagesMatchesReference(t *testing.T) {
	fx, _ := fixtureDB(t)
	ctx := context.Background()
	// Ties go into a transaction that is rolled back, so the shared fixture
	// stays as it is.
	db, err := fx.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Rollback()
	one := func(query string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := db.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	fattura := store.FTSQuery("fattura")
	// Copies of the third best and of the newest match tie with their source
	// on score and time: only the id orders them, across page boundaries.
	byScore := referenceSearch(t, db, store.SearchQuery{Match: fattura})
	byTime := referenceSearch(t, db, store.SearchQuery{Match: fattura, Newest: true})
	for i, src := range []int64{byScore[2].id, byScore[2].id, byScore[2].id, byTime[0].id, byTime[0].id} {
		if _, err := db.ExecContext(ctx, `INSERT INTO messages (chat_id, wa_id, sender_jid, sender_person_id, from_me, ts, kind, text)
			SELECT chat_id, ?, sender_jid, sender_person_id, from_me, ts, kind, text FROM messages WHERE id = ?`, fmt.Sprintf("FXSEARCHTIE%d", i), src); err != nil {
			t.Fatal(err)
		}
	}
	chat := one(`SELECT m.chat_id FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid WHERE messages_fts MATCH ?
		GROUP BY m.chat_id ORDER BY COUNT(*) DESC, m.chat_id LIMIT 1`, fattura)
	person := one(`SELECT m.sender_person_id FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid WHERE messages_fts MATCH ?
		AND m.sender_person_id IS NOT NULL GROUP BY m.sender_person_id ORDER BY COUNT(*) DESC, 1 LIMIT 1`, fattura)
	minTS, maxTS := one(`SELECT MIN(ts) FROM messages`), one(`SELECT MAX(ts) FROM messages`)
	mid := minTS + (maxTS-minTS)/2
	var transcript string
	if err = db.QueryRowContext(ctx, `SELECT md.transcript FROM messages m JOIN media md ON md.id = m.media_id
		WHERE m.kind = 'voice' AND md.transcript != '' ORDER BY m.id LIMIT 1`).Scan(&transcript); err != nil {
		t.Fatal(err)
	}
	voiceWord := ""
	for _, w := range strings.Fields(transcript) {
		if len([]rune(w)) >= 4 {
			voiceWord = w
			break
		}
	}

	type tc struct {
		name  string
		q     store.SearchQuery
		limit []int
	}
	var cases []tc
	for _, newest := range []bool{false, true} {
		for _, m := range []string{fattura, store.FTSQuery("preventivo domani"), store.FTSQuery("fatt*"), store.FTSQuery(`"` + voiceWord + `"`)} {
			cases = append(cases,
				tc{"all", store.SearchQuery{Match: m, Newest: newest}, []int{50, 7}},
				tc{"chat", store.SearchQuery{Match: m, Newest: newest, ChatID: chat}, []int{1, 3}},
				tc{"person", store.SearchQuery{Match: m, Newest: newest, PersonID: person}, []int{2, 20}},
				tc{"range", store.SearchQuery{Match: m, Newest: newest, After: mid, Before: maxTS}, []int{20}},
				tc{"voice", store.SearchQuery{Match: m, Newest: newest, Kinds: []string{"voice"}}, []int{4}},
				tc{"media", store.SearchQuery{Match: m, Newest: newest, Kinds: store.MediaKinds}, []int{8}},
				tc{"mine", store.SearchQuery{Match: m, Newest: newest, FromMe: 1}, []int{5}},
				tc{"theirs", store.SearchQuery{Match: m, Newest: newest, FromMe: 2, Before: mid}, []int{9}},
			)
		}
	}
	// Small pages in the chats of the copies cut through their ties.
	for _, src := range []int64{byScore[2].id, byTime[0].id} {
		tieChat := one(`SELECT chat_id FROM messages WHERE id = ?`, src)
		cases = append(cases,
			tc{"ties", store.SearchQuery{Match: fattura, ChatID: tieChat}, []int{1, 2}},
			tc{"ties/newest", store.SearchQuery{Match: fattura, ChatID: tieChat, Newest: true}, []int{1, 2}},
		)
	}
	cases = append(cases,
		tc{"filters/person", store.SearchQuery{PersonID: person}, []int{1, 10}},
		tc{"filters/voice", store.SearchQuery{Kinds: []string{"voice"}, After: mid}, []int{6}},
		tc{"filters/media", store.SearchQuery{Kinds: store.MediaKinds, FromMe: 2}, []int{25}},
	)

	var hits, ties, idTies, reacted, quoted, transcripts, pages int
	for _, c := range cases {
		ref := referenceSearch(t, db, c.q)
		if n, err := store.CountMatches(ctx, db, c.q); err != nil || n != len(ref) {
			t.Fatalf("%s %+v: CountMatches %d (err %v), want %d", c.name, c.q, n, err, len(ref))
		}
		for i := 1; i < len(ref); i++ {
			if c.q.Match != "" && !c.q.Newest && ref[i].score == ref[i-1].score {
				ties++
			}
			if ref[i].score == ref[i-1].score && ref[i].ts == ref[i-1].ts {
				idTies++
			}
		}
		for _, limit := range c.limit {
			q := c.q
			q.Limit = limit
			ranked := q.Match != "" && !q.Newest
			for off := 0; ; off += limit {
				// Check the first four pages and the last two.
				if off >= 4*limit && off < len(ref)-2*limit {
					off = (len(ref) - 2*limit) / limit * limit
				}
				if ranked {
					q.Offset = off
				} else if q.HasAnchor = off > 0; q.HasAnchor {
					q.AnchorTS, q.AnchorID = ref[off-1].ts, ref[off-1].id
				}
				got, more, err := store.SearchMessages(ctx, db, q)
				if err != nil {
					t.Fatalf("%s limit %d off %d: %v", c.name, limit, off, err)
				}
				pages++
				want := ref[min(off, len(ref)):min(off+limit, len(ref))]
				if len(got) != len(want) || more != (off+limit < len(ref)) {
					t.Fatalf("%s %+v off %d: %d hits more %v, want %d more %v", c.name, q, off, len(got), more, len(want), off+limit < len(ref))
				}
				for i, h := range got {
					w := want[i]
					if h.ID != w.id || h.TextExcerpt != w.textExcerpt || h.TranscriptExcerpt != w.transcriptExcerpt {
						t.Fatalf("%s %+v off %d hit %d: m_%d %q %q, want m_%d %q %q", c.name, q, off, i,
							h.ID, h.TextExcerpt, h.TranscriptExcerpt, w.id, w.textExcerpt, w.transcriptExcerpt)
					}
					m, ok, err := store.MessageByID(ctx, db, h.ID)
					if err != nil || !ok {
						t.Fatalf("MessageByID(%d): %v %v", h.ID, ok, err)
					}
					row := h.MessageRow
					if len(row.Reactions) > 0 {
						reacted++
					}
					row.Reactions = nil
					if !reflect.DeepEqual(row, m) {
						t.Fatalf("%s hit m_%d:\n got %+v\nwant %+v", c.name, h.ID, row, m)
					}
					if m.ReplyToID != 0 {
						quoted++
					}
					if h.TranscriptExcerpt != "" {
						transcripts++
					}
					hits++
				}
				if !more {
					break
				}
			}
		}
	}
	t.Logf("%d cases, %d pages, %d hits, %d score ties, %d id ties, %d with reactions, %d quoting, %d transcript excerpts",
		len(cases), pages, hits, ties, idTies, reacted, quoted, transcripts)
	if ties == 0 || idTies == 0 || reacted == 0 || quoted == 0 || transcripts == 0 || hits < 1000 {
		t.Fatalf("the cases miss a shape: %d hits, %d ties, %d id ties, %d reacted, %d quoted, %d transcripts",
			hits, ties, idTies, reacted, quoted, transcripts)
	}
}

// TestFTSRowsMatchMessages checks the invariant CountMatches relies on to
// count a bare MATCH without joining messages: exactly one messages_fts row
// per message, with rowid = messages.id, after every kind of write the
// triggers handle.
func TestFTSRowsMatchMessages(t *testing.T) {
	fx, _ := fixtureDB(t)
	ctx := context.Background()
	db, err := fx.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Rollback()
	check := func(step string) {
		t.Helper()
		var messages, rows, orphans, missing int
		if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM messages), (SELECT COUNT(*) FROM messages_fts),
			(SELECT COUNT(*) FROM messages_fts f WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = f.rowid)),
			(SELECT COUNT(*) FROM messages m WHERE NOT EXISTS (SELECT 1 FROM messages_fts f WHERE f.rowid = m.id))`).
			Scan(&messages, &rows, &orphans, &missing); err != nil {
			t.Fatal(err)
		}
		if messages == 0 || rows != messages || orphans != 0 || missing != 0 {
			t.Fatalf("%s: %d messages, %d fts rows, %d without a message, %d messages without a row", step, messages, rows, orphans, missing)
		}
	}
	check("fixture")
	for _, s := range []struct{ step, q string }{
		{"insert", `INSERT INTO messages (chat_id, wa_id, sender_jid, sender_person_id, from_me, ts, kind, text)
			SELECT chat_id, 'FXFTSCOPY' || id, sender_jid, sender_person_id, from_me, ts, kind, text FROM messages WHERE id % 50 = 0`},
		{"edit", `UPDATE messages SET text = 'fxedited ' || text WHERE id % 7 = 0`},
		{"media", `UPDATE messages SET media_id = (SELECT MIN(id) FROM media) WHERE id IN (SELECT id FROM messages WHERE media_id IS NULL LIMIT 5)`},
		{"detach", `UPDATE messages SET media_id = NULL WHERE id IN (SELECT id FROM messages WHERE media_id IS NOT NULL LIMIT 5)`},
		{"transcript", `UPDATE media SET transcript = 'fxtranscript' WHERE id IN (SELECT media_id FROM messages WHERE media_id IS NOT NULL LIMIT 20)`},
		{"delete", `DELETE FROM messages WHERE id % 11 = 0`},
	} {
		res, err := db.ExecContext(ctx, s.q)
		if err != nil {
			t.Fatalf("%s: %v", s.step, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			t.Fatalf("%s changed no rows", s.step)
		}
		check(s.step)
	}
}
