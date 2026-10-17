package mcpserver

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

// The cached gaps follow every new connection event: a gap opening and
// closing shows up in the next freshness line and in whatsapp_status.
func TestFreshnessGapCacheFollowsLog(t *testing.T) {
	db := seed(t)
	ctx := context.Background()
	s := &Server{DB: db, WA: fixedStatus{State: wa.StateConnected, Since: t0}, Now: func() time.Time { return t0.Add(3 * time.Hour) }}
	same := func() {
		t.Helper()
		d, err := s.loadFresh(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want, err := store.Gaps(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(d.Gaps, want) {
			t.Fatalf("cached gaps %+v, connection_log gives %+v", d.Gaps, want)
		}
	}
	line := func(want string) {
		t.Helper()
		got, err := s.freshness(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, want) {
			t.Fatalf("line lacks %q: %s", want, got)
		}
		same()
	}
	line("gap 09:59 - 10:30 overlaps this range")
	line("gap 09:59 - 10:30 overlaps this range") // served from the cache
	store.LogConnection(ctx, db, t0.Add(150*time.Minute), store.EvDisconnected, "")
	line("gap 12:30 - now overlaps this range")
	store.LogConnection(ctx, db, t0.Add(160*time.Minute), store.EvConnected, "")
	line("gap 12:30 - 12:40 overlaps this range")
	// A row older than the newest still counts: Gaps orders by ts, so the
	// first gap now opens at the keepalive timeout.
	store.LogConnection(ctx, db, t0.Add(-30*time.Minute), store.EvKeepAliveLost, "")
	line("gap 09:30 - 10:30 overlaps this range")
	res, err := s.status(ctx, nil, statusIn{})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "Offline gaps (daemon not connected): 2 recorded:\n") || !strings.Contains(text, "(keepalive_timeout)") {
		t.Fatalf("status gaps:\n%s", text)
	}
}

// Parallel reads while connection events are appended never return gaps
// older than the log version they were read with, and end in step.
func TestFreshnessGapCacheConcurrent(t *testing.T) {
	db := seed(t)
	ctx := context.Background()
	s := &Server{DB: db, WA: fixedStatus{State: wa.StateConnected, Since: t0}, Now: func() time.Time { return t0.Add(24 * time.Hour) }}
	base, err := s.loadFresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Each disconnected row opens a gap, the connected row after it closes it.
	wantAtLeast := func(rows int64) int { return len(base.Gaps) + int(rows-base.Log.Rows+1)/2 }

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	wg.Add(1)
	go func() {
		defer wg.Done()
		at := t0.Add(3 * time.Hour)
		for i := 0; i < 20; i++ {
			ev := store.EvDisconnected
			if i%2 == 1 {
				ev = store.EvConnected
			}
			if err := store.LogConnection(ctx, db, at.Add(time.Duration(i)*time.Minute), ev, ""); err != nil {
				errs <- err.Error()
				return
			}
		}
	}()
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				d, err := s.loadFresh(ctx)
				if err != nil {
					errs <- err.Error()
					return
				}
				if n := wantAtLeast(d.Log.Rows); len(d.Gaps) < n {
					errs <- "stale gaps for the log version read"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	d, err := s.loadFresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := store.Gaps(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Gaps, want) || len(want) != len(base.Gaps)+10 {
		t.Fatalf("after the writes: cached %d gaps, connection_log %d, want %d", len(d.Gaps), len(want), len(base.Gaps)+10)
	}
}
