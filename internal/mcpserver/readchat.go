package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

type readChatIn struct {
	Chat           string `json:"chat,omitempty" jsonschema:"c_N, p_N (their DM), phone number such as +39 333 1234567, or exact person or group name; optional with around or cursor"`
	After          string `json:"after,omitempty" jsonschema:"only messages at or after this time"`
	Before         string `json:"before,omitempty" jsonschema:"only messages before this time"`
	Around         string `json:"around,omitempty" jsonschema:"m_N: the messages around it"`
	Limit          int    `json:"limit,omitempty" jsonschema:"messages per page, 1-100, default 30"`
	Cursor         string `json:"cursor,omitempty" jsonschema:"older_cursor or newer_cursor of a previous call; it carries chat and bounds"`
	ResponseFormat string `json:"response_format,omitempty" jsonschema:"default concise; detailed adds WhatsApp ids, reply previews, media details and who reacted"`
}

// resolveChat turns the chat argument into one chat, or an actionable error.
func (s *Server) resolveChat(ctx context.Context, arg string) (*store.ChatRow, string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, "chat is required: pass a c_N chat id, p_N person id, phone number or exact name (see whatsapp_list_chats)"
	}
	if id, ok := parseID(arg, "c_"); ok {
		c, found, err := store.ChatByID(ctx, s.DB, id)
		if err != nil {
			return nil, err.Error()
		}
		if !found {
			return nil, fmt.Sprintf("no chat %s: it may have been merged into another chat; call whatsapp_list_chats to get current ids", arg)
		}
		return &c, ""
	}
	var cands []store.ChatRow
	var err error
	what := ""
	if id, ok := parseID(arg, "p_"); ok {
		cands, err = store.FindChats(ctx, s.DB, id, "", "")
		what = "person " + arg
	} else if digits := phoneDigits(arg); digits != "" {
		cands, err = store.FindChats(ctx, s.DB, 0, digits, "")
		what = "phone " + arg
	} else {
		cands, err = store.FindChats(ctx, s.DB, 0, "", arg)
		what = fmt.Sprintf("name %q", arg)
	}
	if err != nil {
		return nil, err.Error()
	}
	w := newWrapper(s.now())
	switch len(cands) {
	case 1:
		return &cands[0], ""
	case 0:
		sugg, err := store.SuggestChats(ctx, s.DB, arg)
		if err != nil {
			return nil, err.Error()
		}
		msg := fmt.Sprintf("No chat found for %s.", what)
		if len(sugg) > 0 {
			msg += " Similar chats:\n" + candidateList(w, sugg) + "Call again with chat set to one of these ids."
		} else {
			msg += " Use whatsapp_list_chats with a query to find it."
		}
		return nil, msg
	}
	return nil, fmt.Sprintf("%s matches %d chats:\n%sCall again with chat set to one of these ids.", what, len(cands), candidateList(w, cands))
}

func candidateList(w wrapper, chats []store.ChatRow) string {
	lines := make([]string, len(chats))
	for i := range chats {
		c := &chats[i]
		lines[i] = fmt.Sprintf("- %s %s (%s, last message %s)", chatID(c.ID), w.inner().chatName(c), c.Kind, orNever(w.ts(c.LastMessageAt)))
	}
	return w.block(lines) + "\n"
}

// phoneDigits returns the digits of s when s looks like a phone number.
func phoneDigits(s string) string {
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return ""
		}
	}
	if digits < 6 {
		return ""
	}
	d := ""
	for _, r := range s {
		if r >= '0' && r <= '9' {
			d += string(r)
		}
	}
	if strings.HasPrefix(d, "00") {
		d = d[2:]
	}
	if !strings.HasPrefix(strings.TrimSpace(s), "+") && !strings.HasPrefix(strings.TrimSpace(s), "00") && len(d) <= 10 {
		d = "39" + d // a national Italian number
	}
	return d
}

func (s *Server) readChat(ctx context.Context, _ *mcp.CallToolRequest, in readChatIn) (*mcp.CallToolResult, error) {
	limit := in.Limit
	if limit == 0 {
		limit = 30
	}
	if limit < 1 || limit > maxReadLimit {
		return errResult("limit must be between 1 and %d, got %d", maxReadLimit, in.Limit)
	}
	q := store.MessageQuery{Limit: limit}
	var aroundMsg *store.MessageRow
	var chat *store.ChatRow

	switch {
	case in.Cursor != "":
		c, err := decodeCursor(in.Cursor, toolReadChat)
		if err != nil {
			return errResult("%v", err)
		}
		q.ChatID, q.After, q.Before, q.Newer = c.Chat, c.After, c.Before, c.Newer
		q.HasAnchor, q.AnchorTS, q.AnchorID = true, c.TS, c.ID
	case in.Around != "":
		id, ok := parseID(in.Around, "m_")
		if !ok {
			return errResult("around must be a message id like m_8812, got %q", in.Around)
		}
		m, found, err := store.MessageByID(ctx, s.DB, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return errResult("no message %s", in.Around)
		}
		aroundMsg = &m
		q.ChatID = m.ChatID
	default:
		c, msg := s.resolveChat(ctx, in.Chat)
		if c == nil {
			return errResult("%s", msg)
		}
		chat = c
		q.ChatID = c.ID
		if in.After != "" {
			t, err := parseTime(in.After)
			if err != nil {
				return errResult("after: %v", err)
			}
			q.After = t.Unix()
		}
		if in.Before != "" {
			t, err := parseTime(in.Before)
			if err != nil {
				return errResult("before: %v", err)
			}
			q.Before = t.Unix()
		}
		if q.After != 0 && q.Before != 0 && q.After >= q.Before {
			return errResult("after (%s) must be earlier than before (%s)", in.After, in.Before)
		}
		// With only a start time, read forward from it; otherwise show the newest messages.
		q.Newer = q.After != 0
	}
	if chat == nil {
		c, found, err := store.ChatByID(ctx, s.DB, q.ChatID)
		if err != nil {
			return nil, err
		}
		if !found {
			return errResult("the chat of this cursor or message no longer exists; call whatsapp_list_chats")
		}
		chat = &c
	}

	var msgs []store.MessageRow
	var err error
	anchor := -1 // index of the around message in msgs
	// Whether messages lie beyond each end of the page, within the bounds.
	// A read knows it for the side it reads towards. Without an anchor, the
	// other side is empty: the read starts at the bound or at the newest
	// message. Only a cursor's other side needs a probe.
	var olderMore, newerMore bool
	olderKnown, newerKnown := false, false
	if aroundMsg != nil {
		half := (limit + 1) / 2
		older, more, err := store.ReadMessages(ctx, s.DB, store.MessageQuery{ChatID: q.ChatID, Limit: half,
			HasAnchor: true, AnchorTS: aroundMsg.TS, AnchorID: aroundMsg.ID, Inclusive: true})
		if err != nil {
			return nil, err
		}
		olderMore, olderKnown = more, true
		newer, more, err := store.ReadMessages(ctx, s.DB, store.MessageQuery{ChatID: q.ChatID, Limit: limit - len(older),
			Newer: true, HasAnchor: true, AnchorTS: aroundMsg.TS, AnchorID: aroundMsg.ID})
		if err != nil {
			return nil, err
		}
		newerMore, newerKnown = more, true
		anchor = len(older) - 1
		msgs = append(older, newer...)
	} else {
		var more bool
		if msgs, more, err = store.ReadMessages(ctx, s.DB, q); err != nil {
			return nil, err
		}
		if q.Newer {
			newerMore, newerKnown, olderKnown = more, true, !q.HasAnchor
		} else {
			olderMore, olderKnown, newerKnown = more, true, !q.HasAnchor
		}
	}

	// Render, then keep the response under its budget: messages beyond it
	// are left to the cursor (the oldest ones when reading back from the
	// newest, the newest ones when reading forward or around a message).
	w := newWrapper(s.now())
	full := detailed(in.ResponseFormat)
	if full && limit > 1 {
		w.capText = maxDetailedText
	}
	lines := make([]string, len(msgs))
	size := 0
	for i := range msgs {
		lines[i] = w.inner().line(&msgs[i], full)
		size += len(lines[i]) + 1
	}
	left := 0
	for size > readChatBudget && len(msgs) > 1 {
		left++
		// Around a message: drop from the longer side, never the message itself.
		fromEnd := q.Newer
		if aroundMsg != nil {
			fromEnd = len(msgs)-1-anchor >= anchor
		}
		if fromEnd {
			size -= len(lines[len(lines)-1]) + 1
			msgs, lines = msgs[:len(msgs)-1], lines[:len(lines)-1]
			newerMore, newerKnown = true, true
		} else {
			size -= len(lines[0]) + 1
			msgs, lines = msgs[1:], lines[1:]
			anchor--
			olderMore, olderKnown = true, true
		}
	}

	// Cursors in both directions, bounded like this request.
	var olderCur, newerCur string
	if len(msgs) > 0 {
		first, last := msgs[0], msgs[len(msgs)-1]
		bounds := store.MessageQuery{ChatID: q.ChatID, After: q.After, Before: q.Before, Limit: 0, HasAnchor: true}
		if !olderKnown {
			probe := bounds
			probe.AnchorTS, probe.AnchorID = first.TS, first.ID
			if _, olderMore, err = store.ReadMessages(ctx, s.DB, probe); err != nil {
				return nil, err
			}
		}
		if olderMore {
			olderCur = cursor{Tool: toolReadChat, Chat: q.ChatID, TS: first.TS, ID: first.ID, After: q.After, Before: q.Before}.encode()
		}
		if !newerKnown {
			probe := bounds
			probe.Newer, probe.AnchorTS, probe.AnchorID = true, last.TS, last.ID
			if _, newerMore, err = store.ReadMessages(ctx, s.DB, probe); err != nil {
				return nil, err
			}
		}
		if newerMore {
			newerCur = cursor{Tool: toolReadChat, Chat: q.ChatID, Newer: true, TS: last.TS, ID: last.ID, After: q.After, Before: q.Before}.encode()
		}
	}

	// Where a cursor leads on, the range ends at the message shown. Where
	// none does, it runs to the requested bound, or to now: a gap between
	// that bound and the messages may hide some.
	from, to := q.After, q.Before
	if len(msgs) > 0 {
		if olderCur != "" || from == 0 {
			from = msgs[0].TS
		}
		if newerCur != "" {
			to = msgs[len(msgs)-1].TS
		}
	}
	fresh, err := s.freshness(ctx, from, to)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString(fresh + "\n")
	fmt.Fprintf(&b, "Chat %s: %s (%s)", chatID(chat.ID), w.chatName(chat), chat.Kind)
	if chat.Person != nil && !chat.Person.IsMe {
		fmt.Fprintf(&b, ", person %s", personID(chat.Person.ID))
	}
	if len(msgs) == 0 {
		b.WriteString("\nNo messages in this range.")
		if q.After != 0 || q.Before != 0 {
			b.WriteString(" Widen after/before or drop them to see the latest messages.")
		}
		return textResult(b.String()), nil
	}
	fmt.Fprintf(&b, ". %d messages, %s to %s.\n", len(msgs), w.ts(msgs[0].TS), w.ts(msgs[len(msgs)-1].TS))
	if left > 0 {
		fmt.Fprintf(&b, "%d more messages of this page left out to keep the answer under about 20k tokens: page with the cursors.\n", left)
	}
	if olderCur != "" {
		fmt.Fprintf(&b, "Older messages: cursor=%q\n", olderCur)
	}

	day := ""
	rows := make([]string, 0, len(msgs)+9)
	if chat.Kind != "dm" {
		if sp := w.inner().participants(msgs); sp != "" {
			rows = append(rows, sp)
		}
	}
	for i := range msgs {
		if d := fmtDay(msgs[i].TS, w.now); d != day {
			day = d
			rows = append(rows, "── "+d+" ──")
		}
		rows = append(rows, lines[i])
	}
	b.WriteString(w.block(rows) + "\n")
	if newerCur != "" {
		fmt.Fprintf(&b, "Newer messages: cursor=%q", newerCur)
	}
	return textResult(strings.TrimRight(b.String(), "\n")), nil
}

// participants lists the people speaking in a group window once, so message
// lines can use bare names: the first line of the block.
func (w wrapper) participants(msgs []store.MessageRow) string {
	seen := map[int64]bool{}
	var parts []string
	for i := range msgs {
		p := msgs[i].Sender
		if p == nil || p.IsMe || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		parts = append(parts, personID(p.ID)+" "+w.label(p))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Speakers: " + strings.Join(parts, "; ")
}

// shown is what a line shows of a message's text and transcript, with the
// full length in runes of each one it cut (0 when whole).
type shown struct {
	text, transcript       string
	textCut, transcriptCut int
}

// shown cuts the text to textCap runes, and the transcript too when the line
// shows it: in detailed mode, or when w.transcripts asks for it.
func (w wrapper) shown(m *store.MessageRow, full bool) shown {
	var s shown
	s.text = m.Text
	if r, max := []rune(m.Text), w.textCap(full); max > 0 && len(r) > max {
		s.text, s.textCut = string(r[:max])+"…", len(r)
	}
	if t := m.Transcript(); t != "" && (full || w.transcripts) {
		s.transcript = t
		if r := []rune(t); w.capText > 0 && len(r) > w.capText {
			s.transcript, s.transcriptCut = string(r[:w.capText])+"…", len(r)
		}
	}
	return s
}

// line renders one transcript line, for a block.
func (w wrapper) line(m *store.MessageRow, full bool) string {
	return w.lineOf(m, full, w.shown(m, full))
}

// lineOf renders a line showing s: the time, id and sender, the markers, then
// the text, which runs to the end of the line. [cut N] marks a text shown in
// part, N runes long.
func (w wrapper) lineOf(m *store.MessageRow, full bool, s shown) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s ", clock(local(m.TS)), messageID(m.ID))
	b.WriteString(w.sender(m) + ":")
	if m.Forwarded {
		b.WriteString(" [forwarded]")
	}
	if m.ReplyToWAID != "" {
		switch {
		case m.ReplyToID != 0 && !full:
			fmt.Fprintf(&b, " [reply to %s]", messageID(m.ReplyToID))
		case m.ReplyToID != 0:
			who := "me"
			if m.ReplySender != nil && !m.ReplySender.IsMe {
				who = w.name(m.ReplySender)
			}
			fmt.Fprintf(&b, " [reply to %s by %s: %s]", messageID(m.ReplyToID), who, w.wrap(clip(m.ReplyPreview, 80)))
		default:
			fmt.Fprintf(&b, " [reply to: %s]", w.wrap(clip(m.ReplyPreview, 80)))
		}
	}
	if m.Kind != "text" {
		b.WriteString(" " + w.mediaTag(m, full))
	}
	if m.DeletedAt != 0 {
		b.WriteString(" [deleted by sender; original shown]")
	}
	if m.EditedAt != 0 {
		if full {
			fmt.Fprintf(&b, " [edited %s]", w.ts(m.EditedAt))
		} else {
			b.WriteString(" [edited]")
		}
	}
	if len(m.Reactions) > 0 {
		b.WriteString(" " + w.reactions(m.Reactions, full))
	}
	if full {
		fmt.Fprintf(&b, " (wa_id %s", w.waID(m.WAID))
		if m.SenderJID != "" {
			fmt.Fprintf(&b, ", from %s", m.SenderJID)
		}
		b.WriteString(")")
	}
	text, tr := w.wrap(s.text), w.wrap(s.transcript)
	if text != "" && tr != "" { // audio carries no caption: never seen, kept apart if it comes
		text += " transcript " + tr
	} else if tr != "" {
		text = tr
	}
	// One cut marker: limit=1 in detailed mode shows the text and the
	// transcript whole.
	if n := max(s.textCut, s.transcriptCut); n > 0 && text != "" {
		fmt.Fprintf(&b, " [cut %d]", n)
	}
	if text != "" {
		b.WriteString(" " + text)
	} else if m.Kind == "text" && m.DeletedAt != 0 {
		b.WriteString(" (text not available)")
	}
	return b.String()
}

const (
	// maxConciseText caps a message body in concise transcripts.
	maxConciseText = 800
	// maxDetailedText caps a message body or transcript in a detailed
	// transcript of more than one message; limit=1 shows it whole.
	maxDetailedText = 4000
	// maxReadLimit is the most messages one read_chat page holds.
	maxReadLimit = 100
	// readChatBudget bounds one read_chat answer in characters (about 20k tokens).
	readChatBudget = 70000
)

// textCap is the most runes of a body shown: maxConciseText in concise
// mode, w.capText (0: no cap) in detailed mode.
func (w wrapper) textCap(full bool) int {
	if !full {
		return maxConciseText
	}
	return w.capText
}

// mediaTag renders "[voice 0:42 · transcript]". The line shows the transcript
// as its text in detailed mode, or when w.transcripts asks for it (a search
// hit); otherwise "transcript" only says there is one.
func (w wrapper) mediaTag(m *store.MessageRow, full bool) string {
	head := m.Kind
	var parts []string
	if md := m.Media; md != nil {
		if md.Duration > 0 {
			head += fmt.Sprintf(" %d:%02d", md.Duration/60, md.Duration%60)
		}
		if md.Filename != "" {
			parts = append(parts, w.wrap(md.Filename))
		}
		if full {
			if sz := humanSize(md.Size); sz != "" {
				parts = append(parts, sz)
			}
			if mt := safeMime(md.Mimetype); mt != "" {
				parts = append(parts, mt)
			}
			parts = append(parts, "media "+md.State)
		}
		if md.Transcript != "" {
			parts = append(parts, "transcript")
		}
	}
	return "[" + strings.Join(append([]string{head}, parts...), " · ") + "]"
}

func (w wrapper) reactions(rs []store.ReactionRow, full bool) string {
	var parts []string
	if full {
		for _, r := range rs {
			who := "me"
			if !r.Person.IsMe {
				p := r.Person
				who = w.name(&p)
			}
			parts = append(parts, w.reactionText(r.Emoji)+" "+who)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	counts := map[string]int{}
	var order []string
	for _, r := range rs {
		e := w.reactionText(r.Emoji)
		if counts[e] == 0 {
			order = append(order, e)
		}
		counts[e]++
	}
	for _, e := range order {
		if counts[e] > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", e, counts[e]))
		} else {
			parts = append(parts, e)
		}
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// reactionText renders a reaction: bare when it is an emoji, quoted like any
// sender text otherwise. WhatsApp sends a reaction as free text, so a sender
// can react with words.
func (w wrapper) reactionText(s string) string {
	s = sanitize(s)
	if isEmoji(s) {
		return s
	}
	return w.wrap(s)
}

// isEmoji reports whether s is a short run of emoji: pictographs, regional
// indicators, skin tones, joiners, variation selectors and keycaps (0-9, #
// or * right before U+20E3). Symbols that read as letters or punctuation
// (circled and squared letters, Braille, box drawing, quote ornaments) are
// not emoji, so a sender cannot spell words with them.
func isEmoji(s string) bool {
	r := []rune(s)
	if len(r) == 0 || len(r) > 16 {
		return false
	}
	seen := false
	for i := 0; i < len(r); i++ {
		switch c := r[i]; {
		case unicode.Is(emoji, c):
			seen = true
		case c == 0x200D, c == 0xFE0E, c == 0xFE0F:
		case c >= '0' && c <= '9', c == '#', c == '*':
			j := i + 1
			if j < len(r) && r[j] == 0xFE0F {
				j++
			}
			if j == len(r) || r[j] != 0x20E3 {
				return false
			}
			i, seen = j, true
		default:
			return false
		}
	}
	return seen
}

// emoji holds the pictographs WhatsApp offers as emoji, skin tones and
// regional indicators included.
var emoji = &unicode.RangeTable{
	R16: []unicode.Range16{
		{0x00A9, 0x00A9, 1}, {0x00AE, 0x00AE, 1}, {0x203C, 0x203C, 1}, {0x2049, 0x2049, 1},
		{0x2122, 0x2122, 1}, {0x2139, 0x2139, 1}, {0x2194, 0x2199, 1}, {0x21A9, 0x21AA, 1},
		{0x231A, 0x231B, 1}, {0x2328, 0x2328, 1}, {0x23CF, 0x23CF, 1}, {0x23E9, 0x23F3, 1},
		{0x23F8, 0x23FA, 1}, {0x24C2, 0x24C2, 1}, {0x25AA, 0x25AB, 1}, {0x25B6, 0x25B6, 1},
		{0x25C0, 0x25C0, 1}, {0x25FB, 0x25FE, 1}, {0x2600, 0x2757, 1}, {0x2763, 0x2767, 1},
		{0x2795, 0x2797, 1}, {0x27A1, 0x27A1, 1}, {0x27B0, 0x27B0, 1}, {0x27BF, 0x27BF, 1},
		{0x2934, 0x2935, 1}, {0x2B05, 0x2B07, 1}, {0x2B1B, 0x2B1C, 1}, {0x2B50, 0x2B50, 1},
		{0x2B55, 0x2B55, 1}, {0x3030, 0x3030, 1}, {0x303D, 0x303D, 1}, {0x3297, 0x3297, 1},
		{0x3299, 0x3299, 1},
	},
	R32: []unicode.Range32{
		{0x1F004, 0x1F004, 1}, {0x1F0CF, 0x1F0CF, 1}, {0x1F170, 0x1F171, 1}, {0x1F17E, 0x1F17F, 1},
		{0x1F18E, 0x1F18E, 1}, {0x1F191, 0x1F19A, 1}, {0x1F1E6, 0x1F1FF, 1}, {0x1F201, 0x1F202, 1},
		{0x1F21A, 0x1F21A, 1}, {0x1F22F, 0x1F22F, 1}, {0x1F232, 0x1F23A, 1}, {0x1F250, 0x1F251, 1},
		{0x1F300, 0x1F64F, 1}, {0x1F680, 0x1F6FF, 1}, {0x1F7E0, 0x1F7EB, 1}, {0x1F7F0, 0x1F7F0, 1},
		{0x1F900, 0x1F9FF, 1}, {0x1FA70, 0x1FAFF, 1},
	},
}
