package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

type searchMessagesIn struct {
	Query          string `json:"query,omitempty" jsonschema:"words in message text, captions and voice-note transcripts: words are AND-ed, \"quoted phrases\" match in order, word* matches a prefix; case, accents and other punctuation are ignored. Optional when a filter is set"`
	From           string `json:"from,omitempty" jsonschema:"sender: p_N, phone number, part of a name, or me"`
	InChat         string `json:"in_chat,omitempty" jsonschema:"only this chat: c_N, p_N (their DM), phone number or exact name"`
	After          string `json:"after,omitempty" jsonschema:"only messages at or after this time"`
	Before         string `json:"before,omitempty" jsonschema:"only messages before this time"`
	HasMedia       string `json:"has_media,omitempty" jsonschema:"only messages with this media"`
	FromMe         *bool  `json:"from_me,omitempty" jsonschema:"true: only messages the user sent; false: only messages others sent"`
	Sort           string `json:"sort,omitempty" jsonschema:"default relevance with a query, newest without"`
	Context        int    `json:"context,omitempty" jsonschema:"messages of the same chat shown before and after each hit, 0-3, default 0"`
	Limit          int    `json:"limit,omitempty" jsonschema:"hits per page, 1-50, default 20 (10 with context)"`
	Cursor         string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page; it carries query, filters, sort and context"`
	ResponseFormat string `json:"response_format,omitempty" jsonschema:"default concise (long texts cut to an excerpt around the match); detailed gives texts up to 4,000 characters, WhatsApp ids, media details and who reacted"`
}

// Concise bodies: a hit is cut to an excerpt around the match, a context line shorter.
const (
	maxHitText     = 300
	maxContextText = 160
)

func (s *Server) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in searchMessagesIn) (*mcp.CallToolResult, error) {
	c := cursor{Tool: toolSearchMessages}
	var sender *store.PersonCandidate // the resolved from person on a first page
	if in.Cursor != "" {
		var err error
		if c, err = decodeCursor(in.Cursor, toolSearchMessages); err != nil {
			return errResult("%v", err)
		}
	} else {
		c.Query, c.Media, c.Sort, c.Ctx = strings.TrimSpace(in.Query), in.HasMedia, in.Sort, in.Context
		if in.FromMe != nil {
			c.Mine = 2
			if *in.FromMe {
				c.Mine = 1
			}
		}
		if from := strings.TrimSpace(in.From); strings.EqualFold(from, "me") {
			if c.Mine == 2 {
				return errResult("from=me contradicts from_me=false")
			}
			c.Mine = 1
		} else if from != "" {
			p, msg, err := s.resolvePerson(ctx, from, "from")
			if err != nil {
				return nil, err
			}
			if p == nil {
				return errResult("%s", msg)
			}
			c.Person, sender = p.ID, p
		}
		if in.InChat != "" {
			chat, msg := s.resolveChat(ctx, in.InChat)
			if chat == nil {
				return errResult("in_chat: %s", msg)
			}
			c.Chat = chat.ID
		}
		if in.After != "" {
			t, err := parseTime(in.After)
			if err != nil {
				return errResult("after: %v", err)
			}
			c.After = t.Unix()
		}
		if in.Before != "" {
			t, err := parseTime(in.Before)
			if err != nil {
				return errResult("before: %v", err)
			}
			c.Before = t.Unix()
		}
		if c.After != 0 && c.Before != 0 && c.After >= c.Before {
			return errResult("after (%s) must be earlier than before (%s)", in.After, in.Before)
		}
	}

	q := store.SearchQuery{PersonID: c.Person, ChatID: c.Chat, After: c.After, Before: c.Before, FromMe: c.Mine}
	switch c.Media {
	case "":
	case "any":
		q.Kinds = store.MediaKinds
	default:
		if !slices.Contains(store.MediaKinds, c.Media) {
			return errResult("has_media must be image, video, voice, audio, document, sticker or any, got %q", c.Media)
		}
		q.Kinds = []string{c.Media}
	}
	if c.Ctx < 0 || c.Ctx > 3 {
		return errResult("context must be between 0 and 3, got %d", c.Ctx)
	}
	switch c.Sort {
	case "", "relevance":
	case "newest":
		q.Newest = true
	default:
		return errResult("sort must be relevance or newest, got %q", c.Sort)
	}
	if c.Query != "" {
		if q.Match = store.FTSQuery(c.Query); q.Match == "" {
			return errResult("query %q has no searchable words: use letters or digits", c.Query)
		}
	} else if q.PersonID == 0 && q.ChatID == 0 && q.After == 0 && q.Before == 0 && q.Kinds == nil && q.FromMe == 0 {
		return errResult("give a query or at least one filter: from, in_chat, after, before, has_media or from_me")
	}
	ranked := q.Match != "" && !q.Newest

	limit := in.Limit
	if limit == 0 {
		limit = 20
		if c.Ctx > 0 {
			limit = 10
		}
	}
	if limit < 1 || limit > 50 {
		return errResult("limit must be between 1 and 50, got %d", in.Limit)
	}
	q.Limit = limit
	if ranked {
		q.Offset = c.Off
	} else if in.Cursor != "" {
		q.HasAnchor, q.AnchorTS, q.AnchorID = true, c.TS, c.ID
	}

	hits, more, err := store.SearchMessages(ctx, s.DB, q)
	simplified := false
	if store.IsFTSError(err) {
		// FTSQuery output is always valid; this guards against tokenizer surprises.
		q.Match, simplified = store.FTSPlainQuery(c.Query), true
		if q.Match == "" {
			return errResult("query %q has no searchable words: use letters or digits", c.Query)
		}
		hits, more, err = store.SearchMessages(ctx, s.DB, q)
	}
	if err != nil {
		return nil, err
	}
	total, err := store.CountMatches(ctx, s.DB, q)
	if err != nil {
		return nil, err
	}
	fresh, err := s.freshness(ctx, q.After, q.Before)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(hits)+1)
	if q.ChatID != 0 {
		ids = append(ids, q.ChatID)
	}
	for i := range hits {
		ids = append(ids, hits[i].ChatID)
	}
	chats, err := store.ChatsByID(ctx, s.DB, ids)
	if err != nil {
		return nil, err
	}
	w := newWrapper(s.now())
	full := detailed(in.ResponseFormat)
	if full {
		w.capText = maxDetailedText
	}

	var b strings.Builder
	b.WriteString(fresh + "\n")
	desc, err := s.describeSearch(ctx, w, c, q, sender, chats)
	if err != nil {
		return nil, err
	}
	b.WriteString(desc)
	if simplified {
		b.WriteString(" (query simplified to plain words)")
	}
	if len(hits) == 0 {
		if c.Off > 0 || q.HasAnchor {
			b.WriteString(": no more matches.")
		} else {
			b.WriteString(": no matches. Try fewer words, a prefix such as fattur*, or fewer filters.")
		}
		return textResult(b.String()), nil
	}
	order := "newest first"
	if ranked {
		order = "best match first"
	}
	matches := fmt.Sprintf("%d matches", total)
	if total == 1 {
		matches = "1 match"
	}
	fmt.Fprintf(&b, ": %s, %s", matches, order)
	if more || c.Off > 0 || q.HasAnchor {
		if ranked {
			fmt.Fprintf(&b, "; showing %d-%d", c.Off+1, c.Off+len(hits))
		} else {
			fmt.Fprintf(&b, "; showing %d", len(hits))
		}
	}
	b.WriteString(".\n")

	anchors := make([]store.Anchor, len(hits))
	for i := range hits {
		anchors[i] = store.Anchor{ChatID: hits[i].ChatID, TS: hits[i].TS, ID: hits[i].ID}
	}
	before, after, err := store.ReadAround(ctx, s.DB, anchors, c.Ctx)
	if err != nil {
		return nil, err
	}
	var lines []string
	for i := range hits {
		h := &hits[i]
		var chat *store.ChatRow
		if row, ok := chats[h.ChatID]; ok {
			chat = &row
		}
		lines = append(lines, w.inner().hitLines(i+1+c.Off, chat, h, before[i], after[i], full, c.Ctx > 0)...)
	}
	b.WriteString(w.block(lines) + "\n")
	if more {
		next := c
		if ranked {
			next.Off = c.Off + len(hits)
		} else {
			last := hits[len(hits)-1]
			next.TS, next.ID = last.TS, last.ID
		}
		fmt.Fprintf(&b, "More matches: call again with cursor=%q", next.encode())
	}
	return textResult(strings.TrimRight(b.String(), "\n")), nil
}

// describeSearch restates the query and the resolved filters, so a wrong
// person or chat pick is visible. sender is the from person when the caller
// already has it; chats holds the in_chat chat when it exists.
func (s *Server) describeSearch(ctx context.Context, w wrapper, c cursor, q store.SearchQuery, sender *store.PersonCandidate, chats map[int64]store.ChatRow) (string, error) {
	var parts []string
	if c.Query != "" {
		parts = append(parts, fmt.Sprintf("Search %q", c.Query))
	} else {
		parts = append(parts, "Messages")
	}
	if q.PersonID != 0 {
		p, found := store.PersonCandidate{}, false
		if sender != nil {
			p, found = *sender, true
		} else {
			var err error
			if p, found, err = store.PersonCandidateByID(ctx, s.DB, q.PersonID); err != nil {
				return "", err
			}
		}
		if found {
			parts = append(parts, "from "+personID(p.ID)+" "+w.label(&p.PersonRef))
		} else {
			parts = append(parts, "from "+personID(q.PersonID))
		}
	}
	switch q.FromMe {
	case 1:
		parts = append(parts, "sent by me")
	case 2:
		parts = append(parts, "sent by others")
	}
	if q.ChatID != 0 {
		if chat, found := chats[q.ChatID]; found {
			parts = append(parts, fmt.Sprintf("in %s %s (%s)", chatID(chat.ID), w.chatName(&chat), chat.Kind))
		} else {
			parts = append(parts, "in "+chatID(q.ChatID))
		}
	}
	if c.Media != "" {
		parts = append(parts, "with "+c.Media+" media")
	}
	if q.After != 0 {
		parts = append(parts, "after "+w.ts(q.After))
	}
	if q.Before != 0 {
		parts = append(parts, "before "+w.ts(q.Before))
	}
	return strings.Join(parts, " "), nil
}

// hitLines renders one hit, for a block: a header naming its chat and day,
// then the hit, with context messages around it marked apart from it.
func (w wrapper) hitLines(n int, chat *store.ChatRow, h *store.SearchHit, before, after []store.MessageRow, full, withContext bool) []string {
	name := "deleted chat"
	kind := ""
	if chat != nil {
		name, kind = w.chatName(chat), " ("+chat.Kind+")"
	}
	day := fmtDay(h.TS, w.now)
	lines := []string{fmt.Sprintf("#%d %s %s%s · %s", n, chatID(h.ChatID), name, kind, day)}
	ctxLine := func(m *store.MessageRow) {
		if d := fmtDay(m.TS, w.now); d != day {
			day = d
			lines = append(lines, "  ── "+d+" ──")
		}
		lines = append(lines, "  "+w.cutLine(m, full, maxContextText, "", ""))
	}
	for i := range before {
		ctxLine(&before[i])
	}
	prefix := ""
	if withContext {
		prefix = "> "
	}
	hw := w
	hw.transcripts = true // the hit may have matched in its transcript
	lines = append(lines, prefix+hw.cutLine(&h.MessageRow, full, maxHitText, h.TextExcerpt, h.TranscriptExcerpt))
	day = fmtDay(h.TS, w.now)
	for i := range after {
		ctxLine(&after[i])
	}
	return lines
}

// cutLine renders a transcript line; in concise mode a text or transcript
// over maxText runes becomes its excerpt (or its start), marked [cut N]. In
// detailed mode one over w.capText keeps the capText runes around its excerpt.
func (w wrapper) cutLine(m *store.MessageRow, full bool, maxText int, textExcerpt, transcriptExcerpt string) string {
	s := w.shown(m, full)
	if full {
		if s.textCut > 0 {
			s.text = window(m.Text, w.capText, textExcerpt)
		}
		if s.transcriptCut > 0 {
			s.transcript = window(m.Transcript(), w.capText, transcriptExcerpt)
		}
		return w.lineOf(m, full, s)
	}
	if r := []rune(m.Text); len(r) > maxText {
		s.text, s.textCut = textExcerpt, len(r)
		if s.text == "" || len([]rune(s.text)) >= len(r) {
			s.text = string(r[:maxText]) + "…"
		}
	}
	if r := []rune(s.transcript); len(r) > maxText {
		s.transcript, s.transcriptCut = transcriptExcerpt, len(r)
		if s.transcript == "" || len([]rune(s.transcript)) >= len(r) {
			s.transcript = string(r[:maxText]) + "…"
		}
	}
	return w.lineOf(m, false, s)
}

// window cuts s to n runes centred on excerpt, the match's FTS snippet ("…"
// where the snippet cut), or to its start when s does not hold the excerpt.
// "…" marks each end it cut.
func window(s string, n int, excerpt string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	start := 0
	if e := strings.Trim(excerpt, "…"); e != "" {
		if i := strings.Index(s, e); i >= 0 {
			mid := utf8.RuneCountInString(s[:i]) + utf8.RuneCountInString(e)/2
			start = min(max(mid-n/2, 0), len(r)-n)
		}
	}
	out := string(r[start : start+n])
	if start > 0 {
		out = "…" + out
	}
	if start+n < len(r) {
		out += "…"
	}
	return out
}
