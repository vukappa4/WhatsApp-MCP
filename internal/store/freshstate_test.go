package store

import (
	"context"
	"testing"
	"time"
)

// GetFreshState reads the same timestamps as GetStateTS and a log version
// that changes with every new connection_log row.
func TestGetFreshState(t *testing.T) {
	db, _ := open(t)
	ctx := context.Background()
	f, err := GetFreshState(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if f != (FreshState{}) {
		t.Fatalf("empty database: %+v", f)
	}
	SetState(ctx, db, KeyLastEventAt, "1000000")
	SetState(ctx, db, KeyPhoneSeenAt, "not a number")
	SetState(ctx, db, KeyHistorySyncAt, "0")
	LogConnection(ctx, db, time.Unix(10, 0), EvConnected, "")
	f, err = GetFreshState(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct {
		key string
		got time.Time
	}{{KeyLastEventAt, f.LastEventAt}, {KeyPhoneSeenAt, f.PhoneSeenAt}, {KeyHistorySyncAt, f.HistorySyncAt}} {
		want, err := GetStateTS(ctx, db, k.key)
		if err != nil {
			t.Fatal(err)
		}
		if !k.got.Equal(want) {
			t.Fatalf("%s = %v, GetStateTS = %v", k.key, k.got, want)
		}
	}
	if !f.LastEventAt.Equal(time.Unix(1000000, 0)) || !f.PhoneSeenAt.IsZero() || !f.HistorySyncAt.IsZero() {
		t.Fatalf("timestamps: %+v", f)
	}
	if f.Log.Rows != 1 || f.Log.MaxID == 0 {
		t.Fatalf("log version after one row: %+v", f.Log)
	}
	// An older ts still changes the version: Gaps orders by ts, not id.
	LogConnection(ctx, db, time.Unix(5, 0), EvDisconnected, "")
	g, err := GetFreshState(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if g.Log == f.Log || g.Log.Rows != 2 {
		t.Fatalf("log version unchanged after a row: %+v -> %+v", f.Log, g.Log)
	}
}
