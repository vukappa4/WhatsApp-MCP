package wa

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/logx"
	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// maxWriteWait is the longest a small competing write may wait behind an
// identity sync.
const maxWriteWait = 500 * time.Millisecond

// longestWriteWait runs fn while another writer commits a small transaction
// every few milliseconds, as live messages do, and returns the longest wait.
func longestWriteWait(t *testing.T, db *store.DB, fn func() error) time.Duration {
	t.Helper()
	ctx := context.Background()
	var longest atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			start := time.Now()
			err := db.Tx(ctx, func(tx *sql.Tx) error { return store.SetState(ctx, tx, "probe", fmt.Sprint(i)) })
			if d := time.Since(start); d > time.Duration(longest.Load()) {
				longest.Store(int64(d))
			}
			if err != nil {
				t.Errorf("competing write: %v", err)
			}
		}
	}()
	err := fn()
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return time.Duration(longest.Load())
}

// pnN and lidN are the phone JID and the LID of test person i.
func pnN(i int) types.JID {
	return types.NewJID(fmt.Sprintf("39340%07d", i), types.DefaultUserServer)
}

func lidN(i int) types.JID {
	return types.NewJID(fmt.Sprintf("2000000%08d", i), types.HiddenUserServer)
}

// largeSession is an account with n LID mappings and n/4 address book
// contacts, as whatsmeow holds it after the first full sync.
func largeSession(n int) *SessionIdentities {
	s := &SessionIdentities{OwnPN: myPN, OwnLID: myLID, PushName: "Me"}
	for i := range n {
		s.LIDMap = append(s.LIDMap, [2]types.JID{lidN(i), pnN(i)})
	}
	for i := 0; i < n; i += 4 {
		s.Contacts = append(s.Contacts, SessionContact{JID: pnN(i), FullName: fmt.Sprintf("Contact %d", i), PushName: "push"})
	}
	return s
}

// The identity sync of a large account must not hold the write lock for
// long: live messages, history and chat state wait behind it.
func TestSessionSyncKeepsWritesFlowing(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	s := largeSession(2000)
	// Persons known from messages under one address only, as before the first sync.
	if err := in.DB.Tx(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 2000; i += 2 {
			if _, err := people.Resolve(ctx, tx, lidN(i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	first := longestWriteWait(t, in.DB, func() error { return in.ApplySession(ctx, s, t0) })
	t.Logf("first sync %s, longest competing write %s", time.Since(start).Round(time.Millisecond), first.Round(time.Millisecond))
	if first > maxWriteWait {
		t.Errorf("first sync held a competing write %s (limit %s)", first, maxWriteWait)
	}
	start = time.Now()
	again := longestWriteWait(t, in.DB, func() error { return in.ApplySession(ctx, s, t0) })
	t.Logf("repeated sync %s, longest competing write %s", time.Since(start).Round(time.Millisecond), again.Round(time.Millisecond))
	if again > maxWriteWait {
		t.Errorf("repeated sync held a competing write %s (limit %s)", again, maxWriteWait)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM persons`); n != 2001 {
		t.Errorf("persons = %d, want 2001 (one per mapping plus me)", n)
	}
	if got := one[string](t, in, `SELECT p.display_name FROM persons p JOIN jids j ON j.person_id = p.id WHERE j.jid = ?`, lidN(8).String()); got != "Contact 8" {
		t.Errorf("display name = %q, want the address book name", got)
	}
}

func TestHistoryIdentitiesKeepWritesFlowing(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	hs := &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum()}
	for i := range 2000 {
		hs.PhoneNumberToLidMappings = append(hs.PhoneNumberToLidMappings,
			&waHistorySync.PhoneNumberToLIDMapping{PnJID: proto.String(pnN(i).String()), LidJID: proto.String(lidN(i).String())})
		hs.Pushnames = append(hs.Pushnames, &waHistorySync.Pushname{ID: proto.String(lidN(i).String()), Pushname: proto.String(fmt.Sprint("Push ", i))})
	}
	start := time.Now()
	wait := longestWriteWait(t, in.DB, func() error { return in.historyBlob(ctx, hs, t0) })
	t.Logf("history identities %s, longest competing write %s", time.Since(start).Round(time.Millisecond), wait.Round(time.Millisecond))
	if wait > maxWriteWait {
		t.Errorf("history identities held a competing write %s (limit %s)", wait, maxWriteWait)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM persons WHERE push_name LIKE 'Push %'`); n != 2000 {
		t.Errorf("persons with push names = %d, want 2000", n)
	}
}

// applySessionOneTx is the identity sync as one transaction applying every
// link and name: the reference the batched sync must match.
func applySessionOneTx(ctx context.Context, in *Ingestor, s *SessionIdentities, at time.Time) error {
	return in.DB.Tx(ctx, func(tx *sql.Tx) error {
		if !s.OwnPN.IsEmpty() || !s.OwnLID.IsEmpty() {
			me, err := people.LinkCurrent(ctx, tx, s.OwnPN, s.OwnLID, at)
			if err != nil {
				return err
			}
			if err := people.MarkMe(ctx, tx, me); err != nil {
				return err
			}
			if err := people.SetNames(ctx, tx, me, people.Names{PushName: s.PushName}); err != nil {
				return err
			}
		}
		for _, m := range s.LIDMap {
			if _, err := people.LinkCurrent(ctx, tx, m[0], m[1], at); err != nil {
				return err
			}
		}
		for _, c := range s.Contacts {
			pid, err := people.Resolve(ctx, tx, c.JID)
			if err != nil {
				return err
			}
			if err := people.SetNames(ctx, tx, pid, people.Names{
				FullName: c.FullName, FirstName: c.FirstName, BusinessName: c.Business, PushName: c.PushName,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// historyIdentitiesOneTx is the reference for historyIdentities.
func historyIdentitiesOneTx(ctx context.Context, in *Ingestor, data *waHistorySync.HistorySync, receivedAt time.Time) error {
	return in.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := in.me(ctx, tx); err != nil {
			return err
		}
		for _, m := range data.GetPhoneNumberToLidMappings() {
			pn, err1 := types.ParseJID(m.GetPnJID())
			lid, err2 := types.ParseJID(m.GetLidJID())
			if err1 != nil || err2 != nil {
				continue
			}
			if _, err := people.LinkCurrent(ctx, tx, lid, pn, receivedAt); err != nil {
				return err
			}
		}
		for _, p := range data.GetPushnames() {
			j, err := types.ParseJID(p.GetID())
			if err != nil || !people.IsUser(j) || p.GetPushname() == "" {
				continue
			}
			if err := in.setNames(ctx, tx, j, people.Names{PushName: p.GetPushname()}); err != nil {
				return err
			}
		}
		return nil
	})
}

// identityWorld draws random but reproducible identity data over a small
// set of JIDs, so links chain, collide and repeat.
type identityWorld struct{ r *rand.Rand }

var worldNames = []string{"", "", "Anna", "Bea", " Carla ", "Dario", "Anna B."}

func (w identityWorld) jid() types.JID { j, _ := w.jidN(); return j }

// jidN draws a phone JID (sometimes on the legacy server) or a LID of
// person i.
func (w identityWorld) jidN() (types.JID, int) {
	i := w.r.Intn(40)
	switch w.r.Intn(9) {
	case 0:
		return types.NewJID(pnN(i).User, types.LegacyUserServer), i
	case 1, 2, 3, 4:
		return pnN(i), i
	}
	return lidN(i), i
}

func (w identityWorld) name() string { return worldNames[w.r.Intn(len(worldNames))] }

// seed stores persons, links, names, DM chats and messages, as ingested
// messages leave them before a sync.
func (w identityWorld) seed(t *testing.T, in *Ingestor) {
	t.Helper()
	ctx := context.Background()
	err := in.DB.Tx(ctx, func(tx *sql.Tx) error {
		for i := range 60 {
			a := w.jid()
			pid, err := people.Resolve(ctx, tx, a)
			if err != nil {
				return err
			}
			if w.r.Intn(3) == 0 {
				if pid, err = people.Link(ctx, tx, a, w.jid(), t0.Add(time.Duration(w.r.Intn(600))*time.Minute)); err != nil {
					return err
				}
			}
			if err := people.SetNames(ctx, tx, pid, people.Names{FullName: w.name(), FirstName: w.name(), PushName: w.name()}); err != nil {
				return err
			}
			if w.r.Intn(2) == 0 {
				chat, err := people.EnsureDMChat(ctx, tx, pid, a)
				if err != nil {
					return err
				}
				for k := range 3 {
					if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO messages(chat_id, wa_id, sender_person_id, ts, kind, text)
						VALUES (?, ?, ?, ?, 'text', ?)`, chat, fmt.Sprintf("W%d", w.r.Intn(80)), pid, i*10+k, w.name()); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (w identityWorld) session() *SessionIdentities {
	s := &SessionIdentities{OwnPN: myPN, OwnLID: myLID, PushName: w.name()}
	// Most of it is consistent (one LID per number, one name per person), so
	// a repeated sync finds most of it stored; the rest collides.
	for range 50 {
		i := w.r.Intn(40)
		if w.r.Intn(5) != 0 {
			s.LIDMap = append(s.LIDMap, [2]types.JID{lidN(i), pnN(i)})
		} else {
			s.LIDMap = append(s.LIDMap, [2]types.JID{lidN(i), pnN(w.r.Intn(40))})
		}
	}
	for range 40 {
		j, i := w.jidN()
		if w.r.Intn(3) != 0 {
			s.Contacts = append(s.Contacts, SessionContact{JID: j, FullName: fmt.Sprint("Person ", i), PushName: fmt.Sprint("P", i)})
		} else {
			s.Contacts = append(s.Contacts, SessionContact{JID: j, FullName: w.name(), FirstName: w.name(), PushName: w.name(), Business: w.name()})
		}
	}
	return s
}

// change returns s with a few mappings and contacts redrawn.
func (w identityWorld) change(s *SessionIdentities) *SessionIdentities {
	c := *s
	c.LIDMap = append([][2]types.JID(nil), s.LIDMap...)
	c.Contacts = append([]SessionContact(nil), s.Contacts...)
	for range 5 {
		c.LIDMap[w.r.Intn(len(c.LIDMap))] = [2]types.JID{lidN(w.r.Intn(40)), pnN(w.r.Intn(40))}
		c.Contacts[w.r.Intn(len(c.Contacts))].FullName = w.name()
	}
	return &c
}

func (w identityWorld) history() *waHistorySync.HistorySync {
	hs := &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum()}
	for range 30 {
		i, pn := w.r.Intn(40), w.r.Intn(40)
		if w.r.Intn(5) != 0 {
			pn = i
		}
		hs.PhoneNumberToLidMappings = append(hs.PhoneNumberToLidMappings, &waHistorySync.PhoneNumberToLIDMapping{
			PnJID: proto.String(pnN(pn).String()), LidJID: proto.String(lidN(i).String())})
	}
	for range 30 {
		j, i := w.jidN()
		name := fmt.Sprint("P", i)
		if w.r.Intn(3) == 0 {
			name = w.name()
		}
		hs.Pushnames = append(hs.Pushnames, &waHistorySync.Pushname{ID: proto.String(j.String()), Pushname: proto.String(name)})
	}
	return hs
}

// lidStore draws the LID store answers.
func (w identityWorld) lidStore() fakeLIDs {
	f := fakeLIDs{}
	for i := range 40 {
		if w.r.Intn(2) == 0 {
			f[lidN(i)] = pnN(w.r.Intn(40))
		}
	}
	return f
}

// identityDump renders every row the identity sync can change, except
// updated_at.
func identityDump(t *testing.T, in *Ingestor) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, display_name, name_source, COALESCE(phone_e164, ''), is_me, saved_contact, full_name, first_name, business_name, push_name FROM persons ORDER BY id`,
		`SELECT jid, person_id, kind FROM jids ORDER BY jid`,
		`SELECT pn, lid, seen_at FROM number_links ORDER BY pn, lid`,
		`SELECT jid, other_jid, note, ts FROM identity_notes ORDER BY jid, other_jid, note`,
		`SELECT id, jid, kind, COALESCE(person_id, 0) FROM chats ORDER BY id`,
		`SELECT id, chat_id, wa_id, COALESCE(sender_person_id, 0), text FROM messages ORDER BY id`,
	} {
		rows, err := in.DB.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

// The batched sync writes only what is missing, yet ends with exactly the
// persons, links, names and chats of applying everything in one transaction,
// on a first sync and on a later one.
func TestIdentitySyncMatchesOneTransaction(t *testing.T) {
	ctx := context.Background()
	for seed := range int64(20) {
		run := func(batched bool) string {
			w := identityWorld{rand.New(rand.NewSource(seed))}
			in := newIngestor(t)
			in.Log = logx.New(io.Discard, false)
			in.LIDs = w.lidStore()
			in.Now = func() time.Time { return t0.Add(20 * time.Hour) } // the LID store's answers count as seen now
			w.seed(t, in)
			s, hs := w.session(), w.history()
			for round := range 3 {
				switch round {
				case 1: // the same again: nothing to write
				case 2: // a few changes
					s = w.change(s)
					hs = w.history()
				}
				var err error
				at := t0.Add(time.Duration(round+10) * time.Hour)
				if batched {
					err = in.ApplySession(ctx, s, at)
				} else {
					err = applySessionOneTx(ctx, in, s, at)
				}
				if err != nil {
					t.Fatalf("seed %d round %d session: %v", seed, round, err)
				}
				if batched {
					err = in.historyIdentities(ctx, hs, at.Add(time.Minute))
				} else {
					err = historyIdentitiesOneTx(ctx, in, hs, at.Add(time.Minute))
				}
				if err != nil {
					t.Fatalf("seed %d round %d history: %v", seed, round, err)
				}
			}
			return identityDump(t, in)
		}
		if want, got := run(false), run(true); got != want {
			t.Fatalf("seed %d: batched sync differs from one transaction\nwant:\n%s\ngot:\n%s", seed, want, got)
		}
	}
}

// A trigger that arrives during an identity sync runs it once more after;
// more triggers meanwhile still add only one run.
func TestIdentitySyncTriggerDuringRunRunsOnceMore(t *testing.T) {
	c, _ := newTestClient(t, t.TempDir())
	var runs atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{}, 4)
	t.Cleanup(func() { close(release) }) // before the client stops: no run stays blocked
	c.syncIdentities = func(context.Context) error {
		runs.Add(1)
		started <- struct{}{}
		<-release
		return nil
	}
	nextRun := func(wait time.Duration) bool {
		select {
		case <-started:
			return true
		case <-time.After(wait):
			return false
		}
	}
	c.goSyncIdentities()
	if !nextRun(5 * time.Second) {
		t.Fatal("first trigger started no run")
	}
	c.goSyncIdentities()
	c.goSyncIdentities()
	release <- struct{}{}
	if !nextRun(5 * time.Second) {
		t.Fatal("triggers during the run started no run after it")
	}
	release <- struct{}{}
	if nextRun(200 * time.Millisecond) {
		t.Fatal("two triggers during one run queued more than one run")
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
	// Idle again: the next trigger starts a run at once.
	c.goSyncIdentities()
	if !nextRun(5 * time.Second) {
		t.Fatal("trigger after the runs started no run")
	}
	release <- struct{}{}
}
