package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Media download states.
const (
	MediaPending     = "pending"     // not downloaded yet
	MediaDownloaded  = "downloaded"  // verified file under the media root
	MediaFailed      = "failed"      // last attempt failed; an on-demand request tries again
	MediaExpired     = "expired"     // gone from WhatsApp's servers and the phone did not re-upload it (yet)
	MediaUnavailable = "unavailable" // final: the phone no longer has it, or it can never be decrypted
)

// MaxEagerAttempts bounds automatic retries of a failed live download.
const MaxEagerAttempts = 3

// MediaStates lists every state in display order.
var MediaStates = []string{MediaDownloaded, MediaPending, MediaFailed, MediaExpired, MediaUnavailable}

// MediaJob is one media file with the message and chat it belongs to:
// everything needed to download, store and transcribe it.
type MediaJob struct {
	MessageID int64
	ChatID    int64
	ChatJID   string
	ChatKind  string
	WAID      string
	SenderJID string
	FromMe    bool
	Kind      string
	Source    string
	TS        int64
	Deleted   bool

	MediaID int64
	MediaFields
	LocalPath string // relative to the media root
	State     string
	Error     string
	Attempts  int

	HasTranscript    bool
	Transcript       string
	TranscriptLang   string
	TranscriptEngine string
	TranscriptError  string
}

const mediaJobQuery = `SELECT m.id, m.chat_id, c.jid, c.kind, m.wa_id, m.sender_jid, m.from_me, m.kind, m.source, m.ts, m.deleted_at IS NOT NULL,
	md.id, md.mimetype, md.filename, COALESCE(md.size, 0), COALESCE(md.duration, 0), md.direct_path, md.url,
	md.media_key, md.file_sha256, md.file_enc_sha256, md.local_path, md.state, md.error, md.attempts,
	md.transcript IS NOT NULL, COALESCE(md.transcript, ''), COALESCE(md.transcript_lang, ''), COALESCE(md.transcript_engine, ''), md.transcript_error
	FROM messages m JOIN chats c ON c.id = m.chat_id JOIN media md ON md.id = m.media_id`

func scanMediaJob(row *sql.Row) (MediaJob, bool, error) {
	var j MediaJob
	err := row.Scan(&j.MessageID, &j.ChatID, &j.ChatJID, &j.ChatKind, &j.WAID, &j.SenderJID, &j.FromMe, &j.Kind, &j.Source, &j.TS, &j.Deleted,
		&j.MediaID, &j.Mimetype, &j.Filename, &j.Size, &j.Duration, &j.DirectPath, &j.URL,
		&j.MediaKey, &j.SHA256, &j.EncSHA256, &j.LocalPath, &j.State, &j.Error, &j.Attempts,
		&j.HasTranscript, &j.Transcript, &j.TranscriptLang, &j.TranscriptEngine, &j.TranscriptError)
	if errors.Is(err, sql.ErrNoRows) {
		return j, false, nil
	}
	return j, err == nil, err
}

// LoadMediaJob reads the media of message id; ok is false when the message
// does not exist or has no media.
func LoadMediaJob(ctx context.Context, e Execer, messageID int64) (MediaJob, bool, error) {
	return scanMediaJob(e.QueryRowContext(ctx, mediaJobQuery+` WHERE m.id = ?`, messageID))
}

// LoadMediaJobByMedia reads a media row with the (oldest) message that uses it.
func LoadMediaJobByMedia(ctx context.Context, e Execer, mediaID int64) (MediaJob, bool, error) {
	return scanMediaJob(e.QueryRowContext(ctx, mediaJobQuery+` WHERE m.media_id = ? ORDER BY m.id LIMIT 1`, mediaID))
}

// SetMediaState records the outcome of a download attempt. localPath is kept
// when empty.
func SetMediaState(ctx context.Context, e Execer, mediaID int64, state, localPath, errText string) error {
	_, err := e.ExecContext(ctx, `UPDATE media SET state = ?, error = ?, attempts = attempts + 1, state_at = ?,
		local_path = CASE WHEN ? != '' THEN ? ELSE local_path END WHERE id = ?`,
		state, errText, Now().Unix(), localPath, localPath, mediaID)
	return err
}

// ResetMediaFile marks a downloaded media whose file disappeared as pending again.
func ResetMediaFile(ctx context.Context, e Execer, mediaID int64) error {
	_, err := e.ExecContext(ctx, `UPDATE media SET state = 'pending', local_path = '', state_at = ? WHERE id = ?`, Now().Unix(), mediaID)
	return err
}

// TakeReuploadRequest records one media retry receipt for a media row when
// fewer than max were sent in the current window (which starts with the
// first request and lasts window). ok is false when the cap is reached;
// next is when the window ends.
func TakeReuploadRequest(ctx context.Context, db *DB, mediaID int64, now time.Time, max int, window time.Duration) (ok bool, next time.Time, err error) {
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		var n int
		var start sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT reupload_requests, reupload_window_at FROM media WHERE id = ?`, mediaID).Scan(&n, &start); err != nil {
			return err
		}
		if !start.Valid || now.Sub(time.Unix(start.Int64, 0)) >= window {
			n, start = 0, sql.NullInt64{Int64: now.Unix(), Valid: true}
		}
		next = time.Unix(start.Int64, 0).Add(window)
		if n >= max {
			return nil
		}
		ok = true
		_, err := tx.ExecContext(ctx, `UPDATE media SET reupload_requests = ?, reupload_window_at = ? WHERE id = ?`, n+1, start.Int64, mediaID)
		return err
	})
	return ok, next, err
}

// SetMediaDirectPath stores the direct path the phone re-uploaded the file to.
func SetMediaDirectPath(ctx context.Context, e Execer, mediaID int64, directPath string) error {
	_, err := e.ExecContext(ctx, `UPDATE media SET direct_path = ? WHERE id = ?`, directPath, mediaID)
	return err
}

// SetTranscript stores a transcript; the FTS trigger indexes it.
func SetTranscript(ctx context.Context, e Execer, mediaID int64, text, lang, engine string) error {
	_, err := e.ExecContext(ctx, `UPDATE media SET transcript = ?, transcript_lang = ?, transcript_engine = ?, transcript_error = '' WHERE id = ?`,
		text, lang, engine, mediaID)
	return err
}

// SetTranscriptError records why transcription failed, so the background
// queue does not retry it; an on-demand request does.
func SetTranscriptError(ctx context.Context, e Execer, mediaID int64, errText string) error {
	_, err := e.ExecContext(ctx, `UPDATE media SET transcript_error = ? WHERE id = ?`, errText, mediaID)
	return err
}

// MediaStateCounts counts media rows per state.
func MediaStateCounts(ctx context.Context, e Execer) (map[string]int, error) {
	rows, err := e.QueryContext(ctx, `SELECT state, COUNT(*) FROM media GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// MediaRef names a media row and the message that uses it.
type MediaRef struct{ MessageID, MediaID int64 }

func queryRefs(ctx context.Context, e Execer, q string, args ...any) ([]MediaRef, error) {
	rows, err := e.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaRef
	for rows.Next() {
		var r MediaRef
		if err := rows.Scan(&r.MessageID, &r.MediaID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingLiveMedia lists live media not downloaded yet, newest first: sent at
// or after since, smaller than maxSize (unknown sizes included), not deleted,
// pending or failed fewer than MaxEagerAttempts times.
func PendingLiveMedia(ctx context.Context, e Execer, since, maxSize int64, limit int) ([]MediaRef, error) {
	return queryRefs(ctx, e, `SELECT m.id, md.id FROM messages m JOIN media md ON md.id = m.media_id
		WHERE (md.state = 'pending' OR (md.state = 'failed' AND md.attempts < ?)) AND m.source = 'live' AND m.ts >= ? AND COALESCE(md.size, 0) < ? AND m.deleted_at IS NULL
		ORDER BY m.ts DESC LIMIT ?`, MaxEagerAttempts, since, maxSize, limit)
}

// UntranscribedAudio lists downloaded voice notes and audio without a
// transcript or a recorded transcription error, newest first.
func UntranscribedAudio(ctx context.Context, e Execer, limit int) ([]MediaRef, error) {
	return queryRefs(ctx, e, `SELECT m.id, md.id FROM messages m JOIN media md ON md.id = m.media_id
		WHERE md.state = 'downloaded' AND m.kind IN ('voice', 'audio') AND md.transcript IS NULL AND md.transcript_error = ''
		ORDER BY m.ts DESC LIMIT ?`, limit)
}
