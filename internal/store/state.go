package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Execer is satisfied by *sql.DB, *sql.Tx and *DB.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Sync state keys.
const (
	KeyLastEventAt      = "last_event_at"      // unix ts of the newest stored message or event
	KeyAliveAt          = "alive_at"           // heartbeat of the running daemon
	KeyOfflineSync      = "offline_sync"       // "pending:<n>" or "done:<n>"
	KeyHistorySyncAt    = "history_sync_at"    // unix ts of the last history sync blob
	KeyHistorySyncState = "history_sync_state" // type and progress of the last blob
	KeyImportLegacy     = "import_legacy"      // summary of the last legacy import
	// KeyAppStateRecovery prefixes the per-collection app state recovery state
	// ("app_state_recovery:regular_low"), a JSON object owned by package wa.
	KeyAppStateRecovery = "app_state_recovery:"
	// KeySendIdempotency prefixes the result of a send made with an
	// idempotency key ("send_idempotency:<key>"), a JSON object owned by package mcpserver.
	KeySendIdempotency = "send_idempotency:"
	// KeyHistoryRequest prefixes the time of the last history request per
	// chat ("history_request:42"); KeyHistoryRequestsDay counts today's
	// ("2026-10-13 3", Europe/Rome).
	KeyHistoryRequest     = "history_request:"
	KeyHistoryRequestsDay = "history_requests_day"
	// KeyTemporaryBan holds WhatsApp's last temporary ban, a JSON object
	// owned by package wa.
	KeyTemporaryBan = "temporary_ban"
	// KeyPhoneSeenAt is the unix time of the newest activity of the
	// account's primary phone seen by this device.
	KeyPhoneSeenAt = "phone_seen_at"
)

// SetState writes a sync_state key.
func SetState(ctx context.Context, e Execer, key, value string) error {
	_, err := e.ExecContext(ctx, `INSERT INTO sync_state(key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, Now().Unix())
	return err
}

// BumpStateTS stores ts under key unless the stored value is already newer.
func BumpStateTS(ctx context.Context, e Execer, key string, ts int64) error {
	_, err := e.ExecContext(ctx, `INSERT INTO sync_state(key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		WHERE CAST(sync_state.value AS INTEGER) < CAST(excluded.value AS INTEGER)`,
		key, strconv.FormatInt(ts, 10), Now().Unix())
	return err
}

// GetState reads a sync_state key; ok is false when missing.
func GetState(ctx context.Context, e Execer, key string) (value string, ok bool, err error) {
	err = e.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// GetStateAt reads a sync_state key and when it was written; ok is false when missing.
func GetStateAt(ctx context.Context, e Execer, key string) (value string, at time.Time, ok bool, err error) {
	var ts int64
	err = e.QueryRowContext(ctx, `SELECT value, updated_at FROM sync_state WHERE key = ?`, key).Scan(&value, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	return value, time.Unix(ts, 0), err == nil, err
}

// ReplaceStateValue rewrites a key's value keeping its updated_at.
func ReplaceStateValue(ctx context.Context, e Execer, key, value string) error {
	_, err := e.ExecContext(ctx, `UPDATE sync_state SET value = ? WHERE key = ?`, value, key)
	return err
}

// StatePrefix returns every key starting with prefix and its value.
func StatePrefix(ctx context.Context, e Execer, prefix string) (map[string]string, error) {
	rows, err := e.QueryContext(ctx, `SELECT key, value FROM sync_state WHERE substr(key, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// DeleteState removes a sync_state key.
func DeleteState(ctx context.Context, e Execer, key string) error {
	_, err := e.ExecContext(ctx, `DELETE FROM sync_state WHERE key = ?`, key)
	return err
}

// DeleteStateBefore removes the keys starting with prefix written before t.
func DeleteStateBefore(ctx context.Context, e Execer, prefix string, t time.Time) error {
	_, err := e.ExecContext(ctx, `DELETE FROM sync_state WHERE substr(key, 1, ?) = ? AND updated_at < ?`, len(prefix), prefix, t.Unix())
	return err
}

// FreshState is what the freshness line of every read result needs, read
// in one query.
type FreshState struct {
	LastEventAt, PhoneSeenAt, HistorySyncAt time.Time // zero when missing
	// Log identifies the content of connection_log for caching Gaps. Rows
	// are only appended while the daemon runs (migrations rewrite them
	// before it serves), so Gaps changes only when Log does.
	Log LogVersion
}

// LogVersion is the newest id and the row count of connection_log.
type LogVersion struct{ MaxID, Rows int64 }

// GetFreshState reads the freshness keys and the connection_log version.
func GetFreshState(ctx context.Context, e Execer) (FreshState, error) {
	var last, phone, hist sql.NullString
	var f FreshState
	err := e.QueryRowContext(ctx, `SELECT
		(SELECT value FROM sync_state WHERE key = ?), (SELECT value FROM sync_state WHERE key = ?),
		(SELECT value FROM sync_state WHERE key = ?),
		(SELECT COALESCE(MAX(id), 0) FROM connection_log), (SELECT COUNT(*) FROM connection_log)`,
		KeyLastEventAt, KeyPhoneSeenAt, KeyHistorySyncAt).Scan(&last, &phone, &hist, &f.Log.MaxID, &f.Log.Rows)
	if err != nil {
		return FreshState{}, err
	}
	f.LastEventAt, f.PhoneSeenAt, f.HistorySyncAt = parseStateTS(last), parseStateTS(phone), parseStateTS(hist)
	return f, nil
}

// GetStateTS reads a timestamp key; zero time when missing.
func GetStateTS(ctx context.Context, e Execer, key string) (time.Time, error) {
	v, ok, err := GetState(ctx, e, key)
	if err != nil || !ok {
		return time.Time{}, err
	}
	return parseStateTS(sql.NullString{String: v, Valid: true}), nil
}

// parseStateTS reads a unix timestamp value; zero time when missing, zero
// or not a number.
func parseStateTS(v sql.NullString) time.Time {
	n, err := strconv.ParseInt(v.String, 10, 64)
	if !v.Valid || err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// Connection log events.
const (
	EvDaemonStart    = "daemon_start"
	EvDaemonStop     = "daemon_stop"
	EvConnected      = "connected"
	EvDisconnected   = "disconnected"
	EvLoggedOut      = "logged_out"
	EvStreamReplaced = "stream_replaced"
	EvTemporaryBan   = "temporary_ban"
	EvClientOutdated = "client_outdated"
	EvConnectFailure = "connect_failure"
	EvKeepAliveLost  = "keepalive_timeout"
	EvKeepAliveBack  = "keepalive_restored"
	EvOfflinePreview = "offline_sync_preview"
	EvOfflineDone    = "offline_sync_completed"
	EvAppStateSynced = "app_state_sync_complete"
	EvAppStateError  = "app_state_sync_error"
	EvAppStateAsk    = "app_state_recovery_requested"
	EvAppStateRecov  = "app_state_recovered"
	EvPaired         = "paired"
	EvNotPaired      = "not_paired"
	EvSuspended      = "suspended" // system sleep or clock jump; logged at the last tick before it
)

// downEvents end a connected period; EvConnected starts one.
var downEvents = map[string]bool{
	EvDaemonStop: true, EvDisconnected: true, EvLoggedOut: true, EvStreamReplaced: true,
	EvTemporaryBan: true, EvClientOutdated: true, EvConnectFailure: true, EvKeepAliveLost: true, EvSuspended: true,
}

// LogConnection appends a connection_log row.
func LogConnection(ctx context.Context, e Execer, ts time.Time, event, detail string) error {
	_, err := e.ExecContext(ctx, `INSERT INTO connection_log(ts, event, detail) VALUES (?, ?, ?)`, ts.Unix(), event, detail)
	return err
}

// ConnEvent is one connection_log row.
type ConnEvent struct {
	TS     time.Time
	Event  string
	Detail string
}

// RecentConnections returns the newest n connection_log rows, newest first.
func RecentConnections(ctx context.Context, e Execer, n int) ([]ConnEvent, error) {
	rows, err := e.QueryContext(ctx, `SELECT ts, event, detail FROM connection_log ORDER BY ts DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnEvent
	for rows.Next() {
		var ts int64
		var ev ConnEvent
		if err := rows.Scan(&ts, &ev.Event, &ev.Detail); err != nil {
			return nil, err
		}
		ev.TS = time.Unix(ts, 0)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Gap is a period in which the daemon was not connected to WhatsApp after
// having been connected at least once.
type Gap struct {
	From, To time.Time // To is zero while the gap is still open
	Reason   string
}

// Gaps derives offline periods from connection_log, oldest first. A gap opens
// on any down event after a connected period and closes on the next connect
// (a keepalive timeout also closes when the keepalive is restored).
func Gaps(ctx context.Context, e Execer) ([]Gap, error) {
	rows, err := e.QueryContext(ctx, `SELECT ts, event FROM connection_log ORDER BY ts, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Gap
	var open *Gap
	seenConnect := false
	for rows.Next() {
		var ts int64
		var ev string
		if err := rows.Scan(&ts, &ev); err != nil {
			return nil, err
		}
		switch {
		case ev == EvKeepAliveBack && open != nil && open.Reason == EvKeepAliveLost:
			open.To = time.Unix(ts, 0)
			out = append(out, *open)
			open = nil
		case ev == EvConnected:
			if open != nil {
				open.To = time.Unix(ts, 0)
				out = append(out, *open)
				open = nil
			}
			seenConnect = true
		case downEvents[ev] && seenConnect && open == nil:
			open = &Gap{From: time.Unix(ts, 0), Reason: ev}
		}
	}
	if open != nil {
		out = append(out, *open)
	}
	return out, rows.Err()
}

// Details of the daemon_stop rows RecordDaemonStart writes for a previous
// session that ended without one.
const (
	StopUnclean     = "unclean exit: last heartbeat"
	StopNoHeartbeat = "previous session had no heartbeat: exit time unknown, no gap claimed"
)

// RecordDaemonStart logs daemon_start. When the previous session (run or
// pair) ended without daemon_stop (crash, kill, power loss) it first logs
// that stop, so the offline gap starts where the session really died:
//
//   - The previous session starts at the newest daemon_start or paired row.
//     When the heartbeat (alive_at) is at or after that start, the session
//     beat: the stop goes at the later of the last heartbeat and the last event.
//   - Otherwise the session never beat (a pair session before pairing shared
//     the lifecycle, or a crash before the first beat). Its exit time is
//     unknown, and closing it at its last event would show the rest of the
//     session as offline. The stop goes at now: no gap is claimed.
func RecordDaemonStart(ctx context.Context, e Execer, now time.Time, detail string) error {
	var lastEvent string
	var lastTS int64
	err := e.QueryRowContext(ctx, `SELECT event, ts FROM connection_log ORDER BY ts DESC, id DESC LIMIT 1`).Scan(&lastEvent, &lastTS)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && lastEvent != EvDaemonStop {
		var sessionStart int64
		if err := e.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts), 0) FROM connection_log WHERE event IN (?, ?)`,
			EvDaemonStart, EvPaired).Scan(&sessionStart); err != nil {
			return err
		}
		alive, err := GetStateTS(ctx, e, KeyAliveAt)
		if err != nil {
			return err
		}
		at, why := now, StopNoHeartbeat
		if !alive.IsZero() && alive.Unix() >= sessionStart {
			at, why = time.Unix(lastTS, 0), StopUnclean
			if alive.After(at) {
				at = alive
			}
		}
		if err := LogConnection(ctx, e, at, EvDaemonStop, why); err != nil {
			return err
		}
	}
	return LogConnection(ctx, e, now, EvDaemonStart, detail)
}

// Heartbeat records that the daemon is alive.
func Heartbeat(ctx context.Context, e Execer, now time.Time) error {
	return SetState(ctx, e, KeyAliveAt, strconv.FormatInt(now.Unix(), 10))
}
