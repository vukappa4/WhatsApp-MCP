// Package store owns data.db: schema, migrations and queries.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // driver "sqlite"
)

// DSN builds the SQLite DSN every database in the daemon uses. whatsmeow's
// sqlstore Upgrade refuses to run without foreign_keys.
func DSN(path string) string {
	return "file:" + uriPath(path) +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
}

// ReadOnlyDSN opens a database without ever writing to it (used for legacy imports).
func ReadOnlyDSN(path string) string { return ReadOnlyDSNWait(path, 10*time.Second) }

// ReadOnlyDSNWait is ReadOnlyDSN waiting at most busy for a lock.
func ReadOnlyDSNWait(path string, busy time.Duration) string {
	return "file:" + uriPath(path) + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(" + strconv.FormatInt(busy.Milliseconds(), 10) + ")"
}

func uriPath(path string) string {
	// EscapedPath percent-encodes spaces, '%', '?' and '#', which SQLite decodes back.
	return (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
}

// DB is data.db.
type DB struct {
	*sql.DB
	Path string
}

// Open opens data.db and applies pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	sdb, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(8)
	sdb.SetConnMaxIdleTime(5 * time.Minute)
	db := &DB{DB: sdb, Path: path}
	if err := db.PingContext(ctx); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.migrate(ctx); err != nil {
		sdb.Close()
		return nil, err
	}
	return db, nil
}

// Tx runs fn in an IMMEDIATE transaction and commits when fn returns nil.
// It rolls back when fn fails or panics: a panic never leaves the
// connection inside an open transaction that holds the write lock.
func (db *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	return runTx(tx, fn)
}

// runTx runs fn in tx, committing on success and rolling back on an error or
// a panic (which then continues).
func runTx(tx *sql.Tx, fn func(tx *sql.Tx) error) error {
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	done = true
	return tx.Commit()
}

// Now is the clock used for updated_at columns. Tests replace it.
var Now = func() time.Time { return time.Now() }
