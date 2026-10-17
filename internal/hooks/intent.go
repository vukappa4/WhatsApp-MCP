// Package hooks implements the Claude Code hooks that gate the send tools:
// UserPromptSubmit records whether the user's own prompt asks to send, and
// PreToolUse asks for confirmation when it did not.
package hooks

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// SendIntent reports whether a prompt written by the user asks to send,
// reply to or forward a message. It is a deliberately simple word-level
// detector over Italian and English, case- and accent-insensitive:
//
//   - A send verb counts in imperative or infinitive form, with or without a
//     clitic (invia, inviare, invialo, mandagli, rispondile, scrivigli,
//     inoltra, send, reply, forward...). Past participles (mandato, inviato,
//     risposto, sent) never count, so questions about received messages do
//     not either ("cosa mi ha mandato Marco?").
//   - "rispondi", "scrivi" and "reply" count only when addressed to someone
//     ("rispondi a Marco", "scrivi a Marco", "reply to her"), so instructions
//     to Claude such as "rispondi in italiano" or "scrivi una funzione" do not.
//   - A negated send verb anywhere ("non mandare", "don't send", "senza
//     inviare", "prima di inviare", "no need to reply") cancels the whole
//     prompt: "prepara il testo e non mandarlo" is not a send.
//   - A prompt about a draft ("bozza", "draft") is never a send.
//   - An infinitive after "da", "per", "di", "a" or "to" describes a purpose,
//     not an order ("un testo da inviare", "aiutami a rispondere", "a message
//     to send").
//   - A question counts only as a polite request opening with a modal ("puoi
//     mandare...?", "can you send...?") or "glielo mandi?"; other questions
//     ask about something ("cosa mi ha mandato?", "should I reply?").
//   - Forms addressed to the user ("mandami", "send me") do not count.
//
// Limits: it reads words, not meaning. "manda la mail a Marco" counts (any
// channel), and an advice request without "?" ("cosa devo rispondere a
// Marco") counts. Requests without one of these verb forms ("lo mandi tu?",
// "fagli sapere", "I want to send...") are missed. A miss costs one
// confirmation prompt; a false hit leaves the decision to Claude Code's
// normal permission flow.
func SendIntent(prompt string) bool {
	cs := clauses(prompt)
	for _, c := range cs {
		for _, w := range c.words {
			if w == "bozza" || w == "bozze" || w == "draft" || w == "drafts" || w == "drafting" {
				return false
			}
		}
	}
	hit := false
	for _, c := range cs {
		h, negated := clauseIntent(c)
		if negated {
			return false
		}
		hit = hit || h
	}
	return hit
}

// requestModals open a question that is a polite request.
var requestModals = map[string]bool{"puoi": true, "potresti": true, "riesci": true, "potete": true, "puo": true,
	"can": true, "could": true, "would": true, "will": true, "please": true, "glielo": true, "gliela": true, "glieli": true, "gliele": true}

// reInfinitive matches the Italian infinitives of the send verbs, with clitics.
var reInfinitive = regexp.MustCompile(`^(?:inviar|mandar|inoltrar|risponder|scriver)\w*$`)

// purpose words before an infinitive: "da inviare", "per mandare", "a rispondere", "to send".
var purpose = map[string]bool{"da": true, "per": true, "di": true, "a": true, "ad": true, "to": true}

// notRecipients after "rispondi a" / "reply to": answering a question, not a person.
var notRecipients = map[string]bool{"domanda": true, "domande": true, "questa": true, "questo": true, "quello": true, "quella": true,
	"sopra": true, "punto": true, "quesito": true, "richiesta": true, "mail": true, "email": true, "question": true, "this": true,
	"that": true, "the": true, "my": true, "these": true}

// clause is one sentence-like piece of the prompt, folded to words.
type clause struct {
	words    []string
	raw      []string // the original spelling, for capitalized names
	question bool
}

var clauseSplit = regexp.MustCompile(`[.!?;\n]+|,\s*(?:ma|e poi|poi|but|and then|then)\b`)

// clauses splits the prompt at sentence ends and folds each piece.
func clauses(prompt string) []clause {
	var out []clause
	idx := clauseSplit.FindAllStringIndex(prompt, -1)
	start := 0
	for i := 0; i <= len(idx); i++ {
		end := len(prompt)
		q := false
		if i < len(idx) {
			end = idx[i][0]
			q = strings.Contains(prompt[idx[i][0]:idx[i][1]], "?")
		}
		raw := words(prompt[start:end], false)
		if len(raw) > 0 {
			out = append(out, clause{words: words(prompt[start:end], true), raw: raw, question: q})
		}
		if i < len(idx) {
			start = idx[i][1]
		}
	}
	return out
}

// words splits on anything but letters and digits; fold lowercases and strips accents.
func words(s string, fold bool) []string {
	if fold {
		var b strings.Builder
		for _, r := range norm.NFD.String(s) {
			if !unicode.Is(unicode.Mn, r) {
				b.WriteRune(unicode.ToLower(r))
			}
		}
		s = b.String()
	} else {
		s = norm.NFC.String(s)
	}
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

var (
	// Always a send when not negated: invia, inviare, invialo, inviagli, inviaglielo...
	// Excludes inviato/i/a/e (participles), inviami/inviamelo (to the user).
	reAlways = regexp.MustCompile(`^(?:invia|inviare|inviarl[aoie]|inviargli\w*|invial[aoie]|inviagli\w*|invial[eo]|inviatel[aoi]|` +
		`manda|mandare|mandarl[aoie]|mandargli\w*|mandal[aoie]|mandagli\w*|mandatel[aoi]|mandalo|` +
		`inoltra|inoltrare|inoltral[aoie]|inoltragli\w*|inoltrarl[aoie]|inoltrargli\w*|` +
		`rispondigli|rispondile|rispondergli|risponderle|rispondetegli|` +
		`scrivigli|scrivile|scrivergli|scriverle|scrivetegli|` +
		`digli|dille|ditegli|` +
		`send|forward|reply)$`)
	// Need a recipient right after: rispondi a Marco, scrivi a Marco, reply to her.
	reAddressed = regexp.MustCompile(`^(?:rispondi|rispondere|scrivi|scrivere|reply|respond|write|message|text|dm|tell|whatsapp)$`)
	reTo        = regexp.MustCompile(`^(?:a|ad|al|allo|alla|ai|agli|alle|to|back|him|her|them)$`)
)

var (
	negators = map[string]bool{"non": true, "senza": true, "evita": true, "mai": true, "not": true, "dont": true, "never": true, "without": true, "before": true, "prima": true, "t": true, "no": true, "neanche": true, "nemmeno": true}
	// skippable between a negation and its verb: "non lo mandare", "don't ever send", "prima di inviare".
	fillers = map[string]bool{"lo": true, "la": true, "li": true, "le": true, "gli": true, "glielo": true, "gliela": true, "ancora": true,
		"piu": true, "di": true, "ci": true, "ne": true, "voglio": true, "vuoi": true, "devi": true, "devo": true, "serve": true,
		"bisogno": true, "ever": true, "yet": true, "to": true, "it": true, "need": true, "want": true, "you": true, "we": true, "i": true}
	// nouns or idioms that make a send verb something else.
	notAfter = map[string][]string{
		"manda":   {"in", "avanti"},
		"mandare": {"in", "avanti"},
		"mandalo": {"in"},
		"send":    {"me"},
		"forward": {"slash"},
	}
	notBefore = map[string]bool{"look": true, "looking": true, "looked": true, "move": true, "moving": true, "going": true, "step": true, "straight": true, "fast": true, "his": true, "her": true, "their": true, "my": true, "your": true, "the": true, "a": true, "this": true, "that": true}
)

// clauseIntent reports whether the clause orders a send, and whether it
// holds a negated send verb (which cancels the whole prompt).
func clauseIntent(c clause) (hit, negatedSend bool) {
	// A question asks about something unless it is a polite request:
	// "cosa mi ha mandato?", "should I reply?" are not sends; "puoi mandare...?" is.
	ask := !c.question || (len(c.words) > 0 && requestModals[c.words[0]])
	for i, w := range c.words {
		next, prev := "", ""
		if i+1 < len(c.words) {
			next = c.words[i+1]
		}
		if i > 0 {
			prev = c.words[i-1]
		}
		verb := false
		switch {
		case reAlways.MatchString(w):
			verb = true
			for _, n := range notAfter[w] {
				if next == n {
					verb = false
				}
			}
			if (w == "reply" || w == "forward" || w == "send") && notBefore[prev] {
				verb = false // "his reply", "look forward", "the send button"
			}
			if w == "reply" && next != "" && !(reTo.MatchString(next) && !notRecipients[at(c.words, i+2)]) && next != "saying" && next != "that" {
				verb = false // "reply in English", "reply to this question"
			}
		case reAddressed.MatchString(w):
			verb = addressed(w, next, c, i)
		case w == "glielo" || w == "gliela" || w == "glieli" || w == "gliele":
			// "glielo mandi?", "gliela inoltri?": a request in the second person.
			verb = next == "mandi" || next == "invii" || next == "inoltri" || next == "scrivi"
		}
		switch {
		case !verb:
		case negated(c.words, i):
			negatedSend = true
		case purpose[prev] && (reInfinitive.MatchString(w) || prev == "to"):
			// "un testo da inviare", "aiutami a rispondere", "a message to send": a purpose, not an order.
		case ask:
			hit = true
		}
	}
	return hit, negatedSend
}

func at(ws []string, i int) string {
	if i < len(ws) {
		return ws[i]
	}
	return ""
}

// addressed: verbs that only mean a send when followed by a recipient.
func addressed(w, next string, c clause, i int) bool {
	if next == "" {
		return false
	}
	switch w {
	case "message", "text", "dm", "tell", "whatsapp":
		if i > 0 && notBefore[c.words[i-1]] {
			return false // "the message", "this text"
		}
		if next == "him" || next == "her" || next == "them" || next == "back" {
			return true
		}
		// "message Marco", "text Giulia": a capitalized name right after.
		if len(c.raw) != len(c.words) {
			return false
		}
		r := []rune(c.raw[i+1])
		return unicode.IsUpper(r[0])
	case "scrivi", "scrivere":
		if next == "un" || next == "il" || next == "questo" {
			// "scrivi un messaggio a Paola"; "scrivi un messaggio di benvenuto per la newsletter" is copywriting.
			return at(c.words, i+2) == "messaggio" && hasRecipientPrep(c.words[i+3:])
		}
	case "write":
		return next == "to" || next == "back"
	}
	if (next == "su" || next == "on" || next == "via") && i+2 < len(c.words) && c.words[i+2] == "whatsapp" {
		return true
	}
	return reTo.MatchString(next) && !notRecipients[at(c.words, i+2)]
}

// hasRecipientPrep reports whether an Italian recipient preposition follows.
func hasRecipientPrep(ws []string) bool {
	for _, w := range ws {
		switch w {
		case "a", "ad", "al", "allo", "alla", "ai", "agli", "alle":
			return true
		}
	}
	return false
}

// negated reports whether a negation precedes word i, skipping fillers.
func negated(ws []string, i int) bool {
	for j := i - 1; j >= 0 && j >= i-4; j-- {
		switch {
		case negators[ws[j]]:
			switch ws[j] {
			case "no": // "no need to reply"; "no, mandalo" is a go-ahead
				return j+1 < len(ws) && ws[j+1] == "need"
			case "prima": // "prima di inviare"; "prima manda a Marco" is an order
				return j+1 < len(ws) && ws[j+1] == "di"
			}
			return true
		case ws[j] == "don" || ws[j] == "do" && j+1 < len(ws) && ws[j+1] == "not":
			return true
		case fillers[ws[j]]:
		default:
			return false
		}
	}
	return false
}
