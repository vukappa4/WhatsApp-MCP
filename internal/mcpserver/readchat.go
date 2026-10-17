package mcpserver

import (
	"context"
	"fmt"
	"strings"

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
	w := newWrapper()
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
	var b strings.Builder
	for i := range chats {
		c := &chats[i]
		fmt.Fprintf(&b, "- %s %s (%s, last message %s)\n", chatID(c.ID), w.chatName(c), c.Kind, orNever(fmtTS(c.LastMessageAt)))
	}
	return b.String()
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
	if aroundMsg != nil {
		half := (limit + 1) / 2
		older, _, err := store.ReadMessages(ctx, s.DB, store.MessageQuery{ChatID: q.ChatID, Limit: half,
			HasAnchor: true, AnchorTS: aroundMsg.TS, AnchorID: aroundMsg.ID, Inclusive: true})
		if err != nil {
			return nil, err
		}
		newer, _, err := store.ReadMessages(ctx, s.DB, store.MessageQuery{ChatID: q.ChatID, Limit: limit - len(older),
			Newer: true, HasAnchor: true, AnchorTS: aroundMsg.TS, AnchorID: aroundMsg.ID})
		if err != nil {
			return nil, err
		}
		anchor = len(older) - 1
		msgs = append(older, newer...)
	} else if msgs, _, err = store.ReadMessages(ctx, s.DB, q); err != nil {
		return nil, err
	}

	// Render, then keep the response under its budget: messages beyond it
	// are left to the cursor (the oldest ones when reading back from the
	// newest, the newest ones when reading forward or around a message).
	w := newWrapper()
	full := detailed(in.ResponseFormat)
	if full && limit > 1 {
		w.capText = maxDetailedText
	}
	lines := make([]string, len(msgs))
	size := 0
	for i := range msgs {
		lines[i] = w.line(&msgs[i], full)
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
		} else {
			size -= len(lines[0]) + 1
			msgs, lines = msgs[1:], lines[1:]
			anchor--
		}
	}

	// Cursors in both directions, bounded like this request.
	var olderCur, newerCur string
	if len(msgs) > 0 {
		first, last := msgs[0], msgs[len(msgs)-1]
		bounds := store.MessageQuery{ChatID: q.ChatID, After: q.After, Before: q.Before, Limit: 0, HasAnchor: true}
		probe := bounds
		probe.AnchorTS, probe.AnchorID = first.TS, first.ID
		if _, more, err := store.ReadMessages(ctx, s.DB, probe); err != nil {
			return nil, err
		} else if more {
			olderCur = cursor{Tool: toolReadChat, Chat: q.ChatID, TS: first.TS, ID: first.ID, After: q.After, Before: q.Before}.encode()
		}
		probe = bounds
		probe.Newer, probe.AnchorTS, probe.AnchorID = true, last.TS, last.ID
		if _, more, err := store.ReadMessages(ctx, s.DB, probe); err != nil {
			return nil, err
		} else if more {
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
	fmt.Fprintf(&b, ". %d messages, %s to %s.\n", len(msgs), fmtTS(msgs[0].TS), fmtTS(msgs[len(msgs)-1].TS))
	if chat.Kind != "dm" {
		b.WriteString(w.participants(msgs))
	}
	if left > 0 {
		fmt.Fprintf(&b, "%d more messages of this page left out to keep the answer under about 20k tokens: page with the cursors.\n", left)
	}
	if olderCur != "" {
		fmt.Fprintf(&b, "Older messages: cursor=%q\n", olderCur)
	}

	day := ""
	for i := range msgs {
		t := local(msgs[i].TS)
		if d := t.Format("Mon 2 Jan 2006 (UTC-07:00)"); d != day {
			day = d
			fmt.Fprintf(&b, "── %s ──\n", d)
		}
		b.WriteString(lines[i] + "\n")
	}
	if newerCur != "" {
		fmt.Fprintf(&b, "Newer messages: cursor=%q", newerCur)
	}
	return textResult(strings.TrimRight(b.String(), "\n")), nil
}

// participants lists the people speaking in a group window once, so message
// lines can use bare names.
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
	return "Speakers: " + strings.Join(parts, "; ") + "\n"
}

// line renders one transcript line.
func (w wrapper) line(m *store.MessageRow, full bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s ", local(m.TS).Format("15:04"), messageID(m.ID))
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
		b.WriteString(" [deleted by sender; original below]")
	}
	if t := w.wrap(w.body(m, full)); t != "" {
		b.WriteString(" " + t)
		if n, max := len([]rune(m.Text)), w.textCap(full); max > 0 && n > max {
			fmt.Fprintf(&b, " [truncated: %d characters; call whatsapp_read_chat with around=%s, limit=1, response_format=detailed for the full text]", n, messageID(m.ID))
		}
	} else if m.Kind == "text" && m.DeletedAt != 0 {
		b.WriteString(" (text not available)")
	}
	if m.EditedAt != 0 {
		if full {
			fmt.Fprintf(&b, " [edited %s]", fmtTS(m.EditedAt))
		} else {
			b.WriteString(" [edited]")
		}
	}
	if len(m.Reactions) > 0 {
		b.WriteString(" " + w.reactions(m.Reactions, full))
	}
	if full {
		fmt.Fprintf(&b, " (wa_id %s", m.WAID)
		if m.SenderJID != "" {
			fmt.Fprintf(&b, ", from %s", m.SenderJID)
		}
		b.WriteString(")")
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

// body is the message text, cut to textCap runes.
func (w wrapper) body(m *store.MessageRow, full bool) string {
	r := []rune(m.Text)
	if max := w.textCap(full); max == 0 || len(r) <= max {
		return m.Text
	}
	return string(r[:w.textCap(full)]) + "…"
}

// mediaTag renders "[voice 0:42 · transcript]": the transcript text itself
// only in detailed mode, or when w.transcripts asks for it (a search hit).
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
		switch {
		case md.Transcript == "":
		case full || w.transcripts:
			t := md.Transcript
			if r := []rune(t); w.capText > 0 && len(r) > w.capText {
				t = string(r[:w.capText]) + "…"
				parts = append(parts, "transcript: "+w.wrap(t)+fmt.Sprintf(" [transcript truncated: %d characters; whatsapp_get_media message=%s mode=transcript shows all]", len(r), messageID(m.ID)))
				break
			}
			parts = append(parts, "transcript: "+w.wrap(t))
		default:
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
			parts = append(parts, sanitize(r.Emoji)+" "+who)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	counts := map[string]int{}
	var order []string
	for _, r := range rs {
		e := sanitize(r.Emoji)
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
