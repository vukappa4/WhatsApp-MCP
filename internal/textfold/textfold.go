// Package textfold is the one way names are compared: the server resolving
// a recipient and the send gate checking it must agree on who a name is.
package textfold

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Fold lowercases, strips accents and turns everything but letters and
// digits (punctuation, emoji, spaces) into single spaces, so "Niccolò
// D'Amico" and "niccolo d amico" compare equal.
func Fold(s string) string {
	var b strings.Builder
	space := true
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}
