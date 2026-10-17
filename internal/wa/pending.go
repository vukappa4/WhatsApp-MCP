package wa

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// PendingTTL is how long an edit, revoke, reaction or poll vote waits for
// the message it acts on, counted from its own time.
const PendingTTL = 30 * 24 * time.Hour

// errNoTarget means the message a mutation acts on is not stored yet.
var errNoTarget = errors.New("target message not stored yet")

// noTarget is errNoTarget naming the missing message.
type noTarget struct{ id string }

func (e noTarget) Error() string   { return errNoTarget.Error() + ": " + e.id }
func (e noTarget) Is(t error) bool { return t == errNoTarget }

func missing(id string) error { return noTarget{id} }

// deferMutation keeps a mutation whose target is not stored yet, as the
// archived message it came in, until that message arrives (applyPending).
// Retried and duplicate copies are kept once.
func (in *Ingestor) deferMutation(ctx context.Context, tx *sql.Tx, info types.MessageInfo, msg *waE2E.Message, vote *waE2E.PollVoteMessage, src, target string) error {
	if target == "" || info.Timestamp.Before(in.now().Add(-PendingTTL)) {
		return nil
	}
	payload, key, err := encodeMessage(&events.Message{Info: info, Message: msg, RawMessage: msg}, vote)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pending_mutations(target_wa_id, mutation_key, source, ts, payload) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(mutation_key) DO NOTHING`, target, key, src, info.Timestamp.Unix(), payload)
	return err
}

// applyPending applies the mutations that waited for message waID, oldest
// first. Each leaves the table first; one whose target still cannot be
// found (another chat's message with the same id) goes back.
func (in *Ingestor) applyPending(ctx context.Context, tx *sql.Tx, waID string) error {
	type row struct {
		id      int64
		src     string
		payload []byte
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, source, payload FROM pending_mutations WHERE target_wa_id = ? ORDER BY ts, id`, waID)
	if err != nil {
		return err
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.src, &r.payload); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range rs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_mutations WHERE id = ?`, r.id); err != nil {
			return err
		}
		info, msg, vote, err := decodeMessage(r.payload)
		if err != nil {
			in.log().Warn("drop unreadable pending mutation", "target", waID, "err", err)
			continue
		}
		if _, err := in.ingest(ctx, tx, info, msg, r.src, vote); err != nil {
			return err
		}
	}
	return nil
}

// ExpirePending drops mutations whose target never arrived within PendingTTL.
func ExpirePending(ctx context.Context, db *store.DB, now time.Time) (int64, error) {
	var n int64
	err := store.Retry(ctx, func() error {
		res, err := db.ExecContext(ctx, `DELETE FROM pending_mutations WHERE ts < ?`, now.Add(-PendingTTL).Unix())
		if err == nil {
			n, _ = res.RowsAffected()
		}
		return err
	})
	return n, err
}
