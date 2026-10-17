package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

// Raw event kinds.
const (
	RawMessage       = "message"              // live message: info + raw proto
	RawHistoryNotify = "history_notification" // history sync notification, blob not yet downloaded
	RawHistory       = "history"              // decoded history sync blob
	RawGroupEvent    = "group_event"          // live group change (name, description, members)
	RawChatState     = "chat_state"           // archive, pin, mute, mark read, own read receipt
	RawLegacySession = "legacy_session"       // identities of a legacy session DB
	RawLegacyChat    = "legacy_chat"          // one row of a legacy messages.db chats table
	RawLegacyMessage = "legacy_message"       // one row of a legacy messages.db messages table
)

// RawEvent is one archived event with its payload decompressed.
type RawEvent struct {
	ID         int64
	Kind       string
	Key        string
	ReceivedAt time.Time
	Payload    []byte
	Ingested   bool
	Attempts   int
	Error      string
	// AttemptedAt is when ingestion (or a history blob download) was last
	// tried; zero when never.
	AttemptedAt time.Time
}

func compress(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompress(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// IsBusy reports SQLite lock contention, worth retrying.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

// Retry runs fn up to 5 times while it fails with SQLITE_BUSY.
func Retry(ctx context.Context, fn func() error) error {
	delay := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := fn()
		if !IsBusy(err) || attempt == 5 {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// ArchiveRaw stores an event in its own committed transaction. key makes it
// idempotent: archiving the same key again returns the existing row.
func (db *DB) ArchiveRaw(ctx context.Context, kind, key string, payload []byte, at time.Time) (RawEvent, error) {
	z, err := compress(payload)
	if err != nil {
		return RawEvent{}, err
	}
	ev := RawEvent{Kind: kind, Key: key, ReceivedAt: at, Payload: payload}
	err = Retry(ctx, func() error {
		return db.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO raw_events(kind, dedupe_key, received_at, payload) VALUES (?, ?, ?, ?)
				ON CONFLICT(dedupe_key) DO NOTHING`, kind, key, at.Unix(), z); err != nil {
				return err
			}
			var ingested sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT id, ingested_at, attempts, error FROM raw_events WHERE dedupe_key = ?`, key).
				Scan(&ev.ID, &ingested, &ev.Attempts, &ev.Error); err != nil {
				return err
			}
			ev.Ingested = ingested.Valid
			return nil
		})
	})
	return ev, err
}

// UpdateRawPayload replaces an archived event's payload, for data that
// arrives after the event was archived (a poll vote decrypted afterwards).
func (db *DB) UpdateRawPayload(ctx context.Context, id int64, payload []byte) error {
	z, err := compress(payload)
	if err != nil {
		return err
	}
	return Retry(ctx, func() error {
		_, err := db.ExecContext(ctx, `UPDATE raw_events SET payload = ? WHERE id = ?`, z, id)
		return err
	})
}

// MarkRaw records the outcome of ingesting a raw event. A nil error (or
// final=true) marks it ingested; otherwise it stays pending for a retry.
func MarkRaw(ctx context.Context, e Execer, id int64, ingestErr error, final bool) error {
	msg := ""
	if ingestErr != nil {
		msg = ingestErr.Error()
	}
	var ingested any
	if ingestErr == nil || final {
		ingested = Now().Unix()
	}
	_, err := e.ExecContext(ctx, `UPDATE raw_events SET ingested_at = ?, error = ?, attempts = attempts + 1, attempted_at = ? WHERE id = ?`,
		ingested, msg, Now().Unix(), id)
	return err
}

// RawFilter selects raw events in id order.
type RawFilter struct {
	Kinds       []string
	PendingOnly bool
	AfterID     int64
	Limit       int
	MaxAttempts int // skip pending rows that failed this many times (0 = no limit)
}

// ListRaw returns raw events in id order with payloads decompressed.
func ListRaw(ctx context.Context, e Execer, f RawFilter) ([]RawEvent, error) {
	q := `SELECT id, kind, dedupe_key, received_at, payload, ingested_at IS NOT NULL, attempts, error, COALESCE(attempted_at, 0) FROM raw_events WHERE id > ?`
	args := []any{f.AfterID}
	if len(f.Kinds) > 0 {
		q += ` AND kind IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",") + `)`
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.PendingOnly {
		q += ` AND ingested_at IS NULL`
	}
	if f.MaxAttempts > 0 {
		q += ` AND attempts < ?`
		args = append(args, f.MaxAttempts)
	}
	q += ` ORDER BY id LIMIT ?`
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	args = append(args, limit)
	rows, err := e.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawEvent
	for rows.Next() {
		var ev RawEvent
		var at, tried int64
		var z []byte
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.Key, &at, &z, &ev.Ingested, &ev.Attempts, &ev.Error, &tried); err != nil {
			return nil, err
		}
		ev.ReceivedAt = time.Unix(at, 0)
		if tried != 0 {
			ev.AttemptedAt = time.Unix(tried, 0)
		}
		if ev.Payload, err = decompress(z); err != nil {
			return nil, errors.Join(errors.New("corrupt raw event payload"), err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// CountPendingRaw counts events of the given kinds not yet ingested.
func CountPendingRaw(ctx context.Context, e Execer, kinds ...string) (int, error) {
	q := `SELECT COUNT(*) FROM raw_events WHERE ingested_at IS NULL`
	var args []any
	if len(kinds) > 0 {
		q += ` AND kind IN (` + strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",") + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	var n int
	err := e.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// KeyReingest marks a reingest in progress: set in the transaction that
// empties the derived tables, cleared when the rebuild is complete. A run
// that finds it resumes the reingest before serving anything.
const KeyReingest = "reingest_running"

// ResetDerived deletes every table rebuilt from raw_events, keeping the
// archive, connection_log, sync_state and the media rows: messages relink
// to them (AttachMedia), so downloads and transcripts survive. It sets
// KeyReingest; FinishReingest clears it.
func ResetDerived(ctx context.Context, e Execer) error {
	if err := SetState(ctx, e, KeyReingest, strconv.FormatInt(Now().Unix(), 10)); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM reactions`, `DELETE FROM messages`, `DELETE FROM chats`,
		`DELETE FROM jids`, `DELETE FROM number_links`, `DELETE FROM persons`, `DELETE FROM identity_notes`,
		`DELETE FROM messages_fts`, `DELETE FROM pending_mutations`,
		`DELETE FROM sync_state WHERE key IN ('last_event_at', 'history_sync_at', 'history_sync_state')`,
		`UPDATE raw_events SET ingested_at = NULL, attempts = 0, error = '', attempted_at = NULL WHERE kind != 'history_notification'`,
		// A notification whose blob never downloaded is tried again by the
		// daemon (WhatsApp keeps blobs about two weeks); done ones stay done.
		`UPDATE raw_events SET ingested_at = NULL, attempts = 0, error = '', attempted_at = NULL
			WHERE kind = 'history_notification' AND ingested_at IS NOT NULL AND error != ''`,
	} {
		if _, err := e.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// FinishReingest completes a rebuild: media rows no message uses any more
// are deleted (their files stay on disk, adopted if the message returns)
// and KeyReingest is cleared, in one transaction.
func FinishReingest(ctx context.Context, db *DB) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM media WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.media_id = media.id)`); err != nil {
			return err
		}
		return DeleteState(ctx, tx, KeyReingest)
	})
}

// ReingestInterrupted reports whether a reingest started and never finished:
// the derived tables are incomplete.
func ReingestInterrupted(ctx context.Context, e Execer) (bool, error) {
	_, ok, err := GetState(ctx, e, KeyReingest)
	return ok, err
}

// RawItem is one event for ArchiveRawBatch.
type RawItem struct {
	Kind, Key string
	Payload   []byte
}

// ArchiveRawBatch archives many events in one committed transaction.
// Existing keys are left alone. It returns how many rows were new.
func (db *DB) ArchiveRawBatch(ctx context.Context, items []RawItem, at time.Time) (int, error) {
	zs := make([][]byte, len(items))
	for i, it := range items {
		z, err := compress(it.Payload)
		if err != nil {
			return 0, err
		}
		zs[i] = z
	}
	added := 0
	err := Retry(ctx, func() error {
		added = 0
		return db.Tx(ctx, func(tx *sql.Tx) error {
			for i, it := range items {
				res, err := tx.ExecContext(ctx, `INSERT INTO raw_events(kind, dedupe_key, received_at, payload) VALUES (?, ?, ?, ?)
					ON CONFLICT(dedupe_key) DO NOTHING`, it.Kind, it.Key, at.Unix(), zs[i])
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					added++
				}
			}
			return nil
		})
	})
	return added, err
}
