package wa

import (
	"context"
	"sort"
	"sync"

	"go.mau.fi/whatsmeow/types"
)

// Reasons a message is deliberately not stored.
const (
	dropCiphertext     = "ciphertext"      // history stub: the phone could not decrypt it either
	dropPlaceholder    = "placeholder"     // content WhatsApp hides from linked devices
	dropStub           = "stub"            // other history stubs (encryption notice, settings changes...)
	dropNoContent      = "no_content"      // nothing a person would see (key distribution, album header...)
	dropDuplicateMedia = "duplicate_media" // HD or HEVC copy of a stored media message
)

// drops counts messages not stored during one history blob.
type drops struct {
	mu sync.Mutex
	n  map[string]int
}

type dropsKey struct{}

func newDrops() *drops { return &drops{n: map[string]int{}} }

// withDrops attaches a new counter to ctx; drop() then counts into it.
func withDrops(ctx context.Context) (context.Context, *drops) {
	d := newDrops()
	return context.WithValue(ctx, dropsKey{}, d), d
}

// add merges the counts of o (nil is a no-op).
func (d *drops) add(o *drops) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range o.n {
		d.n[k] += v
	}
}

// summary returns the counts as slog attributes in a stable order.
func (d *drops) summary() []any {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := make([]string, 0, len(d.n))
	for k := range d.n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []any
	for _, k := range keys {
		out = append(out, "dropped_"+k, d.n[k])
	}
	return out
}

// drop logs a message that is not stored at debug level and counts it in
// the history blob being ingested, if any.
func (in *Ingestor) drop(ctx context.Context, reason string, info types.MessageInfo, detail string) {
	if d, ok := ctx.Value(dropsKey{}).(*drops); ok {
		d.mu.Lock()
		d.n[reason]++
		d.mu.Unlock()
	}
	in.log().Debug("message not stored", "reason", reason, "detail", detail, "chat", info.Chat, "id", info.ID)
}
