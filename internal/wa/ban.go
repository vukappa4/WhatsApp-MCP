package wa

import (
	"context"
	"encoding/json"
	"time"

	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// phoneSeen records activity of the account's primary phone (device 0): a
// message written on it, a read it sent, a history sync. WhatsApp unlinks
// linked devices when the phone stays offline for 14 days.
func (c *Client) phoneSeen(ctx context.Context, at time.Time) {
	if at.IsZero() || at.After(time.Now().Add(time.Hour)) {
		return
	}
	if err := store.BumpStateTS(ctx, c.db, store.KeyPhoneSeenAt, at.Unix()); err != nil {
		c.log.Warn("record phone activity", "err", err)
	}
}

// banUnknownExpiry is assumed when WhatsApp announces a temporary ban
// without its duration.
const banUnknownExpiry = 24 * time.Hour

// banRecord is the persisted temporary ban (store.KeyTemporaryBan).
type banRecord struct {
	Until  int64  `json:"until"`
	Reason string `json:"reason"`
}

// recordBan persists a temporary ban, so sends stay refused until it
// expires, across restarts too.
func (c *Client) recordBan(ctx context.Context, v *events.TemporaryBan, now time.Time) {
	exp := v.Expire
	if exp <= 0 {
		exp = banUnknownExpiry
	}
	rec := banRecord{Until: now.Add(exp).Unix(), Reason: v.Code.String()}
	c.mu.Lock()
	c.st.BannedUntil, c.st.BanReason = time.Unix(rec.Until, 0), rec.Reason
	c.mu.Unlock()
	b, _ := json.Marshal(rec)
	if err := store.Retry(ctx, func() error { return store.SetState(ctx, c.db, store.KeyTemporaryBan, string(b)) }); err != nil {
		c.log.Error("persist temporary ban", "err", err)
	}
}

// loadBan restores a temporary ban that has not expired.
func (c *Client) loadBan(ctx context.Context, now time.Time) {
	v, ok, err := store.GetState(ctx, c.db, store.KeyTemporaryBan)
	if err != nil || !ok {
		return
	}
	var rec banRecord
	if json.Unmarshal([]byte(v), &rec) != nil || time.Unix(rec.Until, 0).Before(now) {
		return
	}
	c.st.BannedUntil, c.st.BanReason = time.Unix(rec.Until, 0), rec.Reason
}
