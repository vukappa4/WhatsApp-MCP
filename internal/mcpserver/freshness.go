package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

// freshData is the database state behind the freshness line.
type freshData struct {
	store.FreshState
	Gaps []store.Gap // shared with the cache: never modify
}

// phoneSeen is the newest sign of the phone being online: activity on it
// (phone_seen_at) or the last history sync blob, which only the phone sends.
func (d freshData) phoneSeen() time.Time {
	if d.HistorySyncAt.After(d.PhoneSeenAt) {
		return d.HistorySyncAt
	}
	return d.PhoneSeenAt
}

// gapCache keeps the offline gaps of the last connection_log version seen,
// so a read derives them again only after a new connection event.
type gapCache struct {
	mu   sync.Mutex
	ok   bool
	log  store.LogVersion
	gaps []store.Gap
}

// loadFresh reads the freshness state: one query while connection_log is
// unchanged, two after it changed.
func (s *Server) loadFresh(ctx context.Context) (freshData, error) {
	f, err := store.GetFreshState(ctx, s.DB)
	if err != nil {
		return freshData{}, err
	}
	c := &s.gapCache
	c.mu.Lock()
	hit, gaps := c.ok && c.log == f.Log, c.gaps
	c.mu.Unlock()
	if !hit {
		// Gaps reads connection_log after f.Log was read, so it holds at
		// least the rows of f.Log: a newer row changes Log and is read on
		// the next miss.
		if gaps, err = store.Gaps(ctx, s.DB); err != nil {
			return freshData{}, err
		}
		c.mu.Lock()
		c.ok, c.log, c.gaps = true, f.Log, gaps
		c.mu.Unlock()
	}
	return freshData{FreshState: f, Gaps: gaps}, nil
}

// freshness builds the line that starts every read result. from/to (unix)
// bound the data shown; gaps of the daemon overlapping them are named.
func (s *Server) freshness(ctx context.Context, from, to int64) (string, error) {
	d, err := s.loadFresh(ctx)
	if err != nil {
		return "", err
	}
	return s.freshLine(d, from, to), nil
}

// freshLine renders the freshness line from d.
func (s *Server) freshLine(d freshData, from, to int64) string {
	now := s.now()
	st := s.WA.Status()
	lastEvent := d.LastEventAt
	// The newest time carries its year: other times omit now's year.
	dated := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return fmtTS(t.Unix(), time.Time{})
	}
	var parts []string
	stale := false
	switch st.State {
	case wa.StateConnected:
		parts = append(parts, "connected since "+fmtShort(st.Since, now))
		switch {
		case st.OfflinePending == wa.OfflineUnknown:
			parts = append(parts, "REPLAYING missed messages, newest data "+dated(lastEvent))
			stale = true
		case st.OfflinePending > 0:
			parts = append(parts, fmt.Sprintf("CATCHING UP on %d missed events, newest data %s", st.OfflinePending, dated(lastEvent)))
			stale = true
		default:
			parts = append(parts, "synced up to "+dated(now))
		}
	case wa.StateNotPaired:
		parts = append(parts, "NOT PAIRED - whatsapp_status says how to link")
		parts = append(parts, "data up to "+dated(lastEvent))
		stale = true
	case wa.StateConnecting:
		parts = append(parts, "CONNECTING since "+fmtShort(st.Since, now))
		parts = append(parts, "data up to "+dated(lastEvent))
		stale = true
	case wa.StateReconnecting:
		parts = append(parts, "RECONNECTING after system sleep since "+fmtShort(st.Since, now))
		parts = append(parts, "data up to "+dated(lastEvent))
		stale = true
	default:
		reason := string(st.State)
		if st.Detail != "" && st.State != wa.StateDisconnected {
			reason += ": " + st.Detail
		}
		parts = append(parts, fmt.Sprintf("DISCONNECTED since %s (%s)", fmtShort(st.Since, now), reason))
		parts = append(parts, "data up to "+dated(lastEvent))
		stale = true
	}

	if st.StorageError != "" {
		parts = append(parts, "STORAGE ERROR since "+fmtShort(st.StorageErrorAt, now)+": new messages are not being saved (WhatsApp redelivers them once storage works)")
		stale = true
	}

	if seen := d.phoneSeen(); !seen.IsZero() && now.Sub(seen) >= phoneWarnAfter {
		parts = append(parts, fmt.Sprintf("PHONE OFFLINE %d days: open WhatsApp on the phone, this device is unlinked at 14", int(now.Sub(seen).Hours()/24)))
	}

	if to == 0 {
		to = now.Unix()
	}
	var hits []string
	for _, g := range d.Gaps {
		gto := g.To
		if gto.IsZero() {
			gto = now
		}
		if g.From.Unix() <= to && gto.Unix() >= from {
			end := "now"
			if !g.To.IsZero() {
				end = fmtShort(g.To, now)
			}
			hits = append(hits, fmt.Sprintf("gap %s - %s overlaps this range", fmtShort(g.From, now), end))
		}
	}
	if len(hits) > 3 {
		hits = append(hits[len(hits)-3:], fmt.Sprintf("%d older gaps", len(hits)-3))
	}
	parts = append(parts, hits...)
	line := "[whatsapp: " + strings.Join(parts, " | ")
	if stale {
		line += " - results may be stale"
	}
	return line + "]"
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
