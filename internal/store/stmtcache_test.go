package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
)

// cachedQuery is the i-th distinct query text: it returns i+arg and i+arg+1.
func cachedQuery(i int) string {
	return fmt.Sprintf("SELECT %d + ?1 UNION ALL SELECT %d + ?1 + 1", i, i)
}

func (c *stmtCache) entry(q string) *cachedStmt {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byQ[q]; ok {
		return el.Value.(*cachedStmt)
	}
	return nil
}

func (c *stmtCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

func stmtIsClosed(st *sql.Stmt) bool {
	rows, err := st.QueryContext(context.Background(), 0)
	if err == nil {
		rows.Close()
	}
	return err != nil && err.Error() == "sql: statement is closed"
}

func TestStmtCacheReusesAndBounds(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	var got int
	if err := db.QueryRowContext(ctx, cachedQuery(0), 5).Scan(&got); err != nil || got != 5 {
		t.Fatalf("got %d, %v", got, err)
	}
	first := db.stmts.entry(cachedQuery(0))
	if first == nil || first.refs != 0 {
		t.Fatalf("query 0 not cached idle: %+v", first)
	}
	if err := db.QueryRowContext(ctx, cachedQuery(0), 6).Scan(&got); err != nil || got != 6 {
		t.Fatalf("got %d, %v", got, err)
	}
	if db.stmts.entry(cachedQuery(0)) != first {
		t.Fatal("query 0 prepared twice")
	}
	for i := 1; i <= stmtCacheSize; i++ {
		if err := db.QueryRowContext(ctx, cachedQuery(i), 1).Scan(&got); err != nil || got != i+1 {
			t.Fatalf("query %d: got %d, %v", i, got, err)
		}
	}
	if n := db.stmts.len(); n != stmtCacheSize {
		t.Fatalf("cache holds %d statements, want %d", n, stmtCacheSize)
	}
	if db.stmts.entry(cachedQuery(0)) != nil {
		t.Fatal("least recently used query 0 not evicted")
	}
	if !stmtIsClosed(first.st) {
		t.Fatal("evicted idle statement left open")
	}
	if db.stmts.entry(cachedQuery(1)) == nil {
		t.Fatal("query 1 evicted before it was least recently used")
	}
}

// An evicted statement whose rows are still open keeps serving them and
// closes with its last holder.
func TestStmtCacheEvictsWhileRowsOpen(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, cachedQuery(0), 10)
	if err != nil {
		t.Fatal(err)
	}
	held := db.stmts.entry(cachedQuery(0))
	if !rows.Next() {
		t.Fatal(rows.Err())
	}
	for i := 1; i <= stmtCacheSize+5; i++ {
		var got int
		if err := db.QueryRowContext(ctx, cachedQuery(i), 0).Scan(&got); err != nil || got != i {
			t.Fatalf("query %d: got %d, %v", i, got, err)
		}
	}
	if db.stmts.entry(cachedQuery(0)) != nil {
		t.Fatal("query 0 not evicted")
	}
	var a, b int
	if err := rows.Scan(&a); err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("second row lost after eviction: %v", rows.Err())
	}
	if err := rows.Scan(&b); err != nil || a != 10 || b != 11 {
		t.Fatalf("rows after eviction: %d %d %v", a, b, err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !stmtIsClosed(held.st) {
		t.Fatal("evicted statement left open after its rows closed")
	}
}

// One goroutine per pool connection: callers never wait for a connection, so
// database/sql opens none in the background that could outlive Close and
// keep the temp dir busy on Windows.
func TestStmtCacheConcurrentEviction(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 600 {
				i := (g*131 + n*7) % (2 * stmtCacheSize)
				if n%3 == 0 {
					rows, err := db.QueryContext(ctx, cachedQuery(i), n)
					if err != nil {
						errs <- err
						return
					}
					var vals []int
					for rows.Next() {
						var v int
						if err := rows.Scan(&v); err != nil {
							errs <- err
							return
						}
						vals = append(vals, v)
					}
					rows.Close()
					if err := rows.Err(); err != nil || len(vals) != 2 || vals[0] != i+n || vals[1] != i+n+1 {
						errs <- fmt.Errorf("query %d arg %d: %v %v", i, n, vals, err)
						return
					}
					continue
				}
				var v int
				if err := db.QueryRowContext(ctx, cachedQuery(i), n).Scan(&v); err != nil || v != i+n {
					errs <- fmt.Errorf("query %d arg %d: %d %v", i, n, v, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := db.stmts.len(); n > stmtCacheSize {
		t.Fatalf("cache holds %d statements, over %d", n, stmtCacheSize)
	}
	db.stmts.mu.Lock()
	defer db.stmts.mu.Unlock()
	for el := db.stmts.lru.Front(); el != nil; el = el.Next() {
		if cs := el.Value.(*cachedStmt); cs.refs != 0 {
			t.Fatalf("%q still holds %d references", cs.q, cs.refs)
		}
	}
}

func TestStmtCacheErrorsAndClose(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	const bad = "SELECT FROM WHERE"
	_, want := db.DB.QueryContext(ctx, bad)
	if _, err := db.QueryContext(ctx, bad); err == nil || want == nil || err.Error() != want.Error() {
		t.Fatalf("bad query: got %v, want %v", err, want)
	}
	if err := db.QueryRowContext(ctx, bad).Scan(new(int)); err == nil || err.Error() != want.Error() {
		t.Fatalf("bad row query: got %v, want %v", err, want)
	}
	if db.stmts.entry(bad) != nil {
		t.Fatal("failed statement cached")
	}
	var got int
	if err := db.QueryRowContext(ctx, cachedQuery(0), 1).Scan(&got); err != nil {
		t.Fatal(err)
	}
	st := db.stmts.entry(cachedQuery(0)).st
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if db.stmts.len() != 0 || !stmtIsClosed(st) {
		t.Fatal("Close left statements cached or open")
	}
	if _, err := db.QueryContext(ctx, cachedQuery(0), 1); err == nil || err.Error() != "sql: database is closed" {
		t.Fatalf("query after Close: %v", err)
	}
}

// A statement evicted or closed while a caller holds it stays open until
// that caller releases it.
func TestStmtCacheClosesHeldStatementOnRelease(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	held := db.stmts.acquire(ctx, db.DB, cachedQuery(0))
	for i := 1; i <= stmtCacheSize; i++ {
		if err := db.QueryRowContext(ctx, cachedQuery(i), 0).Scan(new(int)); err != nil {
			t.Fatal(err)
		}
	}
	if db.stmts.entry(cachedQuery(0)) != nil {
		t.Fatal("held query 0 not evicted")
	}
	if stmtIsClosed(held.st) {
		t.Fatal("evicted statement closed while held")
	}
	db.stmts.release(held)
	if !stmtIsClosed(held.st) {
		t.Fatal("evicted statement left open after release")
	}

	held = db.stmts.acquire(ctx, db.DB, cachedQuery(1))
	db.stmts.close()
	if stmtIsClosed(held.st) {
		t.Fatal("close closed a held statement")
	}
	db.stmts.release(held)
	if !stmtIsClosed(held.st) {
		t.Fatal("statement left open after close and release")
	}
}
