package people

import (
	"context"
	"slices"
	"strings"

	"go.mau.fi/whatsmeow/types"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// Snapshot is what data.db knows of every user JID at one moment: its
// person, that person's stored names, and the recorded LID <-> number links.
// It is read without the write lock, so a large sync can skip the links and
// names that are already stored and write only the rest.
//
// A step judged unchanged is skipped only when no applied step of the same
// sync touched its persons since the snapshot: Link merges and moves
// numbers, and a merge changes names, so those persons are judged by the
// database again (the step runs and does whatever is still needed). A write
// by another writer after the snapshot lands as if the skipped steps ran
// before it and the applied ones after it; the next sync applies what it
// left behind.
type Snapshot struct {
	person  map[string]int64 // normalized JID -> person
	names   map[int64]Names
	links   map[[2]string]bool // {lid, pn} recorded in number_links
	touched map[int64]bool
}

// ReadSnapshot reads the JIDs and names of every person and the links.
func ReadSnapshot(ctx context.Context, e store.Execer) (*Snapshot, error) {
	s := &Snapshot{person: map[string]int64{}, names: map[int64]Names{}, links: map[[2]string]bool{}, touched: map[int64]bool{}}
	rows, err := e.QueryContext(ctx, `SELECT j.jid, p.id, p.full_name, p.first_name, p.business_name, p.push_name
		FROM jids j JOIN persons p ON p.id = j.person_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var jid string
		var id int64
		var n Names
		if err := rows.Scan(&jid, &id, &n.FullName, &n.FirstName, &n.BusinessName, &n.PushName); err != nil {
			return nil, err
		}
		s.person[jid] = id
		s.names[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if rows, err = e.QueryContext(ctx, `SELECT lid, pn FROM number_links`); err != nil {
		return nil, err
	}
	for rows.Next() {
		var k [2]string
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			return nil, err
		}
		s.links[k] = true
	}
	return s, rows.Err()
}

// Unchanged reports whether linking jids (LinkCurrent for a LID and a phone
// number, Resolve for one JID) and then SetNames with n on their person
// would write nothing: every user JID is known and belongs to one person no
// applied step has touched, a LID and number are linked already, and every
// non-empty name in n equals the stored one. Non-user JIDs are ignored as
// Link ignores them. With none left, or with two JIDs Link does not link
// (two numbers, two LIDs), it reports false, so the step runs.
func (s *Snapshot) Unchanged(jids []types.JID, n Names) bool {
	var users []types.JID
	for _, j := range jids {
		if norm, _, ok := Normalize(j); ok && !slices.Contains(users, norm) {
			users = append(users, norm)
		}
	}
	switch len(users) {
	case 1:
	case 2:
		l, pn, ok := numberPair(users[0], users[1])
		if !ok || !s.links[[2]string{l.String(), pn.String()}] {
			return false
		}
	default:
		return false
	}
	var id int64
	for _, j := range users {
		p, known := s.person[j.String()]
		if !known || s.touched[p] || (id != 0 && p != id) {
			return false
		}
		id = p
	}
	have := s.names[id]
	same := func(want, stored string) bool { want = strings.TrimSpace(want); return want == "" || want == stored }
	return same(n.FullName, have.FullName) && same(n.FirstName, have.FirstName) &&
		same(n.BusinessName, have.BusinessName) && same(n.PushName, have.PushName)
}

// Touch records that a step on jids runs: their persons may change, so later
// steps on them are not judged from the snapshot.
func (s *Snapshot) Touch(jids ...types.JID) {
	for _, j := range jids {
		if norm, _, ok := Normalize(j); ok {
			if p, known := s.person[norm.String()]; known {
				s.touched[p] = true
			}
		}
	}
}
