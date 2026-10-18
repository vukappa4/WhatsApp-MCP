package store

import (
	"container/list"
	"context"
	"database/sql"
	"sync"
)

// stmtCacheSize bounds the prepared statements a DB keeps. Query texts
// never depend on list lengths (lists go in as one JSON argument, see
// jsonArg), so the read tools use a fixed set of statements well below it.
const stmtCacheSize = 128

// stmtCache keeps prepared statements by query text and evicts the least
// recently used. modernc SQLite spends 100-200 us preparing a read with
// joins, often more than running it. An evicted statement closes once no
// caller holds it; rows already open keep it alive until they close.
type stmtCache struct {
	mu     sync.Mutex
	byQ    map[string]*list.Element
	lru    list.List // of *cachedStmt, most recent first
	closed bool
}

type cachedStmt struct {
	q       string
	st      *sql.Stmt
	refs    int
	evicted bool
}

// acquire returns the prepared statement for q, preparing it on first use,
// or nil when it cannot be prepared: the caller then runs q unprepared and
// gets the same result or error as before. Every acquired statement must be
// released.
func (c *stmtCache) acquire(ctx context.Context, db *sql.DB, q string) *cachedStmt {
	c.mu.Lock()
	if el, ok := c.byQ[q]; ok {
		cs := el.Value.(*cachedStmt)
		cs.refs++
		c.lru.MoveToFront(el)
		c.mu.Unlock()
		return cs
	}
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil
	}
	st, err := db.PrepareContext(ctx, q)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		st.Close()
		return nil
	}
	if el, ok := c.byQ[q]; ok { // prepared meanwhile by another caller
		cs := el.Value.(*cachedStmt)
		cs.refs++
		c.lru.MoveToFront(el)
		c.mu.Unlock()
		st.Close()
		return cs
	}
	if c.byQ == nil {
		c.byQ = make(map[string]*list.Element)
	}
	cs := &cachedStmt{q: q, st: st, refs: 1}
	c.byQ[q] = c.lru.PushFront(cs)
	var idle []*sql.Stmt
	for c.lru.Len() > stmtCacheSize {
		if old := c.evictLocked(c.lru.Back()); old != nil {
			idle = append(idle, old)
		}
	}
	c.mu.Unlock()
	for _, st := range idle {
		st.Close()
	}
	return cs
}

// evictLocked drops el from the cache and returns its statement when no
// caller holds it, for the caller to close outside the lock.
func (c *stmtCache) evictLocked(el *list.Element) *sql.Stmt {
	cs := c.lru.Remove(el).(*cachedStmt)
	delete(c.byQ, cs.q)
	cs.evicted = true
	if cs.refs == 0 {
		return cs.st
	}
	return nil
}

func (c *stmtCache) release(cs *cachedStmt) {
	c.mu.Lock()
	cs.refs--
	idle := cs.evicted && cs.refs == 0
	c.mu.Unlock()
	if idle {
		cs.st.Close()
	}
}

// close evicts every statement; later queries run unprepared.
func (c *stmtCache) close() {
	c.mu.Lock()
	c.closed = true
	var idle []*sql.Stmt
	for c.lru.Len() > 0 {
		if st := c.evictLocked(c.lru.Back()); st != nil {
			idle = append(idle, st)
		}
	}
	c.mu.Unlock()
	for _, st := range idle {
		st.Close()
	}
}

// QueryContext runs q as a cached prepared statement.
func (db *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	cs := db.stmts.acquire(ctx, db.DB, q)
	if cs == nil {
		return db.DB.QueryContext(ctx, q, args...)
	}
	defer db.stmts.release(cs)
	return cs.st.QueryContext(ctx, args...)
}

// QueryRowContext runs q as a cached prepared statement.
func (db *DB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	cs := db.stmts.acquire(ctx, db.DB, q)
	if cs == nil {
		return db.DB.QueryRowContext(ctx, q, args...)
	}
	defer db.stmts.release(cs)
	return cs.st.QueryRowContext(ctx, args...)
}

// Close closes the cached statements and then the database.
func (db *DB) Close() error {
	db.stmts.close()
	return db.DB.Close()
}
