package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/textfold"
)

// Match scores, best first. Ties rank saved contacts, then recent interaction.
const (
	scoreID        = 1000 // p_N, JID or LID
	scorePhone     = 900  // the whole phone number
	scoreFormer    = 850  // the whole number, a former one now someone else's
	scoreExact     = 100  // a name equals the query
	scorePrefix    = 80   // a name starts with the query
	scoreWords     = 60   // every query word starts a word of one name
	scoreSubstring = 40   // a name contains the query
	scorePhonePart = 30   // the number contains the digits
	scoreLoose     = 20   // every query word appears somewhere in the names or number
)

type personMatch struct {
	store.PersonCandidate
	score int
}

// fold is how every name comparison folds text; the send gate uses the same.
var fold = textfold.Fold

// nameScore rates how well the folded query q matches one person's names.
func nameScore(q string, words []string, c *store.PersonCandidate) int {
	best := 0
	var all []string
	for _, n := range []string{c.FullName, c.FirstName, c.BusinessName, c.PushName, c.Name} {
		f := fold(n)
		if f == "" {
			continue
		}
		all = append(all, f)
		nameWords := strings.Fields(f)
		score := 0
		switch {
		case f == q:
			score = scoreExact
		case strings.HasPrefix(f, q):
			score = scorePrefix
		case everyWordStartsOne(words, nameWords):
			score = scoreWords
		case strings.Contains(f, q):
			score = scoreSubstring
		}
		best = max(best, score)
	}
	if best == 0 {
		hay := strings.Join(all, " ") + " " + strings.TrimPrefix(c.Phone, "+")
		loose := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				loose = false
				break
			}
		}
		if loose {
			best = scoreLoose
		}
	}
	return best
}

func everyWordStartsOne(words, nameWords []string) bool {
	for _, w := range words {
		if !slices.ContainsFunc(nameWords, func(n string) bool { return strings.HasPrefix(n, w) }) {
			return false
		}
	}
	return true
}

// matchPeople finds persons by p_N id, JID or LID, phone number in any
// format, or part of a name, best match first.
func (s *Server) matchPeople(ctx context.Context, query string) ([]personMatch, error) {
	q := strings.TrimSpace(query)
	byScore := map[int64]personMatch{}
	add := func(cs []store.PersonCandidate, score int) {
		for _, c := range cs {
			if old, ok := byScore[c.ID]; !ok || old.score < score {
				byScore[c.ID] = personMatch{c, score}
			}
		}
	}
	switch {
	case q == "":
	case strings.HasPrefix(q, "p_"):
		if id, ok := parseID(q, "p_"); ok {
			c, found, err := store.PersonCandidateByID(ctx, s.DB, id)
			if err != nil {
				return nil, err
			}
			if found {
				add([]store.PersonCandidate{c}, scoreID)
			}
		}
	case strings.Contains(q, "@"):
		if j, err := types.ParseJID(q); err == nil {
			if n, _, ok := people.Normalize(j); ok {
				cs, err := store.PersonsByJID(ctx, s.DB, n.String())
				if err != nil {
					return nil, err
				}
				add(cs, scoreID)
			}
		}
	case phoneDigits(q) != "":
		raw := onlyDigits(q)
		cs, err := store.PersonsByPhone(ctx, s.DB, phoneDigits(q))
		if err != nil {
			return nil, err
		}
		add(cs, scorePhone)
		if cs, err = store.PersonsByFormerPhone(ctx, s.DB, phoneDigits(q)); err != nil {
			return nil, err
		}
		add(cs, scoreFormer)
		// A bare long number may also be a LID.
		if cs, err = store.PersonsByJID(ctx, s.DB, raw+"@lid"); err != nil {
			return nil, err
		}
		add(cs, scoreID)
		if len(byScore) == 0 {
			if cs, err = store.PersonsByPhonePart(ctx, s.DB, raw); err != nil {
				return nil, err
			}
			add(cs, scorePhonePart)
		}
	default:
		fq := fold(q)
		if fq == "" {
			break
		}
		words := strings.Fields(fq)
		all, err := store.AllPersonNames(ctx, s.DB)
		if err != nil {
			return nil, err
		}
		var hits []store.PersonCandidate
		var scores []int
		for i := range all {
			if sc := nameScore(fq, words, &all[i]); sc > 0 {
				hits = append(hits, all[i])
				scores = append(scores, sc)
			}
		}
		if err := store.FillLastAt(ctx, s.DB, hits); err != nil {
			return nil, err
		}
		for i := range hits {
			byScore[hits[i].ID] = personMatch{hits[i], scores[i]}
		}
	}
	out := make([]personMatch, 0, len(byScore))
	for _, m := range byScore {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b personMatch) int {
		switch {
		case a.score != b.score:
			return b.score - a.score
		case a.Saved != b.Saved:
			if a.Saved {
				return -1
			}
			return 1
		case a.LastAt != b.LastAt:
			if a.LastAt > b.LastAt {
				return -1
			}
			return 1
		}
		return int(a.ID - b.ID)
	})
	return out, nil
}

// count renders "1 message", "3 messages".
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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

// resolvePerson picks one person for a filter, or explains the ambiguity.
func (s *Server) resolvePerson(ctx context.Context, arg, param string) (*store.PersonCandidate, string, error) {
	ms, err := s.matchPeople(ctx, arg)
	if err != nil {
		return nil, "", err
	}
	switch {
	case len(ms) == 0:
		return nil, fmt.Sprintf("No person matches %s %q. Use whatsapp_search_people to find the p_N id.", param, arg), nil
	case len(ms) == 1, ms[0].score >= scorePrefix && ms[1].score < ms[0].score:
		return &ms[0].PersonCandidate, "", nil
	}
	w := newWrapper()
	n := min(len(ms), 8)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q matches %d people:\n", param, arg, len(ms))
	for _, m := range ms[:n] {
		fmt.Fprintf(&b, "- %s %s (%s, last interaction %s)\n", personID(m.ID), w.label(&m.PersonRef), nameSource(&m.PersonCandidate), orNever(fmtTS(m.LastAt)))
	}
	if len(ms) > n {
		fmt.Fprintf(&b, "...and %d more.\n", len(ms)-n)
	}
	fmt.Fprintf(&b, "Call again with %s set to one of these ids.", param)
	return nil, b.String(), nil
}

// nameSource says where the display name comes from.
func nameSource(c *store.PersonCandidate) string {
	switch {
	case c.IsMe:
		return "the account owner"
	case c.Source == people.SourceFullName || c.Source == people.SourceFirstName:
		return "saved contact, name from the address book"
	case c.Source == people.SourceBusinessName:
		return "not saved, name from their WhatsApp business profile"
	case c.Source == people.SourcePushName:
		return "not saved, name from their WhatsApp profile"
	case c.Source == people.SourcePhone:
		return "not saved, no name known"
	}
	return "no name or phone known"
}

// ---- whatsapp_search_people ----

type searchPeopleIn struct {
	Query          string `json:"query,omitempty" jsonschema:"part of a name (case and accents ignored), a phone number in any format or part of one (+39 333 123 4567, 393331234567, 3331234567), a p_N id or a LID such as 123456789@lid; required unless cursor is set"`
	Limit          int    `json:"limit,omitempty" jsonschema:"people per page, 1-50, default 10"`
	Cursor         string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page; it carries the query"`
	ResponseFormat string `json:"response_format,omitempty" jsonschema:"default concise; detailed adds every name candidate, all phone numbers and LIDs, and more groups"`
}

func (s *Server) searchPeople(ctx context.Context, _ *mcp.CallToolRequest, in searchPeopleIn) (*mcp.CallToolResult, error) {
	limit := in.Limit
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 50 {
		return errResult("limit must be between 1 and 50, got %d", in.Limit)
	}
	query, offset := in.Query, 0
	if in.Cursor != "" {
		c, err := decodeCursor(in.Cursor, toolSearchPeople)
		if err != nil {
			return errResult("%v", err)
		}
		query, offset = c.Query, c.Off
	}
	if strings.TrimSpace(query) == "" {
		return errResult("query is required: part of a name, a phone number, a p_N id or a LID")
	}
	ms, err := s.matchPeople(ctx, query)
	if err != nil {
		return nil, err
	}
	fresh, err := s.freshness(ctx, s.now().Add(-7*24*time.Hour).Unix(), 0)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(fresh + "\n")
	if len(ms) == 0 {
		fmt.Fprintf(&b, "No person matches %q. Names match any part of address-book and WhatsApp profile names; "+
			"phone numbers match in any format. Try fewer letters or the phone number.", query)
		return textResult(b.String()), nil
	}
	page := ms[min(offset, len(ms)):]
	if len(page) == 0 {
		fmt.Fprintf(&b, "No more people match %q.", query)
		return textResult(b.String()), nil
	}
	more := len(page) > limit
	if more {
		page = page[:limit]
	}
	full := detailed(in.ResponseFormat)
	maxGroups := 3
	if full {
		maxGroups = 10
	}
	w := newWrapper()
	if len(ms) == 1 {
		fmt.Fprintf(&b, "1 person matches %q:\n", query)
	} else {
		fmt.Fprintf(&b, "%d people match %q, best match first; showing %d-%d:\n", len(ms), query, offset+1, offset+len(page))
	}
	ids := make([]int64, len(page))
	for i := range page {
		ids[i] = page[i].ID
	}
	acts, err := store.GetPeopleActivity(ctx, s.DB, ids, maxGroups)
	if err != nil {
		return nil, err
	}
	for i := range page {
		act := acts[page[i].ID]
		b.WriteString(w.personBlock(&page[i], &act, full))
	}
	if more {
		next := cursor{Tool: toolSearchPeople, Query: query, Off: offset + len(page)}.encode()
		fmt.Fprintf(&b, "More people: call again with cursor=%q, or narrow the query.", next)
	}
	return textResult(strings.TrimRight(b.String(), "\n")), nil
}

// personBlock renders one search result: identity, names, numbers, chats.
func (w wrapper) personBlock(m *personMatch, a *store.PersonActivity, full bool) string {
	var b strings.Builder
	label := w.label(&m.PersonRef)
	if m.IsMe {
		label = "me"
		if m.Phone != "" {
			label += " (" + people.FormatPhone(m.Phone) + ")"
		}
	}
	fmt.Fprintf(&b, "%s %s · %s\n", personID(m.ID), label, nameSource(&m.PersonCandidate))

	var phones, lids []string
	for _, j := range a.JIDs {
		user, _, _ := strings.Cut(j.JID, "@")
		switch {
		case j.Kind == "pn" && "+"+user != m.Phone:
			phones = append(phones, people.FormatPhone("+"+user))
		case j.Kind == "lid":
			lids = append(lids, j.JID)
		}
	}
	if full {
		var names []string
		if m.FullName != "" {
			names = append(names, "address book "+fmt.Sprintf("%q", sanitize(m.FullName)))
		}
		if m.FirstName != "" {
			names = append(names, "address book first name "+fmt.Sprintf("%q", sanitize(m.FirstName)))
		}
		if n := people.ProfileName(m.BusinessName); n != "" {
			names = append(names, "business profile "+n)
		}
		if n := people.ProfileName(m.PushName); n != "" {
			names = append(names, "WhatsApp profile "+n)
		}
		if len(names) > 0 {
			b.WriteString("   names: " + strings.Join(names, "; ") + "\n")
		}
		ids := []string{}
		if m.Phone != "" {
			ids = append(ids, "phone "+people.FormatPhone(m.Phone))
		}
		for _, p := range phones {
			ids = append(ids, "phone "+p)
		}
		for _, l := range lids {
			ids = append(ids, "LID "+l)
		}
		if len(ids) > 0 {
			b.WriteString("   ids: " + strings.Join(ids, ", ") + "\n")
		}
	} else if len(phones) > 0 {
		b.WriteString("   other numbers: " + strings.Join(phones, ", ") + "\n")
	}
	if len(a.Former) > 0 {
		var former []string
		for _, f := range a.Former {
			former = append(former, fmt.Sprintf("%s (now %s)", people.FormatPhone(f.Phone), personID(f.Owner)))
		}
		b.WriteString("   former numbers, now another account's: " + strings.Join(former, ", ") + "\n")
	}

	if a.LastAt != 0 {
		fmt.Fprintf(&b, "   last interaction: %s in %s\n", fmtTS(a.LastAt), chatID(a.LastChatID))
	} else {
		b.WriteString("   last interaction: none stored\n")
	}
	who, wrote := "them", "they"
	if m.IsMe {
		who, wrote = "me", "I"
	}
	var chats []string
	if a.DM != nil {
		chats = append(chats, fmt.Sprintf("%s DM (%s, last %s)", chatID(a.DM.Chat.ID), count(a.DM.Messages, "message"), orNever(fmtTS(a.DM.LastAt))))
	}
	for i := range a.Groups {
		g := &a.Groups[i]
		chats = append(chats, fmt.Sprintf("%s %s %s (%d from %s, last %s)", chatID(g.Chat.ID), w.chatName(&g.Chat), g.Chat.Kind,
			g.Messages, who, fmtTS(g.LastAt)))
	}
	if len(chats) == 0 {
		b.WriteString("   chats: none stored\n")
	} else {
		b.WriteString("   chats: " + strings.Join(chats, "; "))
		if extra := a.GroupsTotal - len(a.Groups); extra > 0 {
			fmt.Fprintf(&b, "; %d more groups where %s wrote", extra, wrote)
		}
		b.WriteString("\n")
	}
	return b.String()
}
