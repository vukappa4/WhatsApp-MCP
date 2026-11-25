package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseRef reads a c_N or p_N id, spaces around it ignored: kind is "c" or
// "p", and "" when ref is neither.
func ParseRef(ref string) (kind string, id int64) {
	ref = strings.TrimSpace(ref)
	for _, k := range []string{"c", "p"} {
		if rest, ok := strings.CutPrefix(ref, k+"_"); ok {
			if n, err := strconv.ParseInt(rest, 10, 64); err == nil && n > 0 {
				return k, n
			}
		}
	}
	return "", 0
}

// Destination is where a message to a c_N or p_N id goes. The send gate
// approves and the daemon sends by the same Destination.
type Destination struct {
	JID      string
	ViaPhone bool   // a person's phone JID: they have no known LID
	Kind     string // dm | group
	ChatID   int64  // the chat a c_N id names; 0 for a p_N id
	PersonID int64  // the person of a dm
	Name     string // the group's name
	Label    string // "Marco Rossi (+393331234567)", "group Calcetto (c_2)"
}

var (
	// ErrNotRef means the ref is neither a c_N nor a p_N id.
	ErrNotRef = errors.New("not a c_N or p_N id")
	// ErrUnknownRef means no chat or person has the id.
	ErrUnknownRef = errors.New("not found")
	// ErrChatNoPerson means a DM chat has no person.
	ErrChatNoPerson = errors.New("has no person")
)

// ErrChatKind means the chat is neither a person's nor a group, so it
// receives no messages.
type ErrChatKind struct{ Kind string }

func (e ErrChatKind) Error() string {
	return "is a " + e.Kind + ": only people and groups receive messages"
}

// ResolveDestination resolves a c_N or p_N id: a group chat's JID, or the
// JID SendJID picks for a person (a DM chat goes to its person). The
// Destination it returns with an error holds what was known, such as the
// person whose JIDs SendJID refused.
func ResolveDestination(ctx context.Context, e Execer, ref string) (Destination, error) {
	ref = strings.TrimSpace(ref)
	kind, id := ParseRef(ref)
	switch kind {
	case "p":
		return PersonDestination(ctx, e, id)
	case "":
		return Destination{}, fmt.Errorf("%s: %w", ref, ErrNotRef)
	}
	var chatKind, name, jid string
	var person sql.NullInt64
	err := e.QueryRowContext(ctx, `SELECT kind, person_id, name, jid FROM chats WHERE id = ?`, id).Scan(&chatKind, &person, &name, &jid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Destination{}, fmt.Errorf("chat %s: %w", ref, ErrUnknownRef)
	case err != nil:
		return Destination{}, err
	}
	switch chatKind {
	case "dm":
		if !person.Valid {
			return Destination{ChatID: id}, fmt.Errorf("chat %s %w", ref, ErrChatNoPerson)
		}
		d, err := PersonDestination(ctx, e, person.Int64)
		d.ChatID = id
		return d, err
	case "group":
		label := name
		if label == "" {
			label = "unnamed"
		}
		return Destination{JID: jid, Kind: "group", ChatID: id, Name: name, Label: "group " + label + " (" + ref + ")"}, nil
	}
	return Destination{ChatID: id}, fmt.Errorf("chat %s %w", ref, ErrChatKind{chatKind})
}

// PersonDestination is where a direct message to person id goes.
func PersonDestination(ctx context.Context, e Execer, id int64) (Destination, error) {
	ref := "p_" + strconv.FormatInt(id, 10)
	d := Destination{Kind: "dm", PersonID: id}
	var display, full, first, push, business, source string
	var phone sql.NullString
	err := e.QueryRowContext(ctx, `SELECT display_name, full_name, first_name, push_name, business_name, name_source, phone_e164
		FROM persons WHERE id = ?`, id).Scan(&display, &full, &first, &push, &business, &source, &phone)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return d, fmt.Errorf("person %s: %w", ref, ErrUnknownRef)
	case err != nil:
		return d, err
	}
	rows, err := e.QueryContext(ctx, `SELECT jid, kind FROM jids WHERE person_id = ?`, id)
	if err != nil {
		return d, err
	}
	var jids []JIDRow
	for rows.Next() {
		var j JIDRow
		if err := rows.Scan(&j.JID, &j.Kind); err != nil {
			rows.Close()
			return d, err
		}
		jids = append(jids, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}
	if d.JID, d.ViaPhone, err = SendJID(jids, phone.String); err != nil {
		return d, fmt.Errorf("%s %w", ref, err)
	}
	if d.Label = PersonLabel(display, full, first, push, business, source, phone.String); d.Label == "" {
		d.Label = ref
	}
	return d, nil
}

// PersonLabel is how the send gate shows a person: the display name, else
// another name, with "~" before a name the person chose (as WhatsApp shows
// strangers) and the current number after it: "Marco Rossi (+393331234567)".
func PersonLabel(display, full, first, push, business, source, phone string) string {
	label := display
	if label == "" {
		for _, n := range []string{full, first, push, business} {
			if n != "" {
				label = n
				break
			}
		}
	}
	if source == "push_name" || source == "business_name" {
		label = "~" + label
	}
	if phone != "" {
		label = strings.TrimSpace(label + " (" + phone + ")")
	}
	return label
}
