package store

import (
	"context"
	"database/sql"
	"errors"
)

// EnsureChat returns the chat with this JID, creating it with kind when
// missing. DM chats are created through people.EnsureDMChat instead.
func EnsureChat(ctx context.Context, e Execer, jid, kind string) (int64, error) {
	id, err := ChatIDByJID(ctx, e, jid)
	if err != nil || id != 0 {
		return id, err
	}
	res, err := e.ExecContext(ctx, `INSERT INTO chats(jid, kind, created_at) VALUES (?, ?, ?)`, jid, kind, Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ChatIDByJID returns the chat stored under exactly this JID, 0 when none.
func ChatIDByJID(ctx context.Context, e Execer, jid string) (int64, error) {
	var id int64
	err := e.QueryRowContext(ctx, `SELECT id FROM chats WHERE jid = ?`, jid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SetChatName stores a group, newsletter or broadcast name when it is not empty.
func SetChatName(ctx context.Context, e Execer, chatID int64, name string) error {
	if name == "" {
		return nil
	}
	_, err := e.ExecContext(ctx, `UPDATE chats SET name = ? WHERE id = ? AND name != ?`, name, chatID, name)
	return err
}

// BumpChat moves last_message_at forward to ts. It never moves it backwards.
func BumpChat(ctx context.Context, e Execer, chatID, ts int64) error {
	_, err := e.ExecContext(ctx, `UPDATE chats SET last_message_at = ? WHERE id = ? AND COALESCE(last_message_at, 0) < ?`, ts, chatID, ts)
	return err
}

// GroupNames returns the name of every group chat by chat id.
func GroupNames(ctx context.Context, e Execer) (map[int64]string, error) {
	rows, err := e.QueryContext(ctx, `SELECT id, name FROM chats WHERE kind = 'group' AND name != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}
