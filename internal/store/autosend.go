package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// AutosendChat is one approved destination: a JID and the label the user saw.
type AutosendChat struct {
	JID   string
	Label string
}

// AutosendRule lets one Desktop scheduled task send to Chats while the
// task's fingerprint stays Fingerprint.
type AutosendRule struct {
	TaskID      string
	TaskName    string
	Schedule    string
	Fingerprint string
	ApprovedAt  int64 // unix
	Chats       []AutosendChat
}

// ErrNoRule means no autosend rule matches.
var ErrNoRule = errors.New("no autosend approval")

// PutAutosendRule stores r, replacing the task's earlier rule and its chats
// in one transaction.
func PutAutosendRule(ctx context.Context, db *DB, r AutosendRule) error {
	if r.TaskID == "" || len(r.Chats) == 0 {
		return errors.New("an autosend rule needs a task and at least one chat")
	}
	return db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM autosend_rules WHERE task_id = ?`, r.TaskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO autosend_rules(task_id, task_name, schedule, fingerprint, approved_at) VALUES (?, ?, ?, ?, ?)`,
			r.TaskID, r.TaskName, r.Schedule, r.Fingerprint, r.ApprovedAt); err != nil {
			return err
		}
		for _, c := range r.Chats {
			if _, err := tx.ExecContext(ctx, `INSERT INTO autosend_chats(task_id, jid, label) VALUES (?, ?, ?)
				ON CONFLICT(task_id, jid) DO NOTHING`, r.TaskID, c.JID, c.Label); err != nil {
				return err
			}
		}
		return nil
	})
}

// AutosendRules returns every rule, newest approval first.
func AutosendRules(ctx context.Context, e Execer) ([]AutosendRule, error) {
	return autosendRules(ctx, e, ``)
}

// AutosendRuleFor returns the task's rule, or ErrNoRule.
func AutosendRuleFor(ctx context.Context, e Execer, taskID string) (AutosendRule, error) {
	rs, err := autosendRules(ctx, e, `WHERE task_id = ?`, taskID)
	if err != nil {
		return AutosendRule{}, err
	}
	if len(rs) == 0 {
		return AutosendRule{}, ErrNoRule
	}
	return rs[0], nil
}

func autosendRules(ctx context.Context, e Execer, where string, args ...any) ([]AutosendRule, error) {
	rows, err := e.QueryContext(ctx, `SELECT task_id, task_name, schedule, fingerprint, approved_at FROM autosend_rules `+where+`
		ORDER BY approved_at DESC, task_id`, args...)
	if err != nil {
		return nil, err
	}
	var out []AutosendRule
	for rows.Next() {
		var r AutosendRule
		if err := rows.Scan(&r.TaskID, &r.TaskName, &r.Schedule, &r.Fingerprint, &r.ApprovedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		crow, err := e.QueryContext(ctx, `SELECT jid, label FROM autosend_chats WHERE task_id = ? ORDER BY rowid`, out[i].TaskID)
		if err != nil {
			return nil, err
		}
		for crow.Next() {
			var c AutosendChat
			if err := crow.Scan(&c.JID, &c.Label); err != nil {
				crow.Close()
				return nil, err
			}
			out[i].Chats = append(out[i].Chats, c)
		}
		crow.Close()
		if err := crow.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteAutosendRule deletes the rule of the task with id ref, else of the
// one task named ref (case ignored), and returns it.
func DeleteAutosendRule(ctx context.Context, db *DB, ref string) (AutosendRule, error) {
	ref = strings.TrimSpace(ref)
	var deleted AutosendRule
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		rs, err := autosendRules(ctx, tx, `WHERE task_id = ?`, ref)
		if err != nil {
			return err
		}
		if len(rs) == 0 {
			if rs, err = autosendRules(ctx, tx, `WHERE task_name = ? COLLATE NOCASE`, ref); err != nil {
				return err
			}
		}
		switch {
		case len(rs) == 0:
			return fmt.Errorf("task %q: %w", ref, ErrNoRule)
		case len(rs) > 1:
			return fmt.Errorf("%q names %d approved tasks: use the task id", ref, len(rs))
		}
		deleted = rs[0]
		_, err = tx.ExecContext(ctx, `DELETE FROM autosend_rules WHERE task_id = ?`, deleted.TaskID)
		return err
	})
	return deleted, err
}

// ErrManyLIDs means a person holds more than one WhatsApp account (LID), so
// which one a message is for is unknown.
type ErrManyLIDs struct{ N int }

func (e ErrManyLIDs) Error() string {
	return fmt.Sprintf("holds %d WhatsApp accounts (LIDs)", e.N)
}

// ErrNoSendJID means a person has neither a LID nor a phone number.
var ErrNoSendJID = errors.New("has no phone number or WhatsApp id stored")

// SendJID is the JID a direct message to a person goes to: their one LID,
// else their phone number's JID (viaPhone). A stored number can be out of
// date or given to someone else, so the account (LID) wins. Two or more
// LIDs are ErrManyLIDs.
func SendJID(jids []JIDRow, phoneE164 string) (jid string, viaPhone bool, err error) {
	var lids []string
	for _, j := range jids {
		if j.Kind == "lid" {
			lids = append(lids, j.JID)
		}
	}
	switch {
	case len(lids) > 1:
		return "", false, ErrManyLIDs{len(lids)}
	case len(lids) == 1:
		return lids[0], false, nil
	}
	if digits := strings.TrimPrefix(phoneE164, "+"); digits != "" {
		return digits + "@s.whatsapp.net", true, nil
	}
	return "", false, ErrNoSendJID
}
