package wa

import (
	"context"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// panickyLIDs panics on every lookup, like a bug deep in a dependency.
type panickyLIDs struct{}

func (panickyLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) {
	panic("lid store exploded")
}

func rawRow(t *testing.T, in *Ingestor, waID string) (ingested bool, errText string) {
	t.Helper()
	if err := in.DB.QueryRow(`SELECT ingested_at IS NOT NULL, error FROM raw_events WHERE dedupe_key LIKE ?`, "%:"+waID).Scan(&ingested, &errText); err != nil {
		t.Fatal(err)
	}
	return
}

// A panic while ingesting is recorded on the archived event (final, the
// replay would panic again) and the next message is stored normally.
func TestIngestionPanicIsRecorded(t *testing.T) {
	in := newIngestor(t)
	in.LIDs = panickyLIDs{}
	ctx := context.Background()
	stranger := types.NewJID("222222222222222", types.HiddenUserServer)
	archived, err := in.LiveMessage(ctx, &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: stranger, Sender: stranger},
		ID: "PANIC1", Timestamp: t0}, Message: textMsg("ciao")}, nil)
	if !archived || err == nil || !strings.Contains(err.Error(), "panic while ingesting: lid store exploded") {
		t.Fatalf("archived %v, err %v", archived, err)
	}
	if ingested, e := rawRow(t, in, "PANIC1"); !ingested || !strings.Contains(e, "panic") {
		t.Fatalf("raw event: ingested %v, error %q", ingested, e)
	}
	live(t, in, types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoPN, Sender: marcoPN}, ID: "OK1", Timestamp: t0}, textMsg("dopo"))
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages WHERE wa_id = 'OK1'`); n != 1 {
		t.Fatal("message after the panic not stored")
	}

	// Startup replay: a panicking event is marked and the replay goes on.
	if _, err := in.DB.Exec(`UPDATE raw_events SET ingested_at = NULL, attempts = 0, error = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := in.ReplayPending(ctx); err != nil {
		t.Fatal(err)
	}
	if ingested, e := rawRow(t, in, "PANIC1"); !ingested || !strings.Contains(e, "panic") {
		t.Fatalf("replayed raw event: ingested %v, error %q", ingested, e)
	}
	if ingested, e := rawRow(t, in, "OK1"); !ingested || e != "" {
		t.Fatalf("replay stopped at the panic: %v %q", ingested, e)
	}
}

// The event handler archives before anything that can panic: a panic after
// the archive acks (the message is safe), one before it withholds the ack.
func TestHandlerPanicAcksOnlyArchivedMessages(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	c.ing.LIDs = panickyLIDs{}
	stranger := types.NewJID("222222222222222", types.HiddenUserServer)
	msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: stranger, Sender: stranger},
		ID: "PANIC2", Timestamp: t0}, Message: textMsg("ciao")}
	if !c.handle(msg) {
		t.Fatal("archived message not acked")
	}
	if ingested, e := rawRow(t, c.ing, "PANIC2"); !ingested || !strings.Contains(e, "panic") {
		t.Fatalf("raw event: %v %q", ingested, e)
	}
	// Nothing archived (the ingestor itself is broken): no ack, so WhatsApp redelivers.
	c.ing = nil
	if c.handle(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: marcoPN, Sender: marcoPN}, ID: "LOST1", Timestamp: t0},
		Message: textMsg("x")}) {
		t.Fatal("message acked although it was never archived")
	}
	var n int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM raw_events WHERE dedupe_key LIKE '%:LOST1'`).Scan(&n)
	if n != 0 {
		t.Fatal("unexpected archive")
	}
}
