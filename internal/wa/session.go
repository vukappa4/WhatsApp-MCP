package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// SessionIdentities is what a whatsmeow session DB knows about people: the
// account itself, the LID <-> phone map and the address book.
type SessionIdentities struct {
	OwnPN, OwnLID types.JID
	PushName      string
	LIDMap        [][2]types.JID // {lid, pn}
	Contacts      []SessionContact
}

// SessionContact is one row of whatsmeow_contacts.
type SessionContact struct {
	JID                                     types.JID
	FirstName, FullName, PushName, Business string
}

// ReadSession reads identities from a whatsmeow sqlstore database. It only
// runs SELECTs, so it is safe on a read-only connection.
func ReadSession(ctx context.Context, db *sql.DB) (*SessionIdentities, error) {
	s := &SessionIdentities{}
	var jid, lid sql.NullString
	err := db.QueryRowContext(ctx, `SELECT jid, lid, push_name FROM whatsmeow_device LIMIT 1`).Scan(&jid, &lid, &s.PushName)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil // not paired
	}
	if err != nil {
		return nil, fmt.Errorf("read whatsmeow_device: %w", err)
	}
	ownJID := jid.String
	if j, err := types.ParseJID(jid.String); err == nil {
		s.OwnPN = j.ToNonAD()
	}
	if j, err := types.ParseJID(lid.String); err == nil {
		s.OwnLID = j.ToNonAD()
	}

	rows, err := db.QueryContext(ctx, `SELECT lid, pn FROM whatsmeow_lid_map`)
	if err != nil {
		return nil, fmt.Errorf("read whatsmeow_lid_map: %w", err)
	}
	for rows.Next() {
		var l, p string
		if err := rows.Scan(&l, &p); err != nil {
			rows.Close()
			return nil, err
		}
		s.LIDMap = append(s.LIDMap, [2]types.JID{types.NewJID(l, types.HiddenUserServer), types.NewJID(p, types.DefaultUserServer)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `SELECT their_jid, COALESCE(first_name, ''), COALESCE(full_name, ''),
		COALESCE(push_name, ''), COALESCE(business_name, '') FROM whatsmeow_contacts WHERE our_jid = ?`, ownJID)
	if err != nil {
		return nil, fmt.Errorf("read whatsmeow_contacts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var their string
		var c SessionContact
		if err := rows.Scan(&their, &c.FirstName, &c.FullName, &c.PushName, &c.Business); err != nil {
			return nil, err
		}
		j, err := types.ParseJID(their)
		if err != nil || !people.IsUser(j) {
			continue
		}
		c.JID = j
		s.Contacts = append(s.Contacts, c)
	}
	return s, rows.Err()
}

// Identity writes run in short transactions: each commits after
// identityBatchSize steps or once it has run identityBatchTime, whichever
// comes first. Between two batches the sync pauses identityBatchPause:
// SQLite has no queue for its write lock, and a writer waiting for it
// (live messages above all) polls it every 25 ms or less in its first 100 ms
// of waiting, so without the pause the next batch would take the lock again
// before any waiter sees it free. A waiting writer thus waits about one batch.
const (
	identityBatchSize  = 200
	identityBatchTime  = 50 * time.Millisecond
	identityBatchPause = 50 * time.Millisecond
)

// identitySteps collects the writes of an identity sync, in order. Each step
// links JIDs and stores names; it is kept only when the snapshot read before
// says data.db lacks something it writes.
type identitySteps struct {
	snap  *people.Snapshot
	steps []func(tx *sql.Tx) error
}

func (in *Ingestor) newIdentitySteps(ctx context.Context) (*identitySteps, error) {
	snap, err := people.ReadSnapshot(ctx, in.DB)
	if err != nil {
		return nil, fmt.Errorf("read identities: %w", err)
	}
	return &identitySteps{snap: snap}, nil
}

// always adds a step that runs whatever the snapshot says.
func (s *identitySteps) always(step func(tx *sql.Tx) error, jids ...types.JID) {
	s.snap.Touch(jids...)
	s.steps = append(s.steps, step)
}

// add adds a step unless linking jids and storing n would change nothing.
func (s *identitySteps) add(jids []types.JID, n people.Names, step func(tx *sql.Tx) error) {
	if s.snap.Unchanged(jids, n) {
		return
	}
	s.always(step, jids...)
}

// apply runs the steps in order, in short transactions. Every step is
// idempotent: a batch that meets a busy database is retried whole, and a
// sync cut short is completed by the next one.
func (s *identitySteps) apply(ctx context.Context, db *store.DB) error {
	steps := s.steps
	for len(steps) > 0 {
		n := 0
		err := store.Retry(ctx, func() error {
			return db.Tx(ctx, func(tx *sql.Tx) error {
				start := time.Now()
				for n = 0; n < len(steps) && n < identityBatchSize && (n == 0 || time.Since(start) < identityBatchTime); n++ {
					if err := steps[n](tx); err != nil {
						return err
					}
				}
				return nil
			})
		})
		if err != nil {
			return err
		}
		if steps = steps[n:]; len(steps) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(identityBatchPause):
		}
	}
	return nil
}

// ApplySession links every LID to its phone number and stores address book
// names. The session's map has no times: a link it adds or changes counts
// as seen at time at (the sync's; for an archived session, when it was
// archived). It writes only what data.db lacks, in short transactions.
func (in *Ingestor) ApplySession(ctx context.Context, s *SessionIdentities, at time.Time) error {
	steps, err := in.newIdentitySteps(ctx)
	if err != nil {
		return err
	}
	if !s.OwnPN.IsEmpty() || !s.OwnLID.IsEmpty() {
		steps.always(func(tx *sql.Tx) error {
			me, err := people.LinkCurrent(ctx, tx, s.OwnPN, s.OwnLID, at)
			if err != nil {
				return err
			}
			if err := people.MarkMe(ctx, tx, me); err != nil {
				return err
			}
			return people.SetNames(ctx, tx, me, people.Names{PushName: s.PushName})
		}, s.OwnPN, s.OwnLID)
	}
	for _, m := range s.LIDMap {
		steps.add(m[:], people.Names{}, func(tx *sql.Tx) error {
			_, err := people.LinkCurrent(ctx, tx, m[0], m[1], at)
			return err
		})
	}
	for _, c := range s.Contacts {
		names := people.Names{FullName: c.FullName, FirstName: c.FirstName, BusinessName: c.Business, PushName: c.PushName}
		steps.add([]types.JID{c.JID}, names, func(tx *sql.Tx) error {
			pid, err := people.Resolve(ctx, tx, c.JID)
			if err != nil {
				return err
			}
			return people.SetNames(ctx, tx, pid, names)
		})
	}
	if len(steps.steps) > 0 {
		in.log().Info("applying session identities", "changes", len(steps.steps),
			"lid_links", len(s.LIDMap), "contacts", len(s.Contacts))
	}
	return steps.apply(ctx, in.DB)
}
