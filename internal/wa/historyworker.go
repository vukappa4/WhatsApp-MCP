package wa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// archivedNotification is the raw_events payload of a history sync notification.
type archivedNotification struct {
	MessageID string `json:"message_id"`
	Notif     []byte `json:"notification"` // waE2E.HistorySyncNotification proto
}

// History blob downloads retry with exponential backoff while WhatsApp may
// still hold the blob.
const (
	// historyRetention is how long WhatsApp's servers keep a history blob
	// for the device to download; past it a failure is final.
	historyRetention = 14 * 24 * time.Hour
	historyRetryMin  = 30 * time.Second
	historyRetryMax  = time.Hour
	historyTick      = 15 * time.Second
)

// historyBackoff is the wait after the n-th failed download before the next
// try: 30 s, 1 min, 2 min ... capped at 1 h.
func historyBackoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return min(historyRetryMin<<min(n-1, 12), historyRetryMax)
}

// historyAPI is the part of the whatsmeow client the history worker uses.
type historyAPI interface {
	IsConnected() bool
	DownloadHistorySync(ctx context.Context, notif *waE2E.HistorySyncNotification, synchronousStorage bool) (*waHistorySync.HistorySync, error)
	DeleteMedia(ctx context.Context, appInfo whatsmeow.MediaType, directPath string, encFileHash []byte, encHandle string) error
}

// archiveNotification stores a history sync notification before the
// message is acked. The blob itself is downloaded by the worker.
func (c *Client) archiveNotification(ctx context.Context, msgID string, n *waE2E.HistorySyncNotification) error {
	nb, err := marshalPartial.Marshal(n)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(archivedNotification{MessageID: msgID, Notif: nb})
	if err != nil {
		return err
	}
	if _, err := c.ing.archive(ctx, store.RawHistoryNotify, "hsn:"+msgID, payload); err != nil {
		return err
	}
	select {
	case c.arrivals <- struct{}{}:
	default:
	}
	c.kickWorker()
	return nil
}

func (c *Client) kickWorker() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) startWorker() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		t := time.NewTicker(historyTick)
		defer t.Stop()
		for {
			c.processNotifications(time.Now())
			select {
			case <-c.ctx.Done():
				return
			case <-c.wake:
			case <-t.C:
			}
		}
	}()
}

// processNotifications downloads every pending history blob whose backoff
// has passed, while WhatsApp is connected: archives it, marks the
// notification done, deletes the blob from WhatsApp's server and ingests it.
// Nothing is tried, and no attempt counted, while disconnected.
func (c *Client) processNotifications(now time.Time) {
	if c.history == nil {
		return
	}
	after := int64(0)
	for c.ctx.Err() == nil {
		if !c.history.IsConnected() {
			return
		}
		evs, err := store.ListRaw(c.dbCtx, c.db, store.RawFilter{Kinds: []string{store.RawHistoryNotify}, PendingOnly: true, AfterID: after, Limit: 20})
		if err != nil {
			c.log.Error("list history notifications", "err", err)
			return
		}
		if len(evs) == 0 {
			return
		}
		after = evs[len(evs)-1].ID
		for _, ev := range evs {
			if c.ctx.Err() != nil || !c.history.IsConnected() {
				return
			}
			if !ev.AttemptedAt.IsZero() && now.Before(ev.AttemptedAt.Add(historyBackoff(ev.Attempts))) {
				continue
			}
			c.processNotification(ev, now)
		}
	}
}

// notConnected reports a download that failed only because the connection
// went away: it is retried without counting an attempt.
func notConnected(err error) bool {
	return errors.Is(err, whatsmeow.ErrNotConnected) || errors.Is(err, whatsmeow.ErrNotLoggedIn) ||
		errors.Is(err, context.Canceled)
}

// blobGone reports a blob the server no longer has.
func blobGone(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

func (c *Client) processNotification(ev store.RawEvent, now time.Time) {
	mark := func(err error, final bool) {
		if merr := store.Retry(c.dbCtx, func() error { return store.MarkRaw(c.dbCtx, c.db, ev.ID, err, final) }); merr != nil {
			c.log.Error("mark history notification", "id", ev.ID, "err", merr)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("history blob processing panicked", "id", ev.ID, "panic", r, "stack", string(debug.Stack()))
			mark(fmt.Errorf("panic: %v", r), now.Sub(ev.ReceivedAt) > historyRetention)
		}
	}()
	var an archivedNotification
	var n waE2E.HistorySyncNotification
	if err := json.Unmarshal(ev.Payload, &an); err != nil {
		mark(err, true)
		return
	}
	if err := unmarshalPartial.Unmarshal(an.Notif, &n); err != nil {
		mark(err, true)
		return
	}
	blob, err := c.history.DownloadHistorySync(c.ctx, &n, true)
	if err != nil {
		if notConnected(err) || c.ctx.Err() != nil || !c.history.IsConnected() {
			c.log.Info("history blob download interrupted: connection lost, retrying when connected", "message", an.MessageID, "err", err)
			return
		}
		expired := now.Sub(ev.ReceivedAt) > historyRetention
		final := blobGone(err) || expired
		level := slog.LevelWarn
		if final {
			level = slog.LevelError
		}
		c.log.Log(c.dbCtx, level, "download history blob", "message", an.MessageID, "attempt", ev.Attempts+1, "final", final,
			"next_retry_in", historyBackoff(ev.Attempts+1), "err", err)
		mark(fmt.Errorf("download: %w", err), final)
		return
	}
	archiveErr, ingestErr := c.ing.ArchiveHistory(c.dbCtx, "history:"+an.MessageID, blob)
	if archiveErr != nil {
		c.log.Error("archive history blob", "message", an.MessageID, "err", archiveErr)
		mark(archiveErr, false)
		return
	}
	mark(nil, true)
	if ingestErr != nil {
		c.log.Error("history blob ingested with errors", "message", an.MessageID, "err", ingestErr)
	}
	if n.GetInitialHistBootstrapInlinePayload() == nil && n.GetDirectPath() != "" {
		if err := c.history.DeleteMedia(c.ctx, whatsmeow.MediaHistory, n.GetDirectPath(), n.GetFileEncSHA256(), n.GetEncHandle()); err != nil {
			c.log.Warn("delete history blob from server", "err", err)
		}
	}
	c.goSyncIdentities()
}

// pendingHistory counts history notifications and blobs not yet ingested.
func (c *Client) pendingHistory() int {
	n, err := store.CountPendingRaw(c.dbCtx, c.db, store.RawHistoryNotify, store.RawHistory)
	if err != nil {
		return 1
	}
	return n
}

// waitQuiet returns once no arrival was seen for idle and pending reports
// no queued work, or when ctx ends. Each arrival restarts the quiet period,
// so a slow ingest never ends the wait while notifications keep coming.
// Pending work that makes no progress (no arrival, the count not falling)
// for stall ends the wait too: a blob that keeps failing is left to the
// daemon's retries instead of holding pairing forever. It reports whether
// everything was stored.
func waitQuiet(ctx context.Context, arrivals <-chan struct{}, pending func() int, idle, poll, stall time.Duration, out io.Writer) bool {
	seen := 0
	timer := time.NewTimer(idle)
	defer timer.Stop()
	lastProgress, lastPending := time.Now(), -1
	for {
		select {
		case <-ctx.Done():
			return false
		case <-arrivals:
			seen++
			lastProgress = time.Now()
			fmt.Fprintf(out, "history notifications received: %d\n", seen)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			n := pending()
			if n == 0 {
				return true
			}
			if lastPending < 0 || n < lastPending {
				lastProgress = time.Now()
			}
			lastPending = n
			if time.Since(lastProgress) >= stall {
				fmt.Fprintf(out, "%d history blobs made no progress for %s: the daemon keeps retrying them\n", n, stall)
				return false
			}
			fmt.Fprintf(out, "waiting for %d history blobs to finish\n", n)
			timer.Reset(poll)
		}
	}
}
