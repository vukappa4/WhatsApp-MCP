// Package people maps every WhatsApp user JID (phone number or LID) to one
// person, links each LID with the phone numbers it had, and picks the
// display name and the current number.
package people

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/nyaruka/phonenumbers"
	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// Kind of a user JID.
type Kind string

const (
	KindPN    Kind = "pn"
	KindLID   Kind = "lid"
	KindOther Kind = "other"
)

// ErrNotUser is returned for JIDs that do not name a person (groups, newsletters...).
var ErrNotUser = errors.New("not a user JID")

// ErrTwoAccounts is returned by Merge for two persons that each hold a LID:
// a LID is one WhatsApp account, so they are two people.
var ErrTwoAccounts = errors.New("persons hold different LIDs")

// Normalize strips device and agent parts. A hosted (business API) device
// names its account, as whatsmeow maps it for direct messages. ok is false
// for non-user JIDs.
func Normalize(j types.JID) (types.JID, Kind, bool) {
	if j.User == "" {
		return types.JID{}, "", false
	}
	switch j.Server {
	case types.DefaultUserServer, types.LegacyUserServer, types.HostedServer:
		return types.NewJID(j.User, types.DefaultUserServer), KindPN, true
	case types.HiddenUserServer, types.HostedLIDServer:
		return types.NewJID(j.User, types.HiddenUserServer), KindLID, true
	case types.BotServer:
		return types.NewJID(j.User, types.BotServer), KindOther, true
	}
	return types.JID{}, "", false
}

// IsUser reports whether j names a person.
func IsUser(j types.JID) bool { _, _, ok := Normalize(j); return ok }

// Lookup returns the person of a JID without creating one; 0 when unknown.
func Lookup(ctx context.Context, e store.Execer, j types.JID) (int64, error) {
	n, _, ok := Normalize(j)
	if !ok {
		return 0, ErrNotUser
	}
	var id int64
	err := e.QueryRowContext(ctx, `SELECT person_id FROM jids WHERE jid = ?`, n.String()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Resolve returns the person of a JID, creating it when unknown.
func Resolve(ctx context.Context, e store.Execer, j types.JID) (int64, error) {
	n, kind, ok := Normalize(j)
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNotUser, j)
	}
	id, err := Lookup(ctx, e, n)
	if err != nil || id != 0 {
		return id, err
	}
	var phone any
	if kind == KindPN {
		phone = "+" + n.User
	}
	res, err := e.ExecContext(ctx, `INSERT INTO persons(phone_e164, updated_at) VALUES (?, ?)`, phone, store.Now().Unix())
	if err != nil {
		return 0, err
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	if _, err := e.ExecContext(ctx, `INSERT INTO jids(jid, person_id, kind) VALUES (?, ?, ?)`, n.String(), id, string(kind)); err != nil {
		return 0, err
	}
	return id, recompute(ctx, e, id)
}

// Link records that a and b were one WhatsApp account at time at: a LID and
// a phone number it had, in either order. A LID is one account, so a person
// holds at most one LID and Link never merges two of them:
//   - The number belongs to the LID that had it last: the newest seen_at in
//     number_links, the holder keeping it on a tie. When that is this LID,
//     the number joins its person: a person known only by that number merges
//     into it, and a number another LID's person holds moves over (WhatsApp
//     gave the number to another account), noted as "number reassigned".
//   - An older observation only records a former number of this LID.
//
// The numbers each person ends with depend only on the observations, not on
// their order. Any other pair (two numbers, two LIDs, a bot) proves nothing:
// both are resolved and nothing is linked. Empty or non-user JIDs are
// ignored. It returns the person of the LID, else that of a (of b when a is
// not a user).
func Link(ctx context.Context, e store.Execer, a, b types.JID, at time.Time) (int64, error) {
	l, pn, ok := numberPair(a, b)
	if !ok {
		return resolveEach(ctx, e, a, b)
	}
	res, err := e.ExecContext(ctx, `INSERT INTO number_links(pn, lid, seen_at) VALUES (?, ?, ?)
		ON CONFLICT (pn, lid) DO UPDATE SET seen_at = excluded.seen_at WHERE excluded.seen_at > number_links.seen_at`,
		pn.String(), l.String(), at.Unix())
	if err != nil {
		return 0, fmt.Errorf("record link %s %s: %w", l, pn, err)
	}
	changed, _ := res.RowsAffected()
	return claim(ctx, e, l, pn, changed > 0)
}

// LinkCurrent records that pn is the LID's number now, from a source without
// times of its own (whatsmeow's LID map, a history blob's mappings, the
// account's own JIDs): when the pair is recorded and the LID's person holds
// the number, it writes nothing; otherwise it is Link at time at. Seeing a
// known pair again thus never moves a number or changes a current one.
func LinkCurrent(ctx context.Context, e store.Execer, a, b types.JID, at time.Time) (int64, error) {
	if l, pn, ok := numberPair(a, b); ok {
		var id int64
		err := e.QueryRowContext(ctx, `SELECT jl.person_id FROM number_links n
			JOIN jids jl ON jl.jid = n.lid JOIN jids jp ON jp.jid = n.pn AND jp.person_id = jl.person_id
			WHERE n.pn = ? AND n.lid = ?`, pn.String(), l.String()).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	return Link(ctx, e, a, b, at)
}

// numberPair returns the LID and the phone JID among a and b; ok is false
// unless one is a LID and the other a phone number.
func numberPair(a, b types.JID) (l, pn types.JID, ok bool) {
	na, ka, aok := Normalize(a)
	nb, kb, bok := Normalize(b)
	switch {
	case !aok || !bok:
	case ka == KindLID && kb == KindPN:
		return na, nb, true
	case ka == KindPN && kb == KindLID:
		return nb, na, true
	}
	return types.JID{}, types.JID{}, false
}

// resolveEach resolves the user JIDs among a and b without linking them and
// returns the person of a, or of b when a is not a user.
func resolveEach(ctx context.Context, e store.Execer, a, b types.JID) (int64, error) {
	aok, bok := IsUser(a), IsUser(b)
	switch {
	case !aok && !bok:
		return 0, fmt.Errorf("%w: %s / %s", ErrNotUser, a, b)
	case !aok:
		return Resolve(ctx, e, b)
	}
	pa, err := Resolve(ctx, e, a)
	if err != nil || !bok {
		return pa, err
	}
	_, err = Resolve(ctx, e, b)
	return pa, err
}

// claim gives pn to the person of l when l had it last (see Link). changed
// says whether the link's time moved, which may change the current number.
func claim(ctx context.Context, e store.Execer, l, pn types.JID, changed bool) (int64, error) {
	pl, err := Resolve(ctx, e, l)
	if err != nil {
		return 0, err
	}
	px, err := Lookup(ctx, e, pn)
	if err != nil {
		return 0, err
	}
	switch {
	case px == 0:
		if _, err := e.ExecContext(ctx, `INSERT INTO jids(jid, person_id, kind) VALUES (?, ?, 'pn')`, pn.String(), pl); err != nil {
			return 0, err
		}
		return pl, recompute(ctx, e, pl)
	case px == pl && changed:
		return pl, recompute(ctx, e, pl)
	case px == pl:
		return pl, nil
	}
	holder, err := personLID(ctx, e, px)
	if err != nil {
		return 0, err
	}
	if holder == "" {
		return Merge(ctx, e, pl, px)
	}
	var newest bool
	if err := e.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM number_links o WHERE o.pn = ?1 AND o.lid != ?2
		AND o.seen_at >= (SELECT seen_at FROM number_links WHERE pn = ?1 AND lid = ?2))`, pn.String(), l.String()).Scan(&newest); err != nil {
		return 0, err
	}
	if !newest {
		// An older claim: the number stays with the account that had it last.
		return pl, noteReassigned(ctx, e, pn, holder)
	}
	if _, err := e.ExecContext(ctx, `UPDATE jids SET person_id = ? WHERE jid = ?`, pl, pn.String()); err != nil {
		return 0, fmt.Errorf("move %s to person %d: %w", pn, pl, err)
	}
	if err := noteReassigned(ctx, e, pn, l.String()); err != nil {
		return 0, err
	}
	if err := recompute(ctx, e, px); err != nil {
		return 0, err
	}
	return pl, recompute(ctx, e, pl)
}

// noteReassigned records that the number pn belongs to the LID owner since
// the time of that evidence. The note is the same whichever claim came first.
func noteReassigned(ctx context.Context, e store.Execer, pn types.JID, owner string) error {
	_, err := e.ExecContext(ctx, `INSERT OR IGNORE INTO identity_notes(jid, other_jid, note, ts)
		SELECT ?1, ?2, 'number reassigned', seen_at FROM number_links WHERE pn = ?1 AND lid = ?2`, pn.String(), owner)
	return err
}

// personLID returns the LID a person holds, "" when it holds none.
func personLID(ctx context.Context, e store.Execer, id int64) (string, error) {
	var j string
	err := e.QueryRowContext(ctx, `SELECT jid FROM jids WHERE person_id = ? AND kind = 'lid' ORDER BY jid LIMIT 1`, id).Scan(&j)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return j, err
}

type person struct {
	id                                 int64
	phone                              sql.NullString
	isMe, saved                        bool
	fullName, firstName, biz, pushName string
}

func load(ctx context.Context, e store.Execer, id int64) (person, error) {
	p := person{id: id}
	err := e.QueryRowContext(ctx, `SELECT phone_e164, is_me, saved_contact, full_name, first_name, business_name, push_name
		FROM persons WHERE id = ?`, id).Scan(&p.phone, &p.isMe, &p.saved, &p.fullName, &p.firstName, &p.biz, &p.pushName)
	if err != nil {
		return p, fmt.Errorf("load person %d: %w", id, err)
	}
	return p, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Merge folds two persons into one and returns the survivor: the "me"
// person if either is me, else the older (lower id) one. All JIDs, sent
// messages, reactions and the DM chat of the other person move to it. Two
// persons that both hold a LID are two accounts: Merge refuses them
// (ErrTwoAccounts).
func Merge(ctx context.Context, e store.Execer, x, y int64) (int64, error) {
	if x == y {
		return x, nil
	}
	lx, err := personLID(ctx, e, x)
	if err != nil {
		return 0, err
	}
	ly, err := personLID(ctx, e, y)
	if err != nil {
		return 0, err
	}
	if lx != "" && ly != "" {
		return 0, fmt.Errorf("%w: persons %d (%s) and %d (%s)", ErrTwoAccounts, x, lx, y, ly)
	}
	px, err := load(ctx, e, x)
	if err != nil {
		return 0, err
	}
	py, err := load(ctx, e, y)
	if err != nil {
		return 0, err
	}
	keep, drop := px, py
	if (py.isMe && !px.isMe) || (py.isMe == px.isMe && py.id < px.id) {
		keep, drop = py, px
	}

	steps := []struct {
		q    string
		args []any
	}{
		{`UPDATE jids SET person_id = ? WHERE person_id = ?`, []any{keep.id, drop.id}},
		{`UPDATE messages SET sender_person_id = ? WHERE sender_person_id = ?`, []any{keep.id, drop.id}},
		// Both persons reacted to the same message: keep the newest reaction.
		{`DELETE FROM reactions WHERE person_id = ? AND EXISTS (
			SELECT 1 FROM reactions r WHERE r.message_id = reactions.message_id AND r.person_id = ? AND r.ts >= reactions.ts)`,
			[]any{drop.id, keep.id}},
		{`DELETE FROM reactions WHERE person_id = ? AND EXISTS (
			SELECT 1 FROM reactions r WHERE r.message_id = reactions.message_id AND r.person_id = ?)`,
			[]any{keep.id, drop.id}},
		{`UPDATE reactions SET person_id = ? WHERE person_id = ?`, []any{keep.id, drop.id}},
	}
	for _, s := range steps {
		if _, err := e.ExecContext(ctx, s.q, s.args...); err != nil {
			return 0, fmt.Errorf("merge persons %d<-%d: %w", keep.id, drop.id, err)
		}
	}

	keepChat, err := DMChatID(ctx, e, keep.id)
	if err != nil {
		return 0, err
	}
	dropChat, err := DMChatID(ctx, e, drop.id)
	if err != nil {
		return 0, err
	}
	switch {
	case dropChat != 0 && keepChat == 0:
		if _, err := e.ExecContext(ctx, `UPDATE chats SET person_id = ? WHERE id = ?`, keep.id, dropChat); err != nil {
			return 0, err
		}
	case dropChat != 0:
		if err := MergeChats(ctx, e, keepChat, dropChat); err != nil {
			return 0, err
		}
	}

	if _, err := e.ExecContext(ctx, `DELETE FROM persons WHERE id = ?`, drop.id); err != nil {
		return 0, fmt.Errorf("delete merged person %d: %w", drop.id, err)
	}
	_, err = e.ExecContext(ctx, `UPDATE persons SET is_me = ?, saved_contact = ?,
		full_name = ?, first_name = ?, business_name = ?, push_name = ? WHERE id = ?`,
		keep.isMe || drop.isMe, keep.saved || drop.saved,
		firstNonEmpty(keep.fullName, drop.fullName), firstNonEmpty(keep.firstName, drop.firstName),
		firstNonEmpty(keep.biz, drop.biz), firstNonEmpty(keep.pushName, drop.pushName), keep.id)
	if err != nil {
		return 0, err
	}
	return keep.id, recompute(ctx, e, keep.id)
}

// MergeChats moves every message of chat drop into chat keep and deletes
// drop. A message stored in both (same wa_id) is merged field by field with
// store.MergeMessageRows, so no edit, deletion, reply, media or reaction is lost.
func MergeChats(ctx context.Context, e store.Execer, keep, drop int64) error {
	if _, err := e.ExecContext(ctx, `UPDATE OR IGNORE messages SET chat_id = ? WHERE chat_id = ?`, keep, drop); err != nil {
		return fmt.Errorf("merge chats %d<-%d: %w", keep, drop, err)
	}
	rows, err := e.QueryContext(ctx, `SELECT k.id, d.id FROM messages d JOIN messages k ON k.chat_id = ? AND k.wa_id = d.wa_id
		WHERE d.chat_id = ?`, keep, drop)
	if err != nil {
		return err
	}
	var pairs [][2]int64
	for rows.Next() {
		var p [2]int64
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range pairs {
		if err := store.MergeMessageRows(ctx, e, p[0], p[1]); err != nil {
			return fmt.Errorf("merge duplicate message %d<-%d: %w", p[0], p[1], err)
		}
	}
	steps := []struct {
		q    string
		args []any
	}{
		{`UPDATE chats SET
			last_message_at = MAX(COALESCE(last_message_at, 0), COALESCE((SELECT last_message_at FROM chats WHERE id = ?1), 0)),
			unread_count    = unread_count + (SELECT unread_count FROM chats WHERE id = ?1),
			muted_until     = MAX(COALESCE(muted_until, 0), COALESCE((SELECT muted_until FROM chats WHERE id = ?1), 0)),
			-- An app-state flag comes from the newer action; without any, from the
			-- chat a snapshot or live event set (state_updated_at).
			archived        = CASE
				WHEN COALESCE(archived_set_at, 0) < COALESCE((SELECT archived_set_at FROM chats WHERE id = ?1), 0) THEN (SELECT archived FROM chats WHERE id = ?1)
				WHEN archived_set_at IS NULL AND state_updated_at IS NULL THEN (SELECT archived FROM chats WHERE id = ?1)
				ELSE archived END,
			pinned          = CASE
				WHEN COALESCE(pinned_set_at, 0) < COALESCE((SELECT pinned_set_at FROM chats WHERE id = ?1), 0) THEN (SELECT pinned FROM chats WHERE id = ?1)
				WHEN pinned_set_at IS NULL AND state_updated_at IS NULL THEN (SELECT pinned FROM chats WHERE id = ?1)
				ELSE pinned END,
			archived_set_at = MAX(COALESCE(archived_set_at, 0), COALESCE((SELECT archived_set_at FROM chats WHERE id = ?1), 0)),
			pinned_set_at   = MAX(COALESCE(pinned_set_at, 0), COALESCE((SELECT pinned_set_at FROM chats WHERE id = ?1), 0)),
			muted_set_at    = MAX(COALESCE(muted_set_at, 0), COALESCE((SELECT muted_set_at FROM chats WHERE id = ?1), 0)),
			read_set_at     = MAX(COALESCE(read_set_at, 0), COALESCE((SELECT read_set_at FROM chats WHERE id = ?1), 0)),
			state_updated_at = MAX(COALESCE(state_updated_at, 0), COALESCE((SELECT state_updated_at FROM chats WHERE id = ?1), 0)),
			name            = CASE WHEN name = '' THEN (SELECT name FROM chats WHERE id = ?1) ELSE name END,
			description     = CASE WHEN description = '' THEN (SELECT description FROM chats WHERE id = ?1) ELSE description END
		  WHERE id = ?2`, []any{drop, keep}},
		{`UPDATE chats SET last_message_at = NULLIF(last_message_at, 0), muted_until = NULLIF(muted_until, 0),
			state_updated_at = NULLIF(state_updated_at, 0), archived_set_at = NULLIF(archived_set_at, 0),
			pinned_set_at = NULLIF(pinned_set_at, 0), muted_set_at = NULLIF(muted_set_at, 0),
			read_set_at = NULLIF(read_set_at, 0) WHERE id = ?`, []any{keep}},
		{`DELETE FROM chats WHERE id = ?`, []any{drop}},
	}
	for _, s := range steps {
		if _, err := e.ExecContext(ctx, s.q, s.args...); err != nil {
			return fmt.Errorf("merge chats %d<-%d: %w", keep, drop, err)
		}
	}
	return nil
}

// DMChatID returns the DM chat of a person, 0 when none exists.
func DMChatID(ctx context.Context, e store.Execer, personID int64) (int64, error) {
	var id int64
	err := e.QueryRowContext(ctx, `SELECT id FROM chats WHERE person_id = ? AND kind = 'dm'`, personID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// EnsureDMChat returns the person's DM chat, creating it on jid when missing.
// A number WhatsApp gave to another account can still name the chat of its
// former owner: the new chat then takes the person's LID.
func EnsureDMChat(ctx context.Context, e store.Execer, personID int64, jid types.JID) (int64, error) {
	id, err := DMChatID(ctx, e, personID)
	if err != nil || id != 0 {
		return id, err
	}
	n, _, ok := Normalize(jid)
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNotUser, jid)
	}
	chatJID := n.String()
	var taken bool
	if err := e.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM chats WHERE jid = ?)`, chatJID).Scan(&taken); err != nil {
		return 0, err
	}
	if taken {
		l, err := personLID(ctx, e, personID)
		if err != nil {
			return 0, err
		}
		if l != "" {
			chatJID = l
		}
	}
	res, err := e.ExecContext(ctx, `INSERT INTO chats(jid, kind, person_id, created_at) VALUES (?, 'dm', ?, ?)`,
		chatJID, personID, store.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("create dm chat %s: %w", n, err)
	}
	return res.LastInsertId()
}

// MarkMe flags the person as the account owner.
func MarkMe(ctx context.Context, e store.Execer, personID int64) error {
	_, err := e.ExecContext(ctx, `UPDATE persons SET is_me = 1 WHERE id = ?`, personID)
	return err
}

// Names carries name candidates; empty fields leave the stored value alone.
type Names struct {
	FullName, FirstName, BusinessName, PushName string
}

// SetNames stores non-empty name candidates and recomputes the display name.
func SetNames(ctx context.Context, e store.Execer, personID int64, n Names) error {
	n.FullName, n.FirstName = strings.TrimSpace(n.FullName), strings.TrimSpace(n.FirstName)
	n.BusinessName, n.PushName = strings.TrimSpace(n.BusinessName), strings.TrimSpace(n.PushName)
	if n == (Names{}) {
		return nil
	}
	res, err := e.ExecContext(ctx, `UPDATE persons SET
		full_name     = CASE WHEN ?1 != '' THEN ?1 ELSE full_name END,
		first_name    = CASE WHEN ?2 != '' THEN ?2 ELSE first_name END,
		business_name = CASE WHEN ?3 != '' THEN ?3 ELSE business_name END,
		push_name     = CASE WHEN ?4 != '' THEN ?4 ELSE push_name END
		WHERE id = ?5 AND (
			(?1 != '' AND full_name != ?1) OR (?2 != '' AND first_name != ?2) OR
			(?3 != '' AND business_name != ?3) OR (?4 != '' AND push_name != ?4))`,
		n.FullName, n.FirstName, n.BusinessName, n.PushName, personID)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return nil
	}
	return recompute(ctx, e, personID)
}

// Name sources, in priority order.
const (
	SourceFullName     = "full_name"
	SourceFirstName    = "first_name"
	SourceBusinessName = "business_name"
	SourcePushName     = "push_name"
	SourcePhone        = "phone"
	// SourceSystem names WhatsApp's own account (0@s.whatsapp.net), which
	// sends security and service notices.
	SourceSystem = "system"
)

// systemPhone is the phone_e164 of 0@s.whatsapp.net.
const systemPhone = "+0"

// DisplayName applies the priority: saved full name, first name, business
// name, push name, phone number.
func DisplayName(full, first, business, push, phoneE164 string) (name, source string) {
	switch {
	case full != "":
		return full, SourceFullName
	case first != "":
		return first, SourceFirstName
	case business != "":
		return business, SourceBusinessName
	case push != "":
		return push, SourcePushName
	case phoneE164 != "":
		return FormatPhone(phoneE164), SourcePhone
	}
	return "", ""
}

// TrustedSource reports whether a name source was written by the account
// owner (address book) or fixed by this program, rather than by the other person.
func TrustedSource(source string) bool {
	return source == SourceFullName || source == SourceFirstName || source == SourcePhone || source == SourceSystem
}

// FormatPhone renders +393331234567 as "+39 333 123 4567".
func FormatPhone(e164 string) string {
	num, err := phonenumbers.Parse(e164, "")
	if err != nil {
		return e164
	}
	return phonenumbers.Format(num, phonenumbers.INTERNATIONAL)
}

// IsPlausiblePhone reports whether digits form a valid phone number.
func IsPlausiblePhone(digits string) bool {
	num, err := phonenumbers.Parse("+"+digits, "")
	return err == nil && phonenumbers.IsValidNumber(num)
}

// recompute derives the person's current number and display name.
func recompute(ctx context.Context, e store.Execer, id int64) error {
	p, err := load(ctx, e, id)
	if err != nil {
		return err
	}
	if p.phone, err = currentPhone(ctx, e, id, p.phone.String); err != nil {
		return err
	}
	name, source := DisplayName(p.fullName, p.firstName, p.biz, p.pushName, p.phone.String)
	if p.phone.String == systemPhone && source != SourceFullName && source != SourceFirstName {
		name, source = "WhatsApp", SourceSystem // never a profile name chosen by whoever writes from it
	}
	saved := p.fullName != "" || p.firstName != ""
	_, err = e.ExecContext(ctx, `UPDATE persons SET phone_e164 = ?, display_name = ?, name_source = ?, saved_contact = ?, updated_at = ?
		WHERE id = ?`, p.phone, name, source, saved, store.Now().Unix(), id)
	return err
}

// currentPhone picks the person's current number among the phone JIDs it
// holds: the one its LID was seen with last. Numbers without a link to the
// LID (the one number of a person without a LID) come after linked ones;
// ties keep the stored number, then take the lowest. NULL when the person
// holds no number.
func currentPhone(ctx context.Context, e store.Execer, id int64, stored string) (sql.NullString, error) {
	var jid string
	err := e.QueryRowContext(ctx, `SELECT j.jid FROM jids j
		LEFT JOIN number_links n ON n.pn = j.jid AND n.lid IN (SELECT jid FROM jids WHERE person_id = ?1 AND kind = 'lid')
		WHERE j.person_id = ?1 AND j.kind = 'pn'
		GROUP BY j.jid
		ORDER BY MAX(n.seen_at) IS NULL, MAX(n.seen_at) DESC, j.jid = ?2 DESC, j.jid
		LIMIT 1`, id, strings.TrimPrefix(stored, "+")+"@"+types.DefaultUserServer).Scan(&jid)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullString{}, nil
	}
	if err != nil {
		return sql.NullString{}, err
	}
	user, _, _ := strings.Cut(jid, "@")
	return sql.NullString{String: "+" + user, Valid: true}, nil
}

// ProfileName renders a self-chosen WhatsApp profile (push or business) name
// the way WhatsApp shows people outside the address book: "~Name". Control,
// invisible and bidi characters, newlines, angle brackets, double quotes and
// backslashes are removed, spaces collapsed, and the name capped at 40
// characters. Tool output shows the name bare beside quoted sender text, so
// it must not be able to open or close a quote.
func ProfileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '<' || r == '>' || r == '"' || r == '\\':
			continue
		case r == '\n' || r == '\r' || r == '\t' || r == 0x2028 || r == 0x2029:
			b.WriteRune(' ')
		case unicode.IsControl(r):
			continue
		case unicode.Is(unicode.Cf, r) && r != 0x200D: // keep the emoji zero-width joiner
			continue
		default:
			b.WriteRune(r)
		}
	}
	clean := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(clean); len(r) > 40 {
		clean = strings.TrimSpace(string(r[:39])) + "…"
	}
	if clean == "" {
		return ""
	}
	return "~" + clean
}
