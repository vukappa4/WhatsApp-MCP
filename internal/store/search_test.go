package store

import "testing"

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
