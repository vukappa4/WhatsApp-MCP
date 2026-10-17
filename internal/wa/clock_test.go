package wa

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

func TestSuspendedDetection(t *testing.T) {
	for _, tc := range []struct {
		wall, mono time.Duration
		want       bool
	}{
		{30 * time.Second, 30 * time.Second, false},
		{45 * time.Second, 45 * time.Second, false},            // a slow tick
		{3 * time.Hour, 3 * time.Hour, true},                   // sleep, monotonic clock kept running
		{3 * time.Hour, 30 * time.Second, true},                // sleep, monotonic clock paused
		{30*time.Second + 2*time.Hour, 30 * time.Second, true}, // clock set forward
		{30*time.Second - 2*time.Hour, 30 * time.Second, true}, // clock set back
	} {
		if got := suspended(tc.wall, tc.mono, clockTick); got != tc.want {
			t.Errorf("suspended(wall %s, mono %s) = %v", tc.wall, tc.mono, got)
		}
	}
}

// After a sleep the link shows reconnecting (not stale "connected"), the
// sleep is an offline gap, and the next Connected closes it.
func TestResumeAfterSleep(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	ctx := context.Background()
	c.setState(StateConnected, "")
	before := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if err := store.LogConnection(ctx, c.db, before.Add(-time.Hour), store.EvConnected, ""); err != nil {
		t.Fatal(err)
	}
	c.checkClock(before, before.Add(clockTick)) // a normal tick
	if s := c.Status(); s.State != StateConnected {
		t.Fatalf("normal tick changed the state to %s", s.State)
	}
	c.checkClock(before, before.Add(3*time.Hour))
	if s := c.Status(); s.State != StateReconnecting || s.OfflinePending != OfflineUnknown {
		t.Fatalf("after sleep: %+v", s)
	}
	gaps, err := store.Gaps(ctx, c.db)
	if err != nil || len(gaps) == 0 {
		t.Fatalf("gaps = %v, %v", gaps, err)
	}
	g := gaps[len(gaps)-1]
	if !g.From.Equal(before) || !g.To.IsZero() || g.Reason != store.EvSuspended {
		t.Fatalf("sleep gap = %+v", g)
	}
	c.handle(&events.Connected{})
	if s := c.Status(); s.State != StateConnected {
		t.Fatalf("after reconnect: %s", s.State)
	}
	gaps, _ = store.Gaps(ctx, c.db)
	if g := gaps[len(gaps)-1]; g.To.IsZero() {
		t.Fatalf("gap still open: %+v", g)
	}
}

// An incremental app state mismatch refetches the collection with a full
// sync, but not again within the backoff (1 min, doubling up to 1 h).
func TestAppStateRefetchBackoff(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	var mu sync.Mutex
	calls := 0
	c.fetchAppState = func(context.Context, appstate.WAPatchName, bool, bool) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}
	count := func() int { c.wg.Wait(); mu.Lock(); defer mu.Unlock(); return calls }
	now := time.Now()
	c.goRefetchAppState(appstate.WAPatchRegular, now)
	c.goRefetchAppState(appstate.WAPatchRegular, now.Add(30*time.Second))
	if n := count(); n != 1 {
		t.Fatalf("refetches within a minute = %d", n)
	}
	c.goRefetchAppState(appstate.WAPatchRegular, now.Add(61*time.Second))
	c.goRefetchAppState(appstate.WAPatchRegular, now.Add(2*time.Minute)) // second backoff is 2 min
	if n := count(); n != 2 {
		t.Fatalf("refetches = %d, want 2", n)
	}
	c.goRefetchAppState(appstate.WAPatchCriticalBlock, now) // another collection has its own pace
	if n := count(); n != 3 {
		t.Fatalf("refetches = %d, want 3", n)
	}
	if refetchBackoff(20) != time.Hour {
		t.Fatalf("cap = %s", refetchBackoff(20))
	}
}
