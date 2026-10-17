package store

import (
	"context"
	"database/sql"
	"errors"
)

// Message sources, in rank order: a higher rank is a richer, more direct copy.
const (
	SourceLegacy  = "legacy"
	SourceHistory = "history"
	SourceLive    = "live"
)

// SourceRank orders sources: legacy < history < live.
func SourceRank(s string) int {
	switch s {
	case SourceHistory:
		return 1
	case SourceLive:
		return 2
	}
	return 0
}

// MessageFields is the mergeable content of one messages row.
type MessageFields struct {
	ID           int64
	Source       string
	Kind         string
	Text         string
	SenderPerson int64
	SenderJID    string
	FromMe       bool
	TS           int64
	ReplyTo      string
	ReplyPreview string
	EditedAt     int64
	DeletedAt    int64
	Forwarded    bool
	MediaID      int64
}

func fill(dst *string, src string) {
	if *dst == "" {
		*dst = src
	}
}

func prefer(dst *string, src string) {
	if src != "" {
		*dst = src
	}
}

// MergeMessage combines two copies of the same message without losing data.
// The higher-ranked source wins content fields it has; the other copy fills
// what is still empty. Text follows the latest edit: an edited text is never
// replaced by an unedited copy. Edit and delete times keep the strongest fact.
func MergeMessage(old, nw MessageFields) MessageFields {
	out := old
	newer := SourceRank(nw.Source) > SourceRank(old.Source)
	if newer {
		out.Source = nw.Source
		prefer(&out.Kind, nw.Kind)
		prefer(&out.SenderJID, nw.SenderJID)
		prefer(&out.ReplyTo, nw.ReplyTo)
		prefer(&out.ReplyPreview, nw.ReplyPreview)
		if nw.SenderPerson != 0 {
			out.SenderPerson = nw.SenderPerson
		}
		out.FromMe = nw.FromMe
		out.Forwarded = out.Forwarded || nw.Forwarded
	} else {
		fill(&out.Kind, nw.Kind)
		fill(&out.SenderJID, nw.SenderJID)
		fill(&out.ReplyTo, nw.ReplyTo)
		fill(&out.ReplyPreview, nw.ReplyPreview)
		if out.SenderPerson == 0 {
			out.SenderPerson = nw.SenderPerson
		}
		out.Forwarded = out.Forwarded || nw.Forwarded
	}
	switch {
	case nw.EditedAt > old.EditedAt && nw.Text != "":
		out.Text = nw.Text
	case nw.EditedAt == old.EditedAt && newer && nw.Text != "":
		out.Text = nw.Text
	default:
		fill(&out.Text, nw.Text)
	}
	out.EditedAt = max(old.EditedAt, nw.EditedAt)
	switch {
	case old.DeletedAt == 0:
		out.DeletedAt = nw.DeletedAt
	case nw.DeletedAt != 0:
		out.DeletedAt = min(old.DeletedAt, nw.DeletedAt)
	}
	if out.TS == 0 {
		out.TS = nw.TS
	}
	if out.MediaID == 0 {
		out.MediaID = nw.MediaID
	}
	return out
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LoadMessage reads one message's fields; ok is false when it does not exist.
func LoadMessage(ctx context.Context, e Execer, chatID int64, waID string) (MessageFields, bool, error) {
	return scanFields(e.QueryRowContext(ctx, `SELECT `+fieldCols+` FROM messages WHERE chat_id = ? AND wa_id = ?`, chatID, waID))
}

// LoadMessageByID reads one message's fields by row id.
func LoadMessageByID(ctx context.Context, e Execer, id int64) (MessageFields, bool, error) {
	return scanFields(e.QueryRowContext(ctx, `SELECT `+fieldCols+` FROM messages WHERE id = ?`, id))
}

const fieldCols = `id, source, kind, text, COALESCE(sender_person_id, 0), sender_jid, from_me, ts, COALESCE(reply_to_wa_id, ''),
	COALESCE(reply_preview, ''), COALESCE(edited_at, 0), COALESCE(deleted_at, 0), forwarded, COALESCE(media_id, 0)`

func scanFields(row *sql.Row) (MessageFields, bool, error) {
	var f MessageFields
	err := row.Scan(&f.ID, &f.Source, &f.Kind, &f.Text, &f.SenderPerson, &f.SenderJID, &f.FromMe, &f.TS, &f.ReplyTo,
		&f.ReplyPreview, &f.EditedAt, &f.DeletedAt, &f.Forwarded, &f.MediaID)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	return f, err == nil, err
}

// SaveMessage writes merged fields back to row f.ID.
func SaveMessage(ctx context.Context, e Execer, f MessageFields) error {
	_, err := e.ExecContext(ctx, `UPDATE messages SET source = ?, kind = ?, text = ?, sender_person_id = ?, sender_jid = ?, from_me = ?,
		ts = ?, reply_to_wa_id = ?, reply_preview = ?, edited_at = ?, deleted_at = ?, forwarded = ?, media_id = ? WHERE id = ?`,
		f.Source, f.Kind, f.Text, nullInt(f.SenderPerson), f.SenderJID, f.FromMe, f.TS, nullStr(f.ReplyTo), nullStr(f.ReplyPreview),
		nullInt(f.EditedAt), nullInt(f.DeletedAt), f.Forwarded, nullInt(f.MediaID), f.ID)
	return err
}

// MediaFields is the downloadable description of a media file.
type MediaFields struct {
	Mimetype, Filename          string
	Size, Duration              int64
	DirectPath, URL             string
	MediaKey, SHA256, EncSHA256 []byte
}

// SentRef is a stored message sent by this account.
type SentRef struct {
	ID, ChatID, TS int64
}

// SentMessageByWAID finds a message this account sent with WhatsApp id waID.
func SentMessageByWAID(ctx context.Context, e Execer, waID string) (SentRef, bool, error) {
	var r SentRef
	err := e.QueryRowContext(ctx, `SELECT id, chat_id, ts FROM messages WHERE wa_id = ? AND from_me = 1 ORDER BY id LIMIT 1`, waID).
		Scan(&r.ID, &r.ChatID, &r.TS)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// insertMedia stores a new media row for message waID of chat waChat.
func insertMedia(ctx context.Context, e Execer, m MediaFields, waChat, waID string) (int64, error) {
	res, err := e.ExecContext(ctx, `INSERT INTO media(mimetype, filename, size, duration, direct_path, url, media_key, file_sha256, file_enc_sha256, msg_chat_jid, msg_wa_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, m.Mimetype, m.Filename, nullInt(m.Size), nullInt(m.Duration), m.DirectPath, m.URL,
		m.MediaKey, m.SHA256, m.EncSHA256, nullText(waChat), nullText(waID))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AttachMedia returns the media row for the message waID of chat chatID.
// A row no message uses any more with the same WhatsApp id (kept by a
// reingest) is reused, the same chat's, else another chat's holding the
// same file (SHA-256), so its download state, local
// file, transcript and re-uploaded path survive; m only fills its gaps.
// Otherwise a new row is inserted, keyed by the chat's JID and waID.
func AttachMedia(ctx context.Context, e Execer, m MediaFields, chatID int64, waID string) (int64, error) {
	var chatJID string
	if err := e.QueryRowContext(ctx, `SELECT jid FROM chats WHERE id = ?`, chatID).Scan(&chatJID); err != nil {
		return 0, err
	}
	// Same chat first. Another chat's row (the chat's JID changed between
	// phone number and LID) only when it holds the same file: WhatsApp ids
	// are unique per sender, not across chats.
	var id int64
	err := e.QueryRowContext(ctx, `SELECT md.id FROM media md WHERE md.msg_wa_id = ?1
		AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.media_id = md.id)
		AND (md.msg_chat_jid = ?2 OR (length(?3) > 0 AND md.file_sha256 = ?3))
		ORDER BY md.msg_chat_jid = ?2 DESC, md.id LIMIT 1`, waID, chatJID, m.SHA256).Scan(&id)
	switch {
	case err == nil:
		if err := UpdateMedia(ctx, e, id, m, false); err != nil {
			return 0, err
		}
		_, err = e.ExecContext(ctx, `UPDATE media SET msg_chat_jid = ? WHERE id = ?`, chatJID, id)
		return id, err
	case !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}
	return insertMedia(ctx, e, m, chatJID, waID)
}

// UpdateMedia merges m into media row id: when wins, every non-empty field of
// m replaces the stored one; otherwise m only fills empty fields. Download
// state, local path and transcript are never touched.
func UpdateMedia(ctx context.Context, e Execer, id int64, m MediaFields, wins bool) error {
	q := `UPDATE media SET
		mimetype        = CASE WHEN ?1 AND ?2 != '' THEN ?2 WHEN mimetype = '' THEN ?2 ELSE mimetype END,
		filename        = CASE WHEN ?1 AND ?3 != '' THEN ?3 WHEN filename = '' THEN ?3 ELSE filename END,
		size            = CASE WHEN ?1 AND ?4 IS NOT NULL THEN ?4 ELSE COALESCE(size, ?4) END,
		duration        = CASE WHEN ?1 AND ?5 IS NOT NULL THEN ?5 ELSE COALESCE(duration, ?5) END,
		direct_path     = CASE WHEN ?1 AND ?6 != '' THEN ?6 WHEN direct_path = '' THEN ?6 ELSE direct_path END,
		url             = CASE WHEN ?1 AND ?7 != '' THEN ?7 WHEN url = '' THEN ?7 ELSE url END,
		media_key       = CASE WHEN ?1 AND length(?8) > 0 THEN ?8 WHEN media_key IS NULL OR length(media_key) = 0 THEN ?8 ELSE media_key END,
		file_sha256     = CASE WHEN ?1 AND length(?9) > 0 THEN ?9 WHEN file_sha256 IS NULL OR length(file_sha256) = 0 THEN ?9 ELSE file_sha256 END,
		file_enc_sha256 = CASE WHEN ?1 AND length(?10) > 0 THEN ?10 WHEN file_enc_sha256 IS NULL OR length(file_enc_sha256) = 0 THEN ?10 ELSE file_enc_sha256 END
		WHERE id = ?11`
	_, err := e.ExecContext(ctx, q, wins, m.Mimetype, m.Filename, nullInt(m.Size), nullInt(m.Duration), m.DirectPath, m.URL,
		m.MediaKey, m.SHA256, m.EncSHA256, id)
	return err
}

// LoadMedia reads the downloadable fields of a media row.
func LoadMedia(ctx context.Context, e Execer, id int64) (MediaFields, error) {
	var m MediaFields
	err := e.QueryRowContext(ctx, `SELECT mimetype, filename, COALESCE(size, 0), COALESCE(duration, 0), direct_path, url,
		media_key, file_sha256, file_enc_sha256 FROM media WHERE id = ?`, id).
		Scan(&m.Mimetype, &m.Filename, &m.Size, &m.Duration, &m.DirectPath, &m.URL, &m.MediaKey, &m.SHA256, &m.EncSHA256)
	return m, err
}

// MergeMessageRows folds message row drop into row keep (same WhatsApp
// message stored twice): fields merge with MergeMessage, media rows merge
// field by field, reactions move (newest per person wins), then drop is deleted.
func MergeMessageRows(ctx context.Context, e Execer, keepID, dropID int64) error {
	k, ok1, err := LoadMessageByID(ctx, e, keepID)
	if err != nil {
		return err
	}
	d, ok2, err := LoadMessageByID(ctx, e, dropID)
	if err != nil || !ok1 || !ok2 {
		return err
	}
	merged := MergeMessage(k, d)
	merged.ID = keepID
	orphan := int64(0)
	if k.MediaID != 0 && d.MediaID != 0 && k.MediaID != d.MediaID {
		// The row holding the downloaded file and transcript survives;
		// UpdateMedia never moves them. Ties keep keep's row.
		into, from, wins := k.MediaID, d.MediaID, SourceRank(d.Source) > SourceRank(k.Source)
		const progress = `(SELECT (state = 'downloaded') * 2 + (transcript IS NOT NULL) FROM media WHERE id = ?)`
		var kp, dp int
		if err := e.QueryRowContext(ctx, `SELECT `+progress+`, `+progress, k.MediaID, d.MediaID).Scan(&kp, &dp); err != nil {
			return err
		}
		if dp > kp {
			into, from, wins = d.MediaID, k.MediaID, SourceRank(k.Source) >= SourceRank(d.Source)
		}
		fm, err := LoadMedia(ctx, e, from)
		if err != nil {
			return err
		}
		if err := UpdateMedia(ctx, e, into, fm, wins); err != nil {
			return err
		}
		// ResetMediaFile keeps the transcript, so the losing row may hold
		// one the winner lacks: move it over (the FTS trigger reindexes).
		if _, err := e.ExecContext(ctx, `UPDATE media SET transcript = f.transcript, transcript_lang = f.transcript_lang,
			transcript_engine = f.transcript_engine, transcript_error = ''
			FROM media AS f WHERE media.id = ? AND f.id = ? AND media.transcript IS NULL AND f.transcript IS NOT NULL`, into, from); err != nil {
			return err
		}
		merged.MediaID, orphan = into, from
	}
	steps := []struct {
		q    string
		args []any
	}{
		// Detach drop's media first so deleting drop never orphans the media the merge keeps.
		{`UPDATE messages SET media_id = NULL WHERE id = ?`, []any{dropID}},
		{`DELETE FROM reactions WHERE message_id = ?1 AND EXISTS (
			SELECT 1 FROM reactions r WHERE r.message_id = ?2 AND r.person_id = reactions.person_id AND r.ts >= reactions.ts)`, []any{dropID, keepID}},
		{`DELETE FROM reactions WHERE message_id = ?2 AND EXISTS (
			SELECT 1 FROM reactions r WHERE r.message_id = ?1 AND r.person_id = reactions.person_id)`, []any{dropID, keepID}},
		{`UPDATE reactions SET message_id = ? WHERE message_id = ?`, []any{keepID, dropID}},
		{`DELETE FROM messages WHERE id = ?`, []any{dropID}},
	}
	for _, s := range steps {
		if _, err := e.ExecContext(ctx, s.q, s.args...); err != nil {
			return err
		}
	}
	if err := SaveMessage(ctx, e, merged); err != nil {
		return err
	}
	if orphan != 0 {
		_, err = e.ExecContext(ctx, `DELETE FROM media WHERE id = ? AND NOT EXISTS (SELECT 1 FROM messages WHERE media_id = ?)`, orphan, orphan)
	}
	return err
}
