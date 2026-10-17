package wa

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/logx"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// TestRealDataReingest rebuilds a copy of a real data.db (WHATSAPP_MCP_E2E_DB,
// opened read-only and copied with VACUUM INTO) and checks that downloads and
// transcripts survive. It logs counts only, never contents. Without the
// profile's session DB the LID map is missing, so message counts may differ.
func TestRealDataReingest(t *testing.T) {
	src := os.Getenv("WHATSAPP_MCP_E2E_DB")
	if src == "" {
		t.Skip("set WHATSAPP_MCP_E2E_DB to a data.db to run")
	}
	ctx := context.Background()
	dst := filepath.Join(t.TempDir(), "data.db")
	ro, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(src)}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		t.Fatal(err)
	}
	ro.Close()
	start := time.Now()
	db, err := store.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Logf("migrations: %s", time.Since(start).Round(time.Millisecond))
	counts := func() map[string]int {
		out := map[string]int{}
		for k, q := range map[string]string{
			"messages":    `SELECT COUNT(*) FROM messages`,
			"media":       `SELECT COUNT(*) FROM media`,
			"downloaded":  `SELECT COUNT(*) FROM media md JOIN messages m ON m.media_id = md.id WHERE md.state = 'downloaded'`,
			"transcripts": `SELECT COUNT(*) FROM media md JOIN messages m ON m.media_id = md.id WHERE md.transcript IS NOT NULL`,
			"pending":     `SELECT COUNT(*) FROM pending_mutations`,
			"raw_errors":  `SELECT COUNT(*) FROM raw_events WHERE error != '' AND kind != 'history_notification'`,
		} {
			var n int
			if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[k] = n
		}
		return out
	}
	before := counts()
	in := &Ingestor{DB: db, Log: logx.New(os.Stderr, false)}
	start = time.Now()
	n, err := in.Reingest(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := counts()
	t.Logf("reingest of %d raw events: %s", n, time.Since(start).Round(time.Millisecond))
	t.Logf("before %v", before)
	t.Logf("after  %v", after)
	if after["downloaded"] < before["downloaded"] || after["transcripts"] < before["transcripts"] {
		t.Fatalf("reingest lost downloads or transcripts: before %v after %v", before, after)
	}
	if ok, _ := store.ReingestInterrupted(ctx, db); ok {
		t.Fatal("reingest marker left set")
	}
}
