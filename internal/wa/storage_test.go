package wa

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// A failed archive write withholds the ack, shows in the status, and renews
// the connection after a short wait so WhatsApp redelivers the message;
// the error clears once a write works again.
func TestStorageFailureForcesRedelivery(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	old := redeliverDelay
	redeliverDelay = 50 * time.Millisecond
	t.Cleanup(func() { redeliverDelay = old })
	var reconnects atomic.Int32
	c.reconnect = func() { reconnects.Add(1) }
	if _, err := c.db.Exec(`CREATE TRIGGER disk_full BEFORE INSERT ON raw_events BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	msg := func(id string) *events.Message {
		return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoPN, Sender: marcoPN}, ID: id, Timestamp: t0},
			Message: textMsg("ciao")}
	}
	if c.handle(msg("S1")) {
		t.Fatal("message acked although it was not archived")
	}
	c.handle(msg("S2")) // a second failure does not queue a second reconnect
	if s := c.Status(); !strings.Contains(s.StorageError, "disk I/O error") || s.StorageErrorAt.IsZero() {
		t.Fatalf("status: %+v", s)
	}
	deadline := time.Now().Add(5 * time.Second)
	for reconnects.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := reconnects.Load(); n != 1 {
		t.Fatalf("reconnects = %d, want 1", n)
	}
	// A failing group change is not reported as handled either.
	if c.handle(&events.GroupInfo{JID: group, Name: &types.GroupName{Name: "Calcetto"}, Timestamp: t0}) {
		t.Fatal("group change reported handled although it was not archived")
	}
	if _, err := c.db.Exec(`DROP TRIGGER disk_full`); err != nil {
		t.Fatal(err)
	}
	if !c.handle(msg("S1")) { // the redelivered message
		t.Fatal("redelivered message not acked")
	}
	if s := c.Status(); s.StorageError != "" {
		t.Fatalf("storage error kept after a good write: %q", s.StorageError)
	}
}

// A sent message is on the wire: its archive is retried until the send's
// deadline instead of being lost to a short storage hiccup.
func TestStoreSentRetriesArchive(t *testing.T) {
	in := newIngestor(t)
	if _, err := in.DB.Exec(`CREATE TRIGGER disk_full BEFORE INSERT ON raw_events BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		in.DB.Exec(`DROP TRIGGER disk_full`)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := in.StoreSent(ctx, marcoPN, whatsmeow.SendResponse{ID: "3EB0OUT1", Timestamp: t0}, textMsg("ciao"))
	if err != nil || id == 0 {
		t.Fatalf("StoreSent = %d, %v", id, err)
	}
}

// A temporary ban is persisted: a restart still knows when it ends.
func TestTemporaryBanPersists(t *testing.T) {
	dir := t.TempDir()
	c, _ := newTestClient(t, dir)
	start := time.Now()
	c.handle(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: 2 * time.Hour})
	until := c.Status().BannedUntil
	if until.Before(start.Add(2*time.Hour-time.Minute)) || until.After(start.Add(2*time.Hour+time.Minute)) || c.Status().BanReason == "" {
		t.Fatalf("ban = %v %q", until, c.Status().BanReason)
	}
	c2, _ := newTestClient(t, dir)
	if got := c2.Status().BannedUntil; !got.Equal(until.Truncate(time.Second)) {
		t.Fatalf("after restart: %v, want %v", got, until)
	}
}

// Activity of the primary phone (device 0) is recorded; other devices' is not.
func TestPhoneActivityRecorded(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	ctx := context.Background()
	laptop := myPN
	laptop.Device = 7
	c.handle(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoPN, Sender: laptop, IsFromMe: true},
		ID: "D7", Timestamp: t0}, Message: textMsg("dal portatile")})
	if at, _ := store.GetStateTS(ctx, c.db, store.KeyPhoneSeenAt); !at.IsZero() {
		t.Fatalf("another device counted as the phone: %v", at)
	}
	c.handle(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoPN, Sender: myPN, IsFromMe: true},
		ID: "D0", Timestamp: t0.Add(time.Minute)}, Message: textMsg("dal telefono")})
	if at, _ := store.GetStateTS(ctx, c.db, store.KeyPhoneSeenAt); !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("phone activity = %v", at)
	}
}
