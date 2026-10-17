package wa

import (
	"context"
	"errors"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// StorageReporter hears whether archiving events works. The client shows a
// failure in the status and makes WhatsApp redeliver what was not acked.
type StorageReporter interface {
	StorageFailed(err error)
	StorageOK()
}

// archiveError is an event that could not be archived: nothing about it is
// stored, and a message must not be acked.
type archiveError struct{ err error }

func (e archiveError) Error() string { return "archive: " + e.err.Error() }
func (e archiveError) Unwrap() error { return e.err }

// isArchiveError reports whether err means the event was not archived.
func isArchiveError(err error) bool {
	var a archiveError
	return errors.As(err, &a)
}

// archive stores a raw event (store.DB.ArchiveRaw) and reports the outcome
// to in.Storage.
func (in *Ingestor) archive(ctx context.Context, kind, key string, payload []byte) (store.RawEvent, error) {
	ev, err := in.DB.ArchiveRaw(ctx, kind, key, payload, in.now())
	if err != nil {
		if in.Storage != nil {
			in.Storage.StorageFailed(err)
		}
		return ev, archiveError{err}
	}
	if in.Storage != nil {
		in.Storage.StorageOK()
	}
	return ev, nil
}

// Redelivery after a storage failure: WhatsApp resends unacked messages when
// the connection is renewed. The first reconnect waits redeliverDelay; while
// storage keeps failing, the wait doubles up to redeliverMax.
var (
	redeliverDelay = 30 * time.Second
	redeliverMax   = 10 * time.Minute
)

// StorageFailed records a failed archive write in the status and schedules
// a reconnect, so the messages that were not acked come back now instead of
// whenever the connection next drops.
func (c *Client) StorageFailed(err error) {
	c.mu.Lock()
	if c.st.StorageError == "" {
		c.st.StorageErrorAt = time.Now()
	}
	c.st.StorageError = err.Error()
	schedule := !c.redeliverQueued && !c.stopping
	c.redeliverQueued = true
	if c.redeliverWait == 0 {
		c.redeliverWait = redeliverDelay
	}
	wait := c.redeliverWait
	c.redeliverWait = min(c.redeliverWait*2, redeliverMax)
	c.mu.Unlock()
	if !schedule {
		return
	}
	c.log.Error("storage error: messages are not acked and WhatsApp will redeliver them", "err", err, "reconnect_in", wait)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(wait):
		}
		c.mu.Lock()
		c.redeliverQueued = false
		reconnect := c.reconnect
		c.mu.Unlock()
		if reconnect != nil {
			c.log.Info("reconnecting so WhatsApp redelivers messages that could not be stored")
			reconnect()
		}
	}()
}

// StorageOK clears the storage error after an archive write succeeded.
func (c *Client) StorageOK() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.StorageError != "" {
		c.log.Info("storage works again", "failing_since", c.st.StorageErrorAt)
	}
	c.st.StorageError, c.st.StorageErrorAt = "", time.Time{}
	c.redeliverWait = 0
}
