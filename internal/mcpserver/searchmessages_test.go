package mcpserver

import (
	"regexp"
	"strings"
	"testing"
)

func TestSearchMessages(t *testing.T) {
	cs := connect(t, seedSearch(t))
	search := func(args map[string]any) (string, bool) {
		t.Helper()
		return call(t, cs, toolSearchMessages, args)
	}
	hits := func(text string) int { return len(ids(lineIDs, text)) }

	// Accents: "perche" finds "Perché" in the DM and in the group; senders render as names.
	text, isErr := search(map[string]any{"query": "perche"})
	if isErr || hits(text) != 2 || !strings.HasPrefix(text, "[whatsapp: ") || !strings.Contains(text, "Perché non vieni") ||
		!strings.Contains(text, "Niccolò D'Amico:") || !strings.Contains(text, "Calcetto</untrusted_wa") {
		t.Fatalf("accents:\n%s", text)
	}

	// FTS syntax in user input never surfaces as an error.
	for q, want := range map[string]int{
		`e-mail: test (urgent`: 1, `"e-mail`: 1, `NOT OR NEAR`: 1, `test*`: 1, `urgent)`: 1, `(test) AND`: 0,
		`fattur*`: 2, `"fattur"*`: 2, `fattura`: 1, `"ci vediamo"`: 1, `ci vediamo`: 2, `"domani ci`: 0, `text:urgent`: 0,
		`a"b`: 0, `bonifico`: 1, `niccolo`: 1, `-pizza`: 25,
	} {
		text, isErr = search(map[string]any{"query": q})
		if isErr || hits(text) != min(want, 20) {
			t.Errorf("query %q: %d hits (want %d), isErr %v:\n%s", q, hits(text), want, isErr, text)
		}
		if strings.Contains(text, "fts5") || strings.Contains(text, "syntax") {
			t.Errorf("query %q leaked an FTS error:\n%s", q, text)
		}
	}
	for _, q := range []string{`"`, `*`, `-`, `:`, `(`, `"*"`, `()`} {
		text, isErr = search(map[string]any{"query": q})
		if !isErr || !strings.Contains(text, "no searchable words") {
			t.Errorf("query %q:\n%s", q, text)
		}
	}

	// The transcript is searchable and shown with the voice note.
	text, _ = search(map[string]any{"query": "bonifico"})
	if !strings.Contains(text, "[voice 0:07 · transcript: <untrusted_wa") {
		t.Fatalf("transcript hit:\n%s", text)
	}

	// A long transcript becomes an excerpt around the match in concise mode.
	text, _ = search(map[string]any{"query": "assicurazione"})
	if !strings.Contains(text, "della assicurazione") || !strings.Contains(text, "[transcript excerpt of ") || len(text) > 1200 {
		t.Fatalf("long transcript (%d chars):\n%s", len(text), text)
	}

	// Filters.
	for _, tc := range []struct {
		args map[string]any
		want int
	}{
		{map[string]any{"query": "fattur*", "has_media": "image"}, 1},
		{map[string]any{"has_media": "any"}, 3},
		{map[string]any{"has_media": "voice"}, 2},
		{map[string]any{"query": "test", "from_me": true}, 1},
		{map[string]any{"query": "test", "from_me": false}, 0},
		{map[string]any{"query": "test", "from": "me"}, 1},
		{map[string]any{"query": "perche", "in_chat": "Calcetto"}, 1},
		{map[string]any{"query": "perche", "from": "niccolo"}, 2},
		{map[string]any{"query": "perche", "from": "+39 333 555 5555", "in_chat": "+39 333 555 5555"}, 1},
		{map[string]any{"query": "pizza", "after": "2026-10-10T09:00:00+02:00", "before": "2026-10-10T09:10:00+02:00"}, 0},
		{map[string]any{"query": "pizza", "before": "2026-10-11"}, 20},
		{map[string]any{"query": "pizza", "after": "2026-10-12"}, 0},
	} {
		text, isErr = search(tc.args)
		if isErr || hits(text) != tc.want {
			t.Errorf("%v: %d hits, want %d:\n%s", tc.args, hits(text), tc.want, text)
		}
	}
	text, _ = search(map[string]any{"query": "perche", "from": "niccolo"})
	if !strings.Contains(text, "from p_") || !strings.Contains(text, "Niccolò D'Amico (+39 333 555 5555)") {
		t.Fatalf("resolved sender not restated:\n%s", text)
	}
	// A cursor page restates the sender the same way as the first page.
	text, _ = search(map[string]any{"query": "perche", "from": "niccolo", "limit": 1})
	from := regexp.MustCompile(`from p_\d+ Niccolò D'Amico \(\+39 333 555 5555\)`).FindString(text)
	cur := cursorIn(nextCursor, text)
	if from == "" || cur == "" {
		t.Fatalf("first page of 1 lacks the sender or a cursor:\n%s", text)
	}
	if text, _ = search(map[string]any{"cursor": cur}); !strings.Contains(text, from) {
		t.Fatalf("cursor page does not restate %q:\n%s", from, text)
	}
	text, _ = search(map[string]any{"query": "perche", "in_chat": "Calcetto"})
	if !strings.Contains(text, `Search "perche" in c_`) || !strings.Contains(text, "Calcetto</untrusted_wa") ||
		!strings.Contains(text, "(group): 1 match") {
		t.Fatalf("resolved chat not restated:\n%s", text)
	}

	// Ambiguous sender name: an error listing both people.
	text, isErr = search(map[string]any{"query": "pizza", "from": "marco"})
	if !isErr || !strings.Contains(text, "matches 2 people") || !strings.Contains(text, "+39 333 111 1111") ||
		!strings.Contains(text, "+39 333 222 2222") {
		t.Fatalf("ambiguous from:\n%s", text)
	}
	for _, args := range []map[string]any{
		{}, {"query": "x", "has_media": "gif"}, {"query": "x", "context": 4}, {"query": "x", "sort": "oldest"},
		{"query": "x", "limit": 51}, {"query": "x", "from": "me", "from_me": false}, {"query": "x", "in_chat": "c_999"},
		{"query": "x", "cursor": "garbage"},
	} {
		if text, isErr = search(args); !isErr {
			t.Errorf("%v must be an error:\n%s", args, text)
		}
	}

	// Context lines surround the hit and are marked apart from it.
	text, _ = search(map[string]any{"query": "fatturazione", "context": 1})
	if !strings.Contains(text, "\n  ") || !strings.Contains(text, "\n> ") || !strings.Contains(text, "ci sentiamo") ||
		!strings.Contains(text, "foto della fattura") {
		t.Fatalf("context:\n%s", text)
	}

	// Paging, both orders: 25 hits in pages of 20 + 5, no repeats; concise output stays small.
	for _, sort := range []string{"relevance", "newest"} {
		text, _ = search(map[string]any{"query": "pizza", "sort": sort})
		if len(text) > 16000 || !strings.Contains(text, "[excerpt of ") || !strings.Contains(text, "25 matches") {
			t.Fatalf("%s page 1 (%d chars):\n%s", sort, len(text), text)
		}
		seen := map[string]bool{}
		for _, id := range ids(lineIDs, text) {
			seen[id] = true
		}
		cur := cursorIn(nextCursor, text)
		if cur == "" {
			t.Fatalf("%s: no cursor", sort)
		}
		text, _ = search(map[string]any{"cursor": cur})
		for _, id := range ids(lineIDs, text) {
			seen[id] = true
		}
		if len(seen) != 25 || cursorIn(nextCursor, text) != "" {
			t.Fatalf("%s page 2: %d distinct ids:\n%s", sort, len(seen), text)
		}
		if _, isErr := call(t, cs, toolReadChat, map[string]any{"cursor": cur}); !isErr {
			t.Fatal("a search cursor must not work for read_chat")
		}
	}
	text, _ = search(map[string]any{"query": "pizza", "sort": "newest", "limit": 1})
	if !strings.Contains(text, "pizza numero 24") {
		t.Fatalf("newest first:\n%s", text)
	}
	full, _ := search(map[string]any{"query": "pizza", "limit": 1, "response_format": "detailed"})
	if strings.Contains(full, "[excerpt") || !strings.Contains(full, "(wa_id ") {
		t.Fatalf("detailed must show the full text:\n%s", full)
	}
}
