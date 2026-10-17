package wa

import (
	"fmt"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// clockTick is how often the clock watcher checks for a suspend.
const clockTick = 30 * time.Second

// suspended reports whether the time between two watcher ticks shows the
// system slept or the clock jumped: the wall clock moved well past the tick,
// or the wall and monotonic clocks disagree.
func suspended(wall, mono, tick time.Duration) bool {
	skew := wall - mono
	return wall > tick+2*time.Minute || skew > time.Minute || skew < -time.Minute
}

// watchClock detects a sleep or clock jump between ticks. The websocket
// does not survive a sleep, but nothing says so until a keepalive fails.
func (c *Client) watchClock() {
	t := time.NewTicker(clockTick)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		c.checkClock(last, now)
		last = now
	}
}

// checkClock compares two consecutive ticks (the wall clock and, when both
// carry one, the monotonic clock). On a suspend it records a gap from the
// last tick, shows the link as reconnecting until the next Connected, and
// forces a reconnect so the offline queue replays now.
func (c *Client) checkClock(last, now time.Time) {
	wall := now.Round(0).Sub(last.Round(0))
	mono := now.Sub(last)
	if !suspended(wall, mono, clockTick) {
		return
	}
	detail := fmt.Sprintf("no clock tick from %s to %s (wall %s, monotonic %s): system sleep or clock change",
		last.Format(time.RFC3339), now.Format(time.RFC3339), wall.Round(time.Second), mono.Round(time.Second))
	c.log.Warn("system resumed from sleep or the clock jumped: reconnecting", "from", last, "to", now)
	if err := store.Retry(c.dbCtx, func() error { return store.LogConnection(c.dbCtx, c.db, last, store.EvSuspended, detail) }); err != nil {
		c.log.Error("write connection_log", "event", store.EvSuspended, "err", err)
	}
	if s := c.Status().State; s == StateConnected || s == StateConnecting || s == StateReconnecting || s == StateDisconnected {
		c.mu.Lock()
		c.st.State, c.st.Since, c.st.Detail = StateReconnecting, now, "resumed after sleep"
		c.st.OfflinePending = OfflineUnknown
		c.mu.Unlock()
	}
	c.mu.RLock()
	reconnect := c.reconnect
	c.mu.RUnlock()
	if reconnect != nil {
		go reconnect()
	}
}
