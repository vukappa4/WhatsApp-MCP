package hooks

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// Giorgio (p_4) changed number from +39 347 111 1111 to +39 347 222 2222,
// and WhatsApp gave the old number to a stranger (p_5). The gate shows his
// current number, and his old number names the stranger, never him.
func TestHookNumberChangeAndReassignment(t *testing.T) {
	g := newGate(t)
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(g.root, "prod", "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	giorgio, stranger := types.NewJID("555555555555555", types.HiddenUserServer), types.NewJID("666666666666666", types.HiddenUserServer)
	oldNum, newNum := types.NewJID("393471111111", types.DefaultUserServer), types.NewJID("393472222222", types.DefaultUserServer)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, l := range [][2]types.JID{{giorgio, oldNum}, {giorgio, newNum}, {stranger, oldNum}} {
		if _, err := people.Link(ctx, db, l[0], l[1], at.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := people.Lookup(ctx, db, giorgio)
	if err != nil {
		t.Fatal(err)
	}
	if err := people.SetNames(ctx, db, p, people.Names{FullName: "Giorgio Neri"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if p != 4 {
		t.Fatalf("Giorgio is p_%d, want p_4", p)
	}

	g.prompt("s", "scrivi a Luca")
	if r := g.pre("s", send, map[string]any{"to": "Giorgio Neri", "text": "x"}); !strings.Contains(r, "Giorgio Neri (+393472222222)") {
		t.Fatalf("reason %q, want the current number", r)
	}
	for _, tc := range []struct {
		prompt, to string
		silent     bool
	}{
		{"scrivi a Giorgio che arrivo", "p_4", true},
		{"manda al 347 222 2222 che arrivo", "p_4", true},  // his number
		{"manda al 347 111 1111 che arrivo", "p_4", false}, // his old number is someone else's now
		{"scrivi a Giorgio Neri che arrivo", "+39 347 111 1111", false},
		{"scrivi a Giorgio Neri che arrivo", "+39 347 222 2222", true},
		{"scrivi al 347 111 1111 che arrivo", "+39 347 111 1111", true}, // the user named that number
	} {
		g.prompt("s", tc.prompt)
		if r := g.pre("s", send, map[string]any{"to": tc.to, "text": "x"}); (r == "") != tc.silent {
			t.Errorf("prompt %q, to %q: reason %q, want silent=%v", tc.prompt, tc.to, r, tc.silent)
		}
	}
}
