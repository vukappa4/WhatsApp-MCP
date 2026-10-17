package wa

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// heartbeatEvery is how often a session records that it is alive. Tests shorten it.
var heartbeatEvery = time.Minute

// Lifecycle records one session of a process that connects to WhatsApp
// (run or pair) in connection_log: daemon_start (closing a previous session
// that ended without a stop, see store.RecordDaemonStart), a heartbeat every
// minute, and daemon_stop on a clean end. The client logs its own connects
// and disconnects; the disconnect End causes is covered by daemon_stop.
type Lifecycle struct {
	db     *store.DB
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// BeginLifecycle logs daemon_start for mode ("run", "pair") and starts the heartbeat.
func BeginLifecycle(ctx context.Context, db *store.DB, log *slog.Logger, mode, version string) (*Lifecycle, error) {
	now := time.Now()
	if err := store.RecordDaemonStart(ctx, db, now, fmt.Sprintf("%s version %s pid %d", mode, version, os.Getpid())); err != nil {
		return nil, err
	}
	if err := store.Heartbeat(ctx, db, now); err != nil {
		return nil, err
	}
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &Lifecycle{db: db, log: log, cancel: cancel, done: make(chan struct{})}
	go l.beat(hctx)
	return l, nil
}

func (l *Lifecycle) beat(ctx context.Context) {
	defer close(l.done)
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := store.Heartbeat(ctx, l.db, time.Now()); err != nil && ctx.Err() == nil {
			l.log.Warn("heartbeat", "err", err)
		}
	}
}

// End stops the client (when not nil) and the heartbeat, then logs
// daemon_stop as a clean stop with reason. Only the first call acts.
func (l *Lifecycle) End(client *Client, reason string) {
	l.once.Do(func() {
		if client != nil {
			client.Stop()
		}
		l.cancel()
		<-l.done
		if err := store.LogConnection(context.Background(), l.db, time.Now(), store.EvDaemonStop, "clean stop ("+reason+")"); err != nil {
			l.log.Error("log daemon stop", "err", err)
		}
	})
}
