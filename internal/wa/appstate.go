package wa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// App state recovery.
//
// An app state collection whose full snapshot fails the LTHash check cannot
// be synced from the server: the collection is broken on the account side.
// The primary phone can send a plain copy of it instead. The daemon asks for
// one with BuildAppStateRecoveryRequest, at most once per backoff period per
// collection, and whatsmeow applies the answer (AppStateSyncComplete with
// Recovery set). BuildFatalAppStateExceptionNotification is never used: it
// makes the phone reset the collection and logs out every linked device.

// refetchState paces full refetches of a collection whose incremental sync
// failed the LTHash check: a mismatch that keeps coming back (each full
// sync succeeds, the next patch fails again) must not turn into a stream of
// full syncs. The count starts over after a day without refetches.
type refetchState struct {
	running bool
	n       int       // refetches since the collection last synced
	last    time.Time // start of the last refetch
}

// refetchBackoff is the wait after the n-th refetch (n >= 1): 1 min,
// doubling, capped at 1 h.
func refetchBackoff(n int) time.Duration {
	if n < 1 {
		return 0
	}
	return min(time.Minute<<min(n-1, 6), time.Hour)
}

// goRefetchAppState refetches a collection with a full sync in the
// background, at most one at a time and once per backoff period.
func (c *Client) goRefetchAppState(name appstate.WAPatchName, now time.Time) {
	c.mu.Lock()
	st := c.refetches[name]
	if st == nil {
		st = &refetchState{}
		c.refetches[name] = st
	}
	if now.Sub(st.last) > 24*time.Hour {
		st.n = 0 // a mismatch after a quiet day starts over
	}
	if st.running || (st.n > 0 && now.Sub(st.last) < refetchBackoff(st.n)) || c.fetchAppState == nil {
		c.mu.Unlock()
		c.log.Debug("app state LTHash mismatch: full refetch already done recently", "name", name, "refetches", st.n)
		return
	}
	st.running, st.n, st.last = true, st.n+1, now
	fetch := c.fetchAppState
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() { c.mu.Lock(); st.running = false; c.mu.Unlock() }()
		c.log.Warn("app state LTHash mismatch: refetching with full sync", "name", name, "attempt", st.n)
		if err := fetch(c.ctx, name, true, false); err != nil && c.ctx.Err() == nil {
			c.log.Error("full app state resync failed", "name", name, "err", err)
		}
	}()
}

// recoveryState is the persisted recovery state of one collection.
type recoveryState struct {
	Attempts  int    `json:"attempts"`             // requests sent since the collection last synced
	LastAsked int64  `json:"last_asked,omitempty"` // unix time of the last request
	Synced    int64  `json:"synced,omitempty"`     // unix time of the last full sync or recovery
	Version   uint64 `json:"version,omitempty"`
	Recovered bool   `json:"recovered,omitempty"` // the last sync came from a recovery
}

// recoveryBackoff is the wait after the n-th request (n >= 1) before asking
// again: 1h, 2h, 4h ... capped at 24h. The phone answers only while online.
func recoveryBackoff(n int) time.Duration {
	if n < 1 {
		return 0
	}
	d := time.Hour << min(n-1, 5)
	return min(d, 24*time.Hour)
}

func loadRecovery(ctx context.Context, e store.Execer, name appstate.WAPatchName) (recoveryState, error) {
	var st recoveryState
	v, ok, err := store.GetState(ctx, e, store.KeyAppStateRecovery+string(name))
	if err != nil || !ok {
		return st, err
	}
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return recoveryState{}, nil // unreadable state: start over
	}
	return st, nil
}

func saveRecovery(ctx context.Context, e store.Execer, name appstate.WAPatchName, st recoveryState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return store.SetState(ctx, e, store.KeyAppStateRecovery+string(name), string(b))
}

// sendPeerFunc sends a message to the account's own primary device.
type sendPeerFunc func(ctx context.Context, msg *waE2E.Message) error

// goRecoverAppState asks the phone for a copy of a collection whose full
// sync failed the LTHash check, in the background.
func (c *Client) goRecoverAppState(name appstate.WAPatchName) {
	c.mu.Lock()
	if c.recovering[name] {
		c.mu.Unlock()
		return
	}
	c.recovering[name] = true
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() { c.mu.Lock(); delete(c.recovering, name); c.mu.Unlock() }()
		if _, err := c.requestRecovery(c.ctx, name, time.Now()); err != nil && c.ctx.Err() == nil {
			c.log.Error("app state recovery request", "name", name, "err", err)
		}
	}()
}

// requestRecovery sends one recovery request when the backoff allows it and
// records the attempt. sent is false when the previous request is still
// within its backoff period.
func (c *Client) requestRecovery(ctx context.Context, name appstate.WAPatchName, now time.Time) (sent bool, err error) {
	st, err := loadRecovery(ctx, c.db, name)
	if err != nil {
		return false, err
	}
	if st.Attempts > 0 && now.Sub(time.Unix(st.LastAsked, 0)) < recoveryBackoff(st.Attempts) {
		c.log.Debug("app state recovery already requested", "name", name, "attempts", st.Attempts,
			"next_after", time.Unix(st.LastAsked, 0).Add(recoveryBackoff(st.Attempts)))
		return false, nil
	}
	if c.sendPeer == nil {
		return false, errors.New("not connected")
	}
	if err := c.sendPeer(ctx, whatsmeow.BuildAppStateRecoveryRequest(name)); err != nil {
		return false, fmt.Errorf("send recovery request for %s: %w", name, err)
	}
	st.Attempts++
	st.LastAsked = now.Unix()
	if err := store.Retry(ctx, func() error { return saveRecovery(ctx, c.db, name, st) }); err != nil {
		return true, err
	}
	detail := fmt.Sprintf("%s attempt %d", name, st.Attempts)
	c.logEvent(store.EvAppStateAsk, detail)
	c.log.Warn("app state snapshot broken (LTHash mismatch): asked the phone for a copy", "name", name, "attempt", st.Attempts,
		"next_retry_after", recoveryBackoff(st.Attempts))
	return true, nil
}

// appStateSynced records that a collection synced. A full sync from the
// server proves the collection healthy and clears its recovery state. A
// recovery keeps the attempt count: the server copy is still broken, so the
// next mismatch waits for the backoff instead of asking the phone at once.
func (c *Client) appStateSynced(name appstate.WAPatchName, version uint64, recovery bool) {
	ctx := c.dbCtx

	err := store.Retry(ctx, func() error {
		st, err := loadRecovery(ctx, c.db, name)
		if err != nil {
			return err
		}
		if st.Attempts == 0 && st.LastAsked == 0 && !recovery {
			return nil // never broken: nothing to record
		}
		if !recovery {
			st = recoveryState{}
		}
		st.Synced, st.Version, st.Recovered = time.Now().Unix(), version, recovery
		return saveRecovery(ctx, c.db, name, st)
	})
	if err != nil {
		c.log.Error("record app state sync", "name", name, "err", err)
	}
	if recovery {
		c.logEvent(store.EvAppStateRecov, fmt.Sprintf("%s v%d", name, version))
		c.log.Info("app state recovered from the phone", "name", name, "version", version)
	}
}
