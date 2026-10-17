package wa

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// fakeHistory serves history blobs by direct path.
type fakeHistory struct {
	mu        sync.Mutex
	connected bool
	err       error // every download fails with it when set
	downloads int
	deleted   []string
}

func (f *fakeHistory) IsConnected() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connected }

func (f *fakeHistory) DownloadHistorySync(_ context.Context, n *waE2E.HistorySyncNotification, _ bool) (*waHistorySync.HistorySync, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads++
	if f.err != nil {
		return nil, f.err
	}
	return &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum()}, nil
}

func (f *fakeHistory) DeleteMedia(_ context.Context, _ whatsmeow.MediaType, path string, _ []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, path)
	return nil
}

func (f *fakeHistory) set(connected bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected, f.err = connected, err
}

func (f *fakeHistory) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.downloads }

type notifRow struct {
	pending  bool
	attempts int
	err      string
}

func notifState(t *testing.T, c *Client, id string) notifRow {
	t.Helper()
	var r notifRow
	if err := c.db.QueryRow(`SELECT ingested_at IS NULL, attempts, error FROM raw_events WHERE dedupe_key = ?`, "hsn:"+id).
		Scan(&r.pending, &r.attempts, &r.err); err != nil {
		t.Fatal(err)
	}
	return r
}

// A blob that fails to download is retried with backoff for as long as
// WhatsApp keeps it, never while disconnected and never counting a lost
// connection as an attempt; a blob the server no longer has, or one past
// the retention, is final.
func TestHistoryBlobRetries(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	old := store.Now
	store.Now = func() time.Time { return now }
	t.Cleanup(func() { store.Now = old })
	fake := &fakeHistory{}
	c.history = fake
	n := &waE2E.HistorySyncNotification{DirectPath: proto.String("/v/hist1"), SyncType: waE2E.HistorySyncType_RECENT.Enum()}
	if err := c.archiveNotification(ctx, "N1", n); err != nil {
		t.Fatal(err)
	}

	// Disconnected: nothing is tried.
	c.processNotifications(now)
	if fake.count() != 0 || notifState(t, c, "N1") != (notifRow{true, 0, ""}) {
		t.Fatalf("tried while disconnected: %d %+v", fake.count(), notifState(t, c, "N1"))
	}
	// The connection drops during the download: no attempt counted.
	fake.set(true, whatsmeow.ErrNotConnected)
	c.processNotifications(now)
	if s := notifState(t, c, "N1"); !s.pending || s.attempts != 0 {
		t.Fatalf("lost connection counted: %+v", s)
	}
	// A short outage: failures back off 30 s, 1 min, 2 min ..., never final.
	fake.set(true, errors.New("dial tcp: no route to host"))
	tries := 0
	for i := 0; i < 12; i++ {
		c.processNotifications(now)
		tries++
		s := notifState(t, c, "N1")
		if !s.pending || s.attempts != tries || !strings.Contains(s.err, "no route") {
			t.Fatalf("attempt %d: %+v", tries, s)
		}
		wait := historyBackoff(tries)
		now = now.Add(wait - time.Second)
		c.processNotifications(now) // still within the backoff
		if notifState(t, c, "N1").attempts != tries {
			t.Fatalf("retried before the backoff of %s", wait)
		}
		now = now.Add(time.Second)
	}
	if historyBackoff(12) != time.Hour {
		t.Fatalf("backoff cap = %s", historyBackoff(12))
	}
	// Back online: the blob is stored, marked done and deleted from the server.
	fake.set(true, nil)
	c.processNotifications(now)
	if s := notifState(t, c, "N1"); s.pending || s.err != "" {
		t.Fatalf("after success: %+v", s)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "/v/hist1" {
		t.Fatalf("deleted = %v", fake.deleted)
	}
	var blobs int
	c.db.QueryRow(`SELECT COUNT(*) FROM raw_events WHERE kind = 'history' AND dedupe_key = 'history:N1'`).Scan(&blobs)
	if blobs != 1 {
		t.Fatal("blob not archived")
	}

	// Gone from the server: final at once.
	if err := c.archiveNotification(ctx, "N2", n); err != nil {
		t.Fatal(err)
	}
	fake.set(true, whatsmeow.ErrMediaDownloadFailedWith404)
	c.processNotifications(now)
	if s := notifState(t, c, "N2"); s.pending || !strings.Contains(s.err, "404") {
		t.Fatalf("404: %+v", s)
	}
	// Past the retention: final.
	if err := c.archiveNotification(ctx, "N3", n); err != nil {
		t.Fatal(err)
	}
	c.db.Exec(`UPDATE raw_events SET received_at = ? WHERE dedupe_key = 'hsn:N3'`, now.Add(-historyRetention-time.Hour).Unix())
	fake.set(true, errors.New("timeout"))
	c.processNotifications(now)
	if s := notifState(t, c, "N3"); s.pending {
		t.Fatalf("expired blob still pending: %+v", s)
	}

	// reingest gives failed notifications another chance; done ones stay done.
	if err := c.db.Tx(ctx, func(tx *sql.Tx) error { return store.ResetDerived(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	if s := notifState(t, c, "N2"); !s.pending || s.attempts != 0 || s.err != "" {
		t.Fatalf("failed notification after reset: %+v", s)
	}
	if s := notifState(t, c, "N1"); s.pending {
		t.Fatal("done notification reset")
	}
}

// Pairing does not wait forever on a blob that keeps failing.
func TestWaitQuietGivesUpOnStalledBlobs(t *testing.T) {
	var out bytes.Buffer
	start := time.Now()
	done := waitQuiet(context.Background(), make(chan struct{}), func() int { return 1 }, 20*time.Millisecond, 10*time.Millisecond, 150*time.Millisecond, &out)
	if done || time.Since(start) > 2*time.Second || !strings.Contains(out.String(), "made no progress") {
		t.Fatalf("done=%v after %s: %q", done, time.Since(start), out.String())
	}
}
