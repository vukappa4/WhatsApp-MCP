package hooks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/textfold"
)

// Recipient is what the gate knows about a tool's target: how to show it,
// and the names and phone numbers the user may call it by.
type Recipient struct {
	Label  string   // "Marco Rossi (+39 333 123 4567)", "group Calcetto"
	Names  []string // person names (full, first, push, business, display) or the group name
	Phones []string // digits only, with country code
	IDs    []string // c_N / p_N ids the user may have typed
}

// Resolver maps a tool's recipient argument to a Recipient.
type Resolver interface {
	Resolve(ctx context.Context, profile, target string) (Recipient, error)
}

// ProfileOf is the daemon profile behind an MCP server name: "dev" when the
// name says so (whatsapp-dev), else "prod".
func ProfileOf(server string) string {
	if strings.Contains(strings.ToLower(server), "dev") {
		return "dev"
	}
	return "prod"
}

// DBResolver reads <Root>/<profile>/data.db read-only, waiting at most a
// second for a lock: the hook never writes and never touches the network.
type DBResolver struct {
	Root string // config.AppDir
}

// resolveBusyTimeout is how long the hook waits for a write lock to clear.
const resolveBusyTimeout = time.Second

func (d DBResolver) Resolve(ctx context.Context, profile, target string) (Recipient, error) {
	target = strings.TrimSpace(target)
	r := Recipient{Label: target}
	if target == "" {
		return r, errors.New("no recipient given")
	}
	id, idKind := parseRef(target)
	digits := ""
	if idKind == "" {
		user, _, isJID := strings.Cut(target, "@")
		if isJID {
			digits = onlyDigits(user)
		} else {
			digits = phoneDigits(target)
		}
	}
	isName := idKind == "" && digits == "" && !strings.Contains(target, "@")
	path := filepath.Join(d.Root, profile, "data.db")
	if _, err := os.Stat(path); err != nil {
		if digits != "" {
			r.Phones = []string{digits}
			return r, nil
		}
		return r, fmt.Errorf("no %s data to look up %s", profile, target)
	}
	db, err := sql.Open("sqlite", store.ReadOnlyDSNWait(path, resolveBusyTimeout))
	if err != nil {
		return r, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	switch {
	case isName:
		return r, d.byName(ctx, db, target, &r)
	case idKind == "p":
		r.IDs = []string{target}
		return r, d.person(ctx, db, id, &r)
	case idKind == "c":
		r.IDs = []string{target}
		var kind, name string
		var person sql.NullInt64
		err := db.QueryRowContext(ctx, `SELECT kind, person_id, name FROM chats WHERE id = ?`, id).Scan(&kind, &person, &name)
		if err != nil {
			return r, fmt.Errorf("chat %s: %w", target, err)
		}
		if kind == "dm" && person.Valid {
			return r, d.person(ctx, db, person.Int64, &r)
		}
		r.Label = "group " + name
		return r, d.group(ctx, db, id, name, &r)
	}
	r.Phones = []string{digits}
	// The person holding the number now, as the send tools find it: a
	// former number given to another account names that account.
	var person sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT person_id FROM jids WHERE jid IN (?, ?) UNION ALL
		SELECT id FROM persons WHERE phone_e164 = ? LIMIT 1`, target, digits+"@s.whatsapp.net", "+"+digits).Scan(&person)
	if err == nil && person.Valid {
		return r, d.person(ctx, db, person.Int64, &r)
	}
	var name string
	var chatID int64
	if err := db.QueryRowContext(ctx, `SELECT id, name FROM chats WHERE jid = ? AND kind = 'group'`, target).Scan(&chatID, &name); err == nil {
		r.Label, r.Phones = "group "+name, nil
		return r, d.group(ctx, db, chatID, name, &r)
	}
	if strings.HasSuffix(target, "@g.us") {
		return r, fmt.Errorf("group %s is not stored", target)
	}
	return r, nil
}

// group fills r with a group's name when the user has written in it: anyone
// in a group can rename it, so the name of a group the user never wrote in
// does not identify it (its c_N id still does).
func (d DBResolver) group(ctx context.Context, db *sql.DB, chatID int64, name string, r *Recipient) error {
	var wrote bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE chat_id = ? AND from_me = 1)`, chatID).Scan(&wrote); err != nil {
		return err
	}
	if wrote {
		r.Names = append(r.Names, name)
	}
	r.IDs = append(r.IDs, "c_"+strconv.FormatInt(chatID, 10))
	return nil
}

// maxNameCandidates bounds the lookups for a name many people share: the
// server refuses such a name as ambiguous anyway.
const maxNameCandidates = 20

// byName resolves a name target the way the server matches one: a person's
// name or a group's name equal to it once both are folded (case, accents,
// emoji and punctuation ignored, textfold.Fold). It collects the trusted
// names of every candidate. A name nobody has is an error, so the gate asks:
// it never vouches for a recipient it cannot see.
func (d DBResolver) byName(ctx context.Context, db *sql.DB, target string, r *Recipient) error {
	want := textfold.Fold(target)
	if want == "" {
		return errors.New("not a name")
	}
	// The same rows the server searches: persons with a name or a number, named groups.
	rows, err := db.QueryContext(ctx, `SELECT id, 0, full_name, first_name, push_name, business_name, display_name FROM persons
			WHERE display_name != '' OR phone_e164 IS NOT NULL
		UNION ALL SELECT id, 1, name, '', '', '', '' FROM chats WHERE kind = 'group' AND name != ''`)
	if err != nil {
		return err
	}
	type cand struct {
		id    int64
		group bool
		name  string // a group's stored name
	}
	var cs []cand
	for rows.Next() {
		var c cand
		var names [5]string
		if err := rows.Scan(&c.id, &c.group, &names[0], &names[1], &names[2], &names[3], &names[4]); err != nil {
			rows.Close()
			return err
		}
		for _, n := range names {
			if n != "" && textfold.Fold(n) == want {
				c.name = names[0]
				cs = append(cs, c)
				break
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(cs) == 0 {
		return errors.New("no stored person or group has this name")
	}
	cs = cs[:min(len(cs), maxNameCandidates)]
	label := r.Label
	for _, c := range cs {
		if c.group {
			r.Label = "group " + c.name
			if err := d.group(ctx, db, c.id, c.name, r); err != nil {
				return err
			}
			continue
		}
		if err := d.person(ctx, db, c.id, r); err != nil {
			return err
		}
	}
	if len(cs) > 1 {
		r.Label = label
	}
	return nil
}

// person fills r with a person's names and phone numbers. Names from the
// address book always count; names the person chose (push and business
// names) only for a saved contact or someone the user has written to: a
// stranger can call themselves anything. The numbers are the ones the person
// holds (jids): a former number WhatsApp gave to another account names that
// account, never this person. The label shows the current number.
func (d DBResolver) person(ctx context.Context, db *sql.DB, id int64, r *Recipient) error {
	var full, first, push, business, display, source string
	var phone sql.NullString
	var trusted bool
	err := db.QueryRowContext(ctx, `SELECT full_name, first_name, push_name, business_name, display_name, name_source, phone_e164,
		saved_contact = 1 OR EXISTS (SELECT 1 FROM messages m JOIN chats c ON c.id = m.chat_id WHERE c.person_id = persons.id AND c.kind = 'dm' AND m.from_me = 1)
		FROM persons WHERE id = ?`, id).
		Scan(&full, &first, &push, &business, &display, &source, &phone, &trusted)
	if err != nil {
		return fmt.Errorf("person p_%d: %w", id, err)
	}
	r.Names = append(r.Names, full, first)
	if trusted || source == "full_name" || source == "first_name" {
		r.Names = append(r.Names, display)
	}
	if trusted {
		r.Names = append(r.Names, push, business)
	}
	r.IDs = append(r.IDs, "p_"+strconv.FormatInt(id, 10))
	if phone.Valid && phone.String != "" {
		r.Phones = append(r.Phones, strings.TrimPrefix(phone.String, "+"))
	}
	rows, err := db.QueryContext(ctx, `SELECT jid FROM jids WHERE person_id = ? AND kind = 'pn'`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var j string
		if rows.Scan(&j) == nil {
			if u, _, ok := strings.Cut(j, "@"); ok && onlyDigits(u) != "" {
				r.Phones = append(r.Phones, onlyDigits(u))
			}
		}
	}
	label := display
	if label == "" {
		label = firstNonEmpty(full, first, push, business)
	}
	if source == "push_name" || source == "business_name" {
		label = "~" + label // a name the person chose, as WhatsApp shows strangers
	}
	if phone.Valid && phone.String != "" {
		label = strings.TrimSpace(label + " (" + phone.String + ")")
	}
	if label != "" {
		r.Label = label
	}
	return rows.Err()
}

// parseRef reads "p_17" and "c_42".
func parseRef(s string) (int64, string) {
	for _, k := range []string{"p", "c"} {
		if rest, ok := strings.CutPrefix(s, k+"_"); ok {
			if n, err := strconv.ParseInt(rest, 10, 64); err == nil && n > 0 {
				return n, k
			}
		}
	}
	return 0, ""
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
		b.WriteRune(r)
	}
	return b.String()
}

// phoneDigits returns the digits of s with country code when s looks like a
// phone number (as the daemon reads one: a number without + or 00 and at
// most 10 digits is Italian), "" otherwise.
func phoneDigits(s string) string {
	n := 0
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			n++
			b.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return ""
		}
	}
	if n < 6 {
		return ""
	}
	d := strings.TrimPrefix(b.String(), "00")
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "+") && !strings.HasPrefix(t, "00") && len(d) <= 10 {
		d = "39" + d
	}
	return d
}

// nameStopwords are name words too common to identify anyone.
var nameStopwords = map[string]bool{"del": true, "dei": true, "della": true, "delle": true, "degli": true, "dal": true, "dalla": true,
	"con": true, "per": true, "the": true, "and": true, "van": true, "von": true, "der": true, "den": true, "des": true,
	"san": true, "dott": true, "dottor": true, "dottore": true, "ing": true, "avv": true, "prof": true, "sig": true, "sigra": true,
	"mrs": true, "miss": true, "group": true, "gruppo": true, "chat": true, "whatsapp": true}

// nameTokens are the words of names that can identify a recipient: at least
// three letters or digits, folded, not a stopword.
func nameTokens(names []string) []string {
	var out []string
	for _, n := range names {
		for _, w := range words(n, true) {
			if len([]rune(w)) >= 3 && !nameStopwords[w] {
				out = append(out, w)
			}
		}
	}
	return out
}

// NamedIn reports whether the prompt (folded words) names the recipient:
// any word of at least three letters of one of its names, at least the
// last six digits of one of its phone numbers, or its id.
func (r Recipient) NamedIn(prompt []string) bool {
	set := map[string]bool{}
	for _, w := range prompt {
		set[w] = true
	}
	for _, t := range nameTokens(r.Names) {
		if set[t] {
			return true
		}
	}
	runs := digitRuns(prompt)
	for _, p := range r.Phones {
		if len(p) < 6 {
			continue
		}
		tail := p[len(p)-6:]
		for _, run := range runs {
			if strings.Contains(run, tail) {
				return true
			}
		}
	}
	for _, id := range r.IDs {
		k, n, _ := strings.Cut(id, "_")
		for i := 0; i+1 < len(prompt); i++ {
			if prompt[i] == k && prompt[i+1] == n {
				return true
			}
		}
	}
	return false
}

// digitRuns joins adjacent all-digit words: "+39 333 123 4567" is one run.
func digitRuns(ws []string) []string {
	var out []string
	cur := ""
	for _, w := range ws {
		if strings.IndexFunc(w, func(r rune) bool { return !unicode.IsDigit(r) }) < 0 {
			cur += w
			continue
		}
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// fileWords are the words that mention a file in a prompt.
var fileWords = map[string]bool{"file": true, "files": true, "pdf": true, "documento": true, "documenti": true, "doc": true,
	"allegato": true, "allegati": true, "allegalo": true, "allegala": true, "foto": true, "fotografia": true, "immagine": true,
	"immagini": true, "audio": true, "vocale": true, "vocali": true, "video": true, "document": true, "documents": true,
	"attachment": true, "attachments": true, "photo": true, "photos": true, "picture": true, "pictures": true, "image": true,
	"images": true, "voice": true, "screenshot": true, "slide": true, "slides": true, "presentazione": true, "presentation": true}

// FileNamedIn reports whether the prompt (folded words) mentions the file
// at path: a word for a file (file, pdf, documento, allegato, foto,
// immagine, audio, vocale, video and their English forms), the file's
// extension, or a word of at least four letters from its name.
func FileNamedIn(path string, prompt []string) bool {
	// Either separator ends a folder name, whatever the OS the path is from.
	base := path[strings.LastIndexAny(path, `/\`)+1:]
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(base)), ".")
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	stems := map[string]bool{}
	for _, w := range words(stem, true) {
		if len([]rune(w)) >= 4 {
			stems[w] = true
		}
	}
	for _, w := range prompt {
		if fileWords[w] || (ext != "" && w == ext) || stems[w] {
			return true
		}
	}
	return false
}
