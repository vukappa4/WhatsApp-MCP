package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

// freshness builds the line that starts every read result. from/to (unix)
// bound the data shown; gaps of the daemon overlapping them are named.
func (s *Server) freshness(ctx context.Context, from, to int64) (string, error) {
	now := s.now()
	st := s.WA.Status()
	lastEvent, err := store.GetStateTS(ctx, s.DB, store.KeyLastEventAt)
	if err != nil {
		return "", err
	}
	var parts []string
	stale := false
	switch st.State {
	case wa.StateConnected:
		parts = append(parts, "connected since "+fmtShort(st.Since, now))
		switch {
		case st.OfflinePending == wa.OfflineUnknown:
			parts = append(parts, "REPLAYING missed messages, newest data "+fmtShort(lastEvent, now))
			stale = true
		case st.OfflinePending > 0:
			parts = append(parts, fmt.Sprintf("CATCHING UP on %d missed events, newest data %s", st.OfflinePending, fmtShort(lastEvent, now)))
			stale = true
		default:
			parts = append(parts, "synced up to "+fmtShort(now, now))
		}
	case wa.StateNotPaired:
		parts = append(parts, "NOT PAIRED - whatsapp_status says how to link")
		parts = append(parts, "data up to "+fmtShort(lastEvent, now))
		stale = true
	case wa.StateConnecting:
		parts = append(parts, "CONNECTING since "+fmtShort(st.Since, now))
		parts = append(parts, "data up to "+fmtShort(lastEvent, now))
		stale = true
	case wa.StateReconnecting:
		parts = append(parts, "RECONNECTING after system sleep since "+fmtShort(st.Since, now))
		parts = append(parts, "data up to "+fmtShort(lastEvent, now))
		stale = true
	default:
		reason := string(st.State)
		if st.Detail != "" && st.State != wa.StateDisconnected {
			reason += ": " + st.Detail
		}
		parts = append(parts, fmt.Sprintf("DISCONNECTED since %s (%s)", fmtShort(st.Since, now), reason))
		parts = append(parts, "data up to "+fmtShort(lastEvent, now))
		stale = true
	}

	if st.StorageError != "" {
		parts = append(parts, "STORAGE ERROR since "+fmtShort(st.StorageErrorAt, now)+": new messages are not being saved (WhatsApp redelivers them once storage works)")
		stale = true
	}

	if seen, err := phoneLastSeen(ctx, s.DB); err != nil {
		return "", err
	} else if !seen.IsZero() && now.Sub(seen) >= phoneWarnAfter {
		parts = append(parts, fmt.Sprintf("PHONE OFFLINE %d days: open WhatsApp on the phone, this device is unlinked at 14", int(now.Sub(seen).Hours()/24)))
	}

	gaps, err := store.Gaps(ctx, s.DB)
	if err != nil {
		return "", err
	}
	if to == 0 {
		to = now.Unix()
	}
	var hits []string
	for _, g := range gaps {
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
	return line + "]", nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
