package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
)

// migration is one schema step. noFK steps run with foreign keys off: SQLite
// changes a CHECK constraint only by rebuilding the table, and dropping a
// parent table with foreign keys on deletes through to its children.
type migration struct {
	sql  string
	noFK bool
}

// migrations is append-only. Entry i upgrades the schema to version i+1.
// Never edit an applied entry: add a new one.
var migrations = []migration{{sql: schemaV1}, {sql: schemaV2}, {sql: schemaV3}, {sql: schemaV4, noFK: true}, {sql: schemaV5}, {sql: schemaV6}, {sql: schemaV7}, {sql: schemaV8}, {sql: schemaV9}, {sql: schemaV10}, {sql: schemaV11}}

// SchemaVersion is the version a fresh database ends at.
func SchemaVersion() int { return len(migrations) }

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for v := 1; v <= len(migrations); v++ {
		m := migrations[v-1]
		// The version is read inside the IMMEDIATE transaction, so two processes
		// starting at once never apply the same migration twice.
		apply := func(tx *sql.Tx) error {
			var current int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
				return err
			}
			if current > len(migrations) {
				return fmt.Errorf("data.db schema v%d is newer than this binary (v%d)", current, len(migrations))
			}
			if current >= v {
				return nil
			}
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			if m.noFK {
				if err := foreignKeyCheck(ctx, tx); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, v, Now().Unix())
			return err
		}
		var err error
		if m.noFK {
			err = db.txWithoutFK(ctx, apply)
		} else {
			err = db.Tx(ctx, apply)
		}
		if err != nil {
			return fmt.Errorf("migrate to v%d: %w", v, err)
		}
	}
	return nil
}

// txWithoutFK runs fn in a transaction on one connection with foreign keys
// off, then turns them back on. A connection that cannot turn them back on is
// discarded, never returned to the pool.
func (db *DB) txWithoutFK(ctx context.Context, fn func(tx *sql.Tx) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		restore := context.WithoutCancel(ctx)
		var on int
		_, err := conn.ExecContext(restore, `PRAGMA foreign_keys = ON`)
		if err == nil {
			err = conn.QueryRowContext(restore, `PRAGMA foreign_keys`).Scan(&on)
		}
		if err != nil || on != 1 {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	return runTx(tx, fn)
}

// foreignKeyCheck fails when a message points at a missing media row: messages
// is the only table that references media.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check(messages)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign key violations after rebuild")
	}
	return rows.Err()
}

const schemaV1 = `
CREATE TABLE persons (
	id            INTEGER PRIMARY KEY,
	display_name  TEXT    NOT NULL DEFAULT '',
	name_source   TEXT    NOT NULL DEFAULT '',  -- full_name | first_name | business_name | push_name | phone | ''
	phone_e164    TEXT,                         -- +393331234567
	is_me         INTEGER NOT NULL DEFAULT 0,
	saved_contact INTEGER NOT NULL DEFAULT 0,
	full_name     TEXT    NOT NULL DEFAULT '',
	first_name    TEXT    NOT NULL DEFAULT '',
	business_name TEXT    NOT NULL DEFAULT '',
	push_name     TEXT    NOT NULL DEFAULT '',
	updated_at    INTEGER NOT NULL
);
CREATE INDEX persons_phone ON persons(phone_e164);
CREATE INDEX persons_name ON persons(display_name COLLATE NOCASE);

CREATE TABLE jids (
	jid       TEXT PRIMARY KEY,                 -- non-AD user JID: 39333...@s.whatsapp.net or 1234...@lid
	person_id INTEGER NOT NULL REFERENCES persons(id),
	kind      TEXT NOT NULL CHECK (kind IN ('pn', 'lid', 'other'))
);
CREATE INDEX jids_person ON jids(person_id);

CREATE TABLE chats (
	id              INTEGER PRIMARY KEY,
	jid             TEXT    NOT NULL UNIQUE,
	kind            TEXT    NOT NULL CHECK (kind IN ('dm', 'group', 'newsletter', 'broadcast')),
	person_id       INTEGER REFERENCES persons(id),  -- set for dm only
	name            TEXT    NOT NULL DEFAULT '',     -- group, newsletter and broadcast names; dm names come from persons
	archived        INTEGER NOT NULL DEFAULT 0,
	muted_until     INTEGER,
	unread_count    INTEGER NOT NULL DEFAULT 0,
	last_message_at INTEGER,                         -- max ts of stored messages, never moves backwards
	created_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX chats_dm_person ON chats(person_id) WHERE kind = 'dm';
CREATE INDEX chats_recency ON chats(last_message_at DESC);

CREATE TABLE media (
	id                INTEGER PRIMARY KEY,
	mimetype          TEXT    NOT NULL DEFAULT '',
	filename          TEXT    NOT NULL DEFAULT '',
	size              INTEGER,
	duration          INTEGER,                       -- seconds
	direct_path       TEXT    NOT NULL DEFAULT '',
	url               TEXT    NOT NULL DEFAULT '',
	media_key         BLOB,
	file_sha256       BLOB,
	file_enc_sha256   BLOB,
	local_path        TEXT    NOT NULL DEFAULT '',
	state             TEXT    NOT NULL DEFAULT 'remote' CHECK (state IN ('remote', 'downloading', 'downloaded', 'failed', 'gone')),
	error             TEXT    NOT NULL DEFAULT '',
	transcript        TEXT,
	transcript_lang   TEXT,
	transcript_engine TEXT
);

CREATE TABLE messages (
	id               INTEGER PRIMARY KEY,
	chat_id          INTEGER NOT NULL REFERENCES chats(id),
	wa_id            TEXT    NOT NULL,
	sender_person_id INTEGER REFERENCES persons(id),
	sender_jid       TEXT    NOT NULL DEFAULT '',
	from_me          INTEGER NOT NULL DEFAULT 0,
	ts               INTEGER NOT NULL,              -- unix seconds, UTC
	kind             TEXT    NOT NULL CHECK (kind IN ('text', 'image', 'video', 'audio', 'voice', 'document', 'sticker',
	                                                  'location', 'live_location', 'contact', 'poll', 'poll_vote', 'call', 'system')),
	text             TEXT    NOT NULL DEFAULT '',   -- body or caption
	reply_to_wa_id   TEXT,
	reply_preview    TEXT,
	edited_at        INTEGER,
	deleted_at       INTEGER,                       -- revoked; text keeps the original
	forwarded        INTEGER NOT NULL DEFAULT 0,
	media_id         INTEGER REFERENCES media(id),
	UNIQUE (chat_id, wa_id)
);
CREATE INDEX messages_chat_ts ON messages(chat_id, ts, id);
CREATE INDEX messages_sender ON messages(sender_person_id, ts);
CREATE INDEX messages_ts ON messages(ts);
CREATE INDEX messages_media ON messages(media_id) WHERE media_id IS NOT NULL;

CREATE TABLE reactions (
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	person_id  INTEGER NOT NULL REFERENCES persons(id),
	emoji      TEXT    NOT NULL,
	ts         INTEGER NOT NULL,
	PRIMARY KEY (message_id, person_id)
);
CREATE INDEX reactions_person ON reactions(person_id);

-- Full-text index over message text (body or caption) and media transcripts.
-- rowid = messages.id. Kept in sync by the triggers below.
CREATE VIRTUAL TABLE messages_fts USING fts5(text, transcript, tokenize = 'unicode61 remove_diacritics 2');

CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts(rowid, text, transcript)
	VALUES (new.id, new.text, COALESCE((SELECT transcript FROM media WHERE id = new.media_id), ''));
END;
CREATE TRIGGER messages_fts_update AFTER UPDATE OF text, media_id ON messages BEGIN
	DELETE FROM messages_fts WHERE rowid = old.id;
	INSERT INTO messages_fts(rowid, text, transcript)
	VALUES (new.id, new.text, COALESCE((SELECT transcript FROM media WHERE id = new.media_id), ''));
END;
CREATE TRIGGER messages_fts_delete AFTER DELETE ON messages BEGIN
	DELETE FROM messages_fts WHERE rowid = old.id;
END;
CREATE TRIGGER media_fts_transcript AFTER UPDATE OF transcript ON media BEGIN
	DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE media_id = new.id);
	INSERT INTO messages_fts(rowid, text, transcript)
	SELECT id, text, COALESCE(new.transcript, '') FROM messages WHERE media_id = new.id;
END;

CREATE TABLE connection_log (
	id     INTEGER PRIMARY KEY,
	ts     INTEGER NOT NULL,   -- unix seconds
	event  TEXT    NOT NULL,
	detail TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX connection_log_ts ON connection_log(ts);

-- Key/value sync state: last_event_at, alive_at, offline_sync, history_sync_*, import_*.
CREATE TABLE sync_state (
	key        TEXT PRIMARY KEY,
	value      TEXT    NOT NULL,
	updated_at INTEGER NOT NULL
);
`

// schemaV2: message provenance, chat description and state time, the raw
// event archive, and identity notes.
const schemaV2 = `
-- Provenance rank: legacy < history < live. A richer source upgrades a row.
-- Rows written before v2 can only come from the legacy import.
ALTER TABLE messages ADD COLUMN source TEXT NOT NULL DEFAULT 'live' CHECK (source IN ('legacy', 'history', 'live'));
UPDATE messages SET source = 'legacy';

ALTER TABLE chats ADD COLUMN description TEXT NOT NULL DEFAULT '';
-- When archived/muted/unread were last set; history snapshots never override it.
ALTER TABLE chats ADD COLUMN state_updated_at INTEGER;

-- Every event as received, gzip-compressed, committed before ingestion.
-- reingest rebuilds all derived tables from it.
CREATE TABLE raw_events (
	id          INTEGER PRIMARY KEY,
	kind        TEXT    NOT NULL,  -- see store.Raw* constants
	dedupe_key  TEXT    NOT NULL UNIQUE,
	received_at INTEGER NOT NULL,
	payload     BLOB    NOT NULL,
	ingested_at INTEGER,
	attempts    INTEGER NOT NULL DEFAULT 0,
	error       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX raw_events_pending ON raw_events(kind, id) WHERE ingested_at IS NULL;

-- Identity facts not applied automatically, such as a contact's number change.
CREATE TABLE identity_notes (
	id        INTEGER PRIMARY KEY,
	jid       TEXT    NOT NULL,
	other_jid TEXT    NOT NULL,
	note      TEXT    NOT NULL,
	ts        INTEGER NOT NULL,
	UNIQUE (jid, other_jid, note)
);
`

// schemaV3: app-state chat flags with per-flag action times, group info
// lookup state, album links, no empty DM chats, and the repair of stops
// written for pair sessions that had no heartbeat.
const schemaV3 = `
-- App-state actions (archive, pin, mute, mark read) carry their own time. Each
-- flag records the time of the action that set it, so an older action never
-- overrides a newer one, whatever order they arrive in.
ALTER TABLE chats ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE chats ADD COLUMN archived_set_at INTEGER;
ALTER TABLE chats ADD COLUMN pinned_set_at INTEGER;
ALTER TABLE chats ADD COLUMN muted_set_at INTEGER;
ALTER TABLE chats ADD COLUMN read_set_at INTEGER;

-- Group info lookups for groups without a name. Left groups usually fail;
-- the error and the attempt count drive the backoff.
ALTER TABLE chats ADD COLUMN info_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE chats ADD COLUMN info_checked_at INTEGER;
ALTER TABLE chats ADD COLUMN info_error TEXT NOT NULL DEFAULT '';

-- Album children (associatedChildMessage) point at the album message.
ALTER TABLE messages ADD COLUMN album_wa_id TEXT;

-- WhatsApp's own account (0@s.whatsapp.net) is named "WhatsApp".
UPDATE persons SET display_name = 'WhatsApp', name_source = 'system'
	WHERE phone_e164 = '+0' AND name_source NOT IN ('full_name', 'first_name');

-- A DM chat exists only once it holds a message. Persons stay.
DELETE FROM chats WHERE kind = 'dm' AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.chat_id = chats.id);

-- A pair session logged no heartbeat and no daemon_stop, so the next start
-- closed it at its last event and showed the rest of the session as offline.
-- Such a stop (the session before it followed a clean stop, or nothing, and
-- holds a paired event) moves to the start that wrote it: the exit time is
-- unknown, no gap is claimed. A pair that followed a crashed run keeps its
-- stop, so the crash still shows.
UPDATE connection_log SET
	ts = (SELECT s.ts FROM connection_log s WHERE s.event = 'daemon_start' AND s.id > connection_log.id ORDER BY s.id LIMIT 1),
	detail = 'pair session without heartbeat: exit time unknown, closed at the next start'
WHERE event = 'daemon_stop' AND detail = 'unclean exit: last heartbeat'
	AND EXISTS (SELECT 1 FROM connection_log s WHERE s.event = 'daemon_start' AND s.id > connection_log.id)
	AND COALESCE((SELECT b.event FROM connection_log b WHERE b.event IN ('daemon_start', 'daemon_stop') AND b.id < connection_log.id
		ORDER BY b.id DESC LIMIT 1), 'daemon_stop') = 'daemon_stop'
	AND EXISTS (SELECT 1 FROM connection_log p WHERE p.event = 'paired' AND p.id < connection_log.id
		AND p.id > COALESCE((SELECT MAX(b.id) FROM connection_log b
			WHERE b.event IN ('daemon_start', 'daemon_stop') AND b.id < connection_log.id), 0));
`

// schemaV4: media download states (pending, downloaded, failed, expired,
// unavailable), attempt bookkeeping and transcription errors. The table is
// rebuilt because SQLite cannot alter a CHECK constraint; the triggers that
// read media are recreated with it.
const schemaV4 = `
DROP TRIGGER messages_fts_insert;
DROP TRIGGER messages_fts_update;
DROP TRIGGER media_fts_transcript;

CREATE TABLE media_v4 (
	id                INTEGER PRIMARY KEY,
	mimetype          TEXT    NOT NULL DEFAULT '',
	filename          TEXT    NOT NULL DEFAULT '',
	size              INTEGER,
	duration          INTEGER,                       -- seconds
	direct_path       TEXT    NOT NULL DEFAULT '',
	url               TEXT    NOT NULL DEFAULT '',
	media_key         BLOB,
	file_sha256       BLOB,
	file_enc_sha256   BLOB,
	local_path        TEXT    NOT NULL DEFAULT '',   -- relative to <data dir>\media
	state             TEXT    NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'downloaded', 'failed', 'expired', 'unavailable')),
	error             TEXT    NOT NULL DEFAULT '',
	attempts          INTEGER NOT NULL DEFAULT 0,
	state_at          INTEGER,                       -- unix seconds of the last state change
	transcript        TEXT,                          -- NULL: not transcribed
	transcript_lang   TEXT,
	transcript_engine TEXT,
	transcript_error  TEXT    NOT NULL DEFAULT ''
);
INSERT INTO media_v4(id, mimetype, filename, size, duration, direct_path, url, media_key, file_sha256, file_enc_sha256,
	local_path, state, error, transcript, transcript_lang, transcript_engine)
SELECT id, mimetype, filename, size, duration, direct_path, url, media_key, file_sha256, file_enc_sha256,
	local_path, CASE state WHEN 'downloaded' THEN 'downloaded' WHEN 'failed' THEN 'failed' WHEN 'gone' THEN 'unavailable' ELSE 'pending' END,
	error, transcript, transcript_lang, transcript_engine
FROM media;
DROP TABLE media;
ALTER TABLE media_v4 RENAME TO media;
CREATE INDEX media_state ON media(state);

CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts(rowid, text, transcript)
	VALUES (new.id, new.text, COALESCE((SELECT transcript FROM media WHERE id = new.media_id), ''));
END;
CREATE TRIGGER messages_fts_update AFTER UPDATE OF text, media_id ON messages BEGIN
	DELETE FROM messages_fts WHERE rowid = old.id;
	INSERT INTO messages_fts(rowid, text, transcript)
	VALUES (new.id, new.text, COALESCE((SELECT transcript FROM media WHERE id = new.media_id), ''));
END;
CREATE TRIGGER media_fts_transcript AFTER UPDATE OF transcript ON media BEGIN
	DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE media_id = new.id);
	INSERT INTO messages_fts(rowid, text, transcript)
	SELECT id, text, COALESCE(new.transcript, '') FROM messages WHERE media_id = new.id;
END;
`

// schemaV11: every observation that a LID had a phone number, with the time
// of its newest evidence. A number belongs to the LID that had it last (jids
// follows); the person's other numbers are former ones. It starts empty: the
// next identity sync fills it, and a reingest rebuilds every person under
// these rules.
const schemaV11 = `
CREATE TABLE number_links (
	pn      TEXT    NOT NULL,  -- phone JID: 39333...@s.whatsapp.net
	lid     TEXT    NOT NULL,  -- LID: 1234...@lid
	seen_at INTEGER NOT NULL,  -- unix time of the newest evidence that lid had pn
	PRIMARY KEY (pn, lid)
);
CREATE INDEX number_links_lid ON number_links(lid);
`

// schemaV10: a downloaded media row's local_path is relative to the media
// root. A row holding an absolute path (a sent file's original location) has
// no file the media code serves: it is pending again, so a download, or the
// sent file kept in the media folder, fills it.
const schemaV10 = `
UPDATE media SET state = 'pending', local_path = ''
WHERE state = 'downloaded' AND (local_path LIKE '/%' OR local_path LIKE '\%' OR local_path GLOB '[A-Za-z]:*');
`

// schemaV9: media re-upload requests to the phone, counted per 24 h window.
const schemaV9 = `
ALTER TABLE media ADD COLUMN reupload_requests INTEGER NOT NULL DEFAULT 0;
ALTER TABLE media ADD COLUMN reupload_window_at INTEGER;
`

// schemaV8: media rows remember the chat JID and WhatsApp id of their
// message, so a reingest relinks them instead of losing downloads and
// transcripts.
const schemaV8 = `
ALTER TABLE media ADD COLUMN msg_chat_jid TEXT;
ALTER TABLE media ADD COLUMN msg_wa_id TEXT;
UPDATE media SET (msg_chat_jid, msg_wa_id) = (SELECT c.jid, m.wa_id FROM messages m JOIN chats c ON c.id = m.chat_id
	WHERE m.media_id = media.id ORDER BY m.id LIMIT 1);
CREATE INDEX media_msg_wa_id ON media(msg_wa_id);
`

// schemaV7: edits, revokes, reactions and poll votes that arrive before the
// message they act on wait here, as archived messages, until it is stored.
const schemaV7 = `
CREATE TABLE pending_mutations (
	id           INTEGER PRIMARY KEY,
	target_wa_id TEXT    NOT NULL,         -- WhatsApp id of the message acted on
	mutation_key TEXT    NOT NULL UNIQUE,  -- chat, sender and id of the mutation itself
	source       TEXT    NOT NULL,         -- live | history | legacy
	ts           INTEGER NOT NULL,         -- the mutation's own time; expires 30 days later
	payload      BLOB    NOT NULL          -- archived message JSON (package wa)
);
CREATE INDEX pending_mutations_target ON pending_mutations(target_wa_id);
`

// schemaV6: when a raw event was last tried, for the history blob backoff.
// Notifications whose blob failed to download within WhatsApp's retention
// (two weeks) are pending again: earlier failures, such as a few minutes
// offline, were final after five quick attempts.
const schemaV6 = `
ALTER TABLE raw_events ADD COLUMN attempted_at INTEGER;
UPDATE raw_events SET ingested_at = NULL, attempts = 0
WHERE kind = 'history_notification' AND ingested_at IS NOT NULL AND error != ''
	AND received_at > CAST(strftime('%s', 'now') AS INTEGER) - 14 * 86400;
`

// schemaV5: a phone re-upload that failed the media key check is retryable
// (failed), not final: only the phone saying the file is gone is unavailable.
const schemaV5 = `
UPDATE media SET state = 'failed', attempts = 0,
	error = 'the phone re-uploaded the file encrypted with a key it does not share; ask again later, or forward it from the phone'
WHERE state = 'unavailable' AND error LIKE 'the phone re-uploaded the file with a different key%';
`
