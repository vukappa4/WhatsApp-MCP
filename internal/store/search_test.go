package store

import (
	"strings"
	"testing"
)

func TestFTSQuery(t *testing.T) {
	for in, want := range map[string]string{
		`ciao mondo`:         `"ciao" "mondo"`,
		`"ci vediamo" dom*`:  `"ci vediamo" "dom"*`,
		`"ci vediamo*"`:      `"ci vediamo"*`,
		`e-mail: (urgent`:    `"e mail" "urgent"`,
		`NOT a OR b`:         `"NOT" "a" "OR" "b"`,
		`a"b`:                `"a" "b"`,
		`"unbalanced phrase`: `"unbalanced phrase"`,
		`perché`:             `"perché"`,
		`* - : ( "`:          ``,
	} {
		if got := FTSQuery(in); got != want {
			t.Errorf("FTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// Whatever the user types, the MATCH expression is valid FTS5.
func TestFTSQueryIsAlwaysValid(t *testing.T) {
	db, _ := open(t)
	inputs := []string{`"`, `""`, `"*"`, `*`, `**a`, `a**`, `-a`, `a:b`, `text:a`, `(a`, `a)`, `a OR`, `NOT a`, `NEAR(a b)`,
		`a + b`, `^a`, `a"b"c`, `'a'`, `\"a\"`, `a\x00b`, "a‮b", `perché`, `l'altro`, `100%`, `#tag @user`, `😀 ciao`,
		`{a b}`, `a AND (b OR c)`, `"open phrase*`, `a*"b`}
	for _, in := range inputs {
		q := FTSQuery(in)
		if q == "" {
			continue
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, q).Scan(&n); err != nil {
			t.Errorf("FTSQuery(%q) = %q: %v", in, q, err)
		}
		if p := FTSPlainQuery(in); p != "" {
			if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, p).Scan(&n); err != nil {
				t.Errorf("FTSPlainQuery(%q) = %q: %v", in, p, err)
			}
		}
	}
}

// Numbers that are not decimal digits (superscripts, subscripts, fractions,
// Roman numerals) are token characters for FTS5's unicode61 tokenizer, so a
// search for them must keep them.
func TestFTSQueryKeepsAllNumbers(t *testing.T) {
	db, _ := open(t)
	texts := []string{"giardino di 100 m² con vista", "formula H₂O pura", "aggiungi ½ bicchiere", "il secolo Ⅻ"}
	for _, text := range texts {
		if _, err := db.Exec(`INSERT INTO messages_fts(text, transcript) VALUES (?, '')`, text); err != nil {
			t.Fatal(err)
		}
	}
	for in, want := range map[string]string{"m²": "giardino", "H₂O": "formula", "½": "aggiungi", "Ⅻ": "secolo", `"100 m²"`: "giardino"} {
		for _, q := range []string{FTSQuery(in), FTSPlainQuery(in)} {
			var got []string
			rows, err := db.Query(`SELECT text FROM messages_fts WHERE messages_fts MATCH ?`, q)
			if err != nil {
				t.Fatalf("search %q as %q: %v", in, q, err)
			}
			for rows.Next() {
				var s string
				rows.Scan(&s)
				got = append(got, s)
			}
			rows.Close()
			if len(got) != 1 || !strings.Contains(got[0], want) {
				t.Errorf("search %q as %q found %q, want the message with %q", in, q, got, want)
			}
		}
	}
}
