package mcpserver

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Europe/Rome without relying on the OS zone database
	"unicode"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

var rome = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		panic(err)
	}
	return loc
}()

func local(ts int64) time.Time { return time.Unix(ts, 0).In(rome) }

// fmtTS renders a timestamp in Europe/Rome as of now: "13 Oct 10:40", with
// the year when it is not now's year ("13 Oct 2025 10:40") and the UTC offset
// only where the wall time is ambiguous (see clock). A zero now always shows
// the year: use it for text that is stored and shown again later.
func fmtTS(ts int64, now time.Time) string {
	if ts == 0 {
		return ""
	}
	return fmtTime(local(ts), now)
}

// fmtShort renders "08:02" for today and fmtTS's form otherwise.
func fmtShort(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	t, now = t.In(rome), now.In(rome)
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return clock(t)
	}
	return fmtTime(t, now)
}

func fmtTime(t, now time.Time) string {
	t = t.In(rome)
	return t.Format(withFold(t, withYear(t, "2 Jan", now)+" 15:04"))
}

// fmtDay renders a day header: "Tue 13 Oct", with the year when it is not
// now's year.
func fmtDay(ts int64, now time.Time) string {
	t := local(ts)
	return t.Format(withYear(t, "Mon 2 Jan", now))
}

func withYear(t time.Time, layout string, now time.Time) string {
	if now.IsZero() || t.Year() != now.In(rome).Year() {
		return layout + " 2006"
	}
	return layout
}

// clock renders "10:40". In the hour that repeats when daylight saving time
// ends, the only time a Europe/Rome wall time names two instants, the UTC
// offset follows: "02:30 +02:00", then "02:30 +01:00".
func clock(t time.Time) string {
	t = t.In(rome)
	return t.Format(withFold(t, "15:04"))
}

// withFold appends the UTC offset to a layout ending in the time of day when
// t falls in a repeated hour.
func withFold(t time.Time, layout string) string {
	ts := t.Unix()
	i := sort.Search(len(romeFolds), func(i int) bool { return romeFolds[i][1] > ts })
	if i < len(romeFolds) && romeFolds[i][0] <= ts {
		return layout + " -07:00"
	}
	return layout
}

// romeFolds lists, in order, the unix ranges [from, to) whose Europe/Rome
// wall times repeat: around each change to a smaller offset, the span of the
// change on both sides.
var romeFolds = func() [][2]int64 {
	var folds [][2]int64
	for t := time.Date(1970, 1, 1, 0, 0, 0, 0, rome); t.Year() < 2200; {
		_, end := t.ZoneBounds()
		if end.IsZero() {
			break
		}
		if !end.After(t) { // ZoneBounds returns end == t on 31 Dec of leap years from 1996
			t = t.Add(time.Hour)
			continue
		}
		_, before := t.Zone()
		_, after := end.Zone()
		if d := int64(before - after); d > 0 {
			folds = append(folds, [2]int64{end.Unix() - d, end.Unix() + d})
		}
		t = end
	}
	return folds
}()

// parseTime is parseTimeAt now.
func parseTime(s string) (time.Time, error) { return parseTimeAt(s, time.Now()) }

// parseTimeAt accepts ISO 8601 with any offset, written +02:00, +0200 or +02.
// Values without an offset, and bare dates, are Europe/Rome; a time alone
// (09:00, 9:05, 09:00:30) is that time today in Europe/Rome, as of now.
func parseTimeAt(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, date := range []string{"2006-01-02T", "2006-01-02 "} {
		for _, clock := range []string{"15:04:05.999999999", "15:04"} {
			for _, zone := range []string{"Z07:00", "Z0700", "Z07"} {
				if t, err := time.Parse(date+clock+zone, s); err == nil {
					return t, nil
				}
			}
		}
	}
	for _, l := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, s, rome); err == nil {
			return t, nil
		}
	}
	for _, l := range []string{"15:04:05", "15:04"} {
		if c, err := time.Parse(l, s); err == nil {
			y, m, d := now.In(rome).Date()
			return time.Date(y, m, d, c.Hour(), c.Minute(), c.Second(), 0, rome), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q: use ISO 8601, e.g. 2026-10-13T09:00:00+02:00, 2026-10-13, or 09:00 for today", s)
}

// tagName matches the wrapper tag name in any case inside sender text, and
// look-alikes such as untrusted_whatsapp. sanitize keeps the zero-width joiner,
// combining marks (variation selectors, accents) and Hangul fillers, which
// render as nothing between letters, so the pattern skips them.
var tagName = regexp.MustCompile(`(?i)` + strings.Join(strings.Split("untrusted_w", ""), `[\x{200D}\p{M}\x{115F}\x{1160}\x{3164}\x{FFA0}]*`))

// sanitize strips control, invisible and bidi formatting characters (keeping
// newlines, tabs and the emoji zero-width joiner) and defuses the wrapper tag.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
		case r == '\r':
			continue
		case unicode.IsControl(r):
			continue
		case r == 0x200D: // ZWJ joins emoji sequences
		case unicode.Is(unicode.Cf, r): // ZWSP, LRM/RLM, LRE..RLO, isolates, BOM, soft hyphen, tag chars...
			continue
		case r == 0x2028 || r == 0x2029:
			r = '\n'
		}
		b.WriteRune(r)
	}
	out := b.String()
	if !strings.Contains(out, "_") { // every tagName match holds an underscore; the regex is slow
		return out
	}
	return tagName.ReplaceAllString(out, "untrusted-w")
}

// wrapper marks sender-written text with a per-response nonce.
// transcripts shows voice-note transcripts in concise lines too.
type wrapper struct {
	nonce       string
	transcripts bool
	capText     int       // detailed mode: most runes of a body or transcript, 0 for no cap
	now         time.Time // times omit now's year
}

func newWrapper(now time.Time) wrapper {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return wrapper{nonce: hex.EncodeToString(buf), now: now}
}

// ts is fmtTS as of the response's now.
func (w wrapper) ts(ts int64) string { return fmtTS(ts, w.now) }

func (w wrapper) wrap(s string) string {
	s = sanitize(s)
	if s == "" {
		return ""
	}
	return "<untrusted_wa n=" + w.nonce + ">" + s + "</untrusted_wa n=" + w.nonce + ">"
}

// name renders a person's name: plain from the address book, "~Name" (cleaned
// and capped, like WhatsApp shows strangers) when the person chose it.
func (w wrapper) name(p *store.PersonRef) string {
	switch {
	case p == nil:
		return "unknown"
	case p.IsMe:
		return "me"
	case people.TrustedSource(p.Source) && p.Source != people.SourcePhone && p.Name != "":
		return sanitize(p.Name)
	}
	if p.Source != people.SourcePhone {
		if n := people.ProfileName(p.Name); n != "" {
			return n
		}
	}
	if p.Phone != "" {
		return people.FormatPhone(p.Phone)
	}
	return "unknown (" + personID(p.ID) + ")"
}

// label renders "Marco Rossi (+39 333 123 4567)".
func (w wrapper) label(p *store.PersonRef) string {
	n := w.name(p)
	if p == nil || p.IsMe || p.Source == people.SourcePhone || p.Source == people.SourceSystem || p.Name == "" {
		return n
	}
	if p.Phone == "" {
		return n + " (phone unknown)"
	}
	return n + " (" + people.FormatPhone(p.Phone) + ")"
}

func personID(id int64) string  { return "p_" + strconv.FormatInt(id, 10) }
func chatID(id int64) string    { return "c_" + strconv.FormatInt(id, 10) }
func messageID(id int64) string { return "m_" + strconv.FormatInt(id, 10) }

// parseID reads "p_17" style ids; ok is false when s has another shape.
func parseID(s, prefix string) (int64, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(s), prefix)
	if !found {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil && n > 0
}

// cursor is the opaque paging state. Tool binds it to the tool that issued it.
type cursor struct {
	Tool   string `json:"t"`
	Chat   int64  `json:"c,omitempty"`
	Newer  bool   `json:"n,omitempty"`
	TS     int64  `json:"ts"`
	ID     int64  `json:"id"`
	After  int64  `json:"a,omitempty"`
	Before int64  `json:"b,omitempty"`
	Kind   string `json:"k,omitempty"`
	Unread bool   `json:"u,omitempty"`
	Query  string `json:"q,omitempty"`
	Person int64  `json:"p,omitempty"`
	Media  string `json:"md,omitempty"`
	Mine   int    `json:"fm,omitempty"`
	Sort   string `json:"s,omitempty"`
	Ctx    int    `json:"x,omitempty"`
	Off    int    `json:"o,omitempty"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s, tool string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err == nil && c.Off < 0 {
		err = errors.New("negative offset") // a forged cursor; a negative slice bound panics the daemon
	}
	if err != nil {
		return c, errors.New("invalid cursor: pass back the cursor string exactly as returned")
	}
	if c.Tool != tool {
		return c, fmt.Errorf("this cursor belongs to %s, not %s", c.Tool, tool)
	}
	return c, nil
}

func humanSize(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// mimePattern is the shape of a media type; anything else in a mimetype is
// sender-controlled text.
var mimePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]{0,63}/[a-z0-9][a-z0-9.+-]{0,63}$`)

// safeMime returns the media type of a sender-supplied mimetype without
// parameters, "" when it does not look like one.
func safeMime(s string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(s, ";", 2)[0]))
	if !mimePattern.MatchString(base) {
		return ""
	}
	return base
}
