package wa

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// groupAPI is the part of whatsmeow the name refresh uses. Tests fake it.
type groupAPI interface {
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
	GetSubscribedNewsletters(ctx context.Context) ([]*types.NewsletterMetadata, error)
}

// Group info lookups for groups without a name.
const (
	infoLookupsPerRun = 30            // at most this many GetGroupInfo calls per refresh
	infoBackoffBase   = 6 * time.Hour // wait after the first failure, doubling per failure
	infoBackoffMax    = 30 * 24 * time.Hour
)

// infoBackoff is the wait after the n-th failed lookup (n >= 1).
func infoBackoff(n int) time.Duration {
	if n < 1 {
		return 0
	}
	return min(infoBackoffBase<<min(n-1, 10), infoBackoffMax)
}

// nameRefresh stores group and newsletter names and descriptions.
type nameRefresh struct {
	db    *store.DB
	api   groupAPI
	now   func() time.Time
	pause time.Duration // between GetGroupInfo calls
	log   func(msg string, args ...any)
}

// run refreshes every joined group, every subscribed newsletter that has a
// chat, then looks up groups that still have no name (left groups, groups
// only seen in history). Lookups tolerate failure: a left group answers
// "not in group", which is recorded on the chat with a growing backoff.
func (r nameRefresh) run(ctx context.Context) error {
	var errs []error
	if groups, err := r.api.GetJoinedGroups(ctx); err != nil {
		errs = append(errs, err)
	} else if err := r.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, g := range groups {
			id, err := store.EnsureChat(ctx, tx, g.JID.String(), "group")
			if err != nil {
				return err
			}
			if err := r.storeGroup(ctx, tx, id, g.Name, g.Topic, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if nls, err := r.api.GetSubscribedNewsletters(ctx); err != nil {
		errs = append(errs, err)
	} else if err := r.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, n := range nls {
			id, err := store.ChatIDByJID(ctx, tx, n.ID.String())
			if err != nil || id == 0 {
				if err != nil {
					return err
				}
				continue // a newsletter chat is created by its first message
			}
			if err := r.storeGroup(ctx, tx, id, n.ThreadMeta.Name.Text, n.ThreadMeta.Description.Text, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := r.lookupUnnamed(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// storeGroup writes a looked-up name and description and the lookup
// outcome. lookupErr nil means success (attempts reset).
func (r nameRefresh) storeGroup(ctx context.Context, tx *sql.Tx, id int64, name, topic string, lookupErr error) error {
	if lookupErr != nil {
		_, err := tx.ExecContext(ctx, `UPDATE chats SET info_attempts = info_attempts + 1, info_checked_at = ?, info_error = ? WHERE id = ?`,
			r.now().Unix(), lookupErr.Error(), id)
		return err
	}
	if err := store.SetChatName(ctx, tx, id, name); err != nil {
		return err
	}
	if topic != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE chats SET description = ? WHERE id = ?`, topic, id); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE chats SET info_attempts = 0, info_checked_at = ?, info_error = '' WHERE id = ?`, r.now().Unix(), id)
	return err
}

// lookupUnnamed calls GetGroupInfo for groups without a name whose backoff
// has passed, most recent chats first. A rate limit ends the run.
func (r nameRefresh) lookupUnnamed(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT id, jid, info_attempts, COALESCE(info_checked_at, 0) FROM chats
		WHERE kind = 'group' AND name = '' ORDER BY COALESCE(last_message_at, 0) DESC, id DESC`)
	if err != nil {
		return err
	}
	type due struct {
		id  int64
		jid types.JID
	}
	var todo []due
	now := r.now()
	for rows.Next() {
		var id, checked int64
		var jid string
		var attempts int
		if err := rows.Scan(&id, &jid, &attempts, &checked); err != nil {
			rows.Close()
			return err
		}
		if attempts > 0 && now.Sub(time.Unix(checked, 0)) < infoBackoff(attempts) {
			continue
		}
		if j, err := types.ParseJID(jid); err == nil && len(todo) < infoLookupsPerRun {
			todo = append(todo, due{id, j})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	named, failed := 0, 0
	for i, d := range todo {
		if i > 0 && r.pause > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.pause):
			}
		}
		info, lerr := r.api.GetGroupInfo(ctx, d.jid)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Only answers about the group itself back off; a rate limit, a
		// timeout or a lost connection ends the run without recording.
		var name, topic string
		switch {
		case errors.Is(lerr, whatsmeow.ErrNotInGroup):
			lerr = errors.New("not in group (left or removed)")
		case errors.Is(lerr, whatsmeow.ErrGroupNotFound):
			lerr = errors.New("group does not exist")
		case lerr != nil:
			r.log("group info lookups stopped; continuing at the next refresh", "named", named, "failed", failed, "err", lerr)
			return nil
		case info.Name == "":
			lerr = errors.New("group has no name")
		default:
			name, topic = info.Name, info.Topic
		}
		if lerr != nil {
			failed++
		} else {
			named++
		}
		if err := store.Retry(ctx, func() error {
			return r.db.Tx(ctx, func(tx *sql.Tx) error { return r.storeGroup(ctx, tx, d.id, name, topic, lerr) })
		}); err != nil {
			return err
		}
	}
	if len(todo) > 0 {
		r.log("looked up groups without a name", "named", named, "failed", failed)
	}
	return nil
}
