package autosend

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/useracl"
)

func TestArgsKeyNormalizes(t *testing.T) {
	k := ArgsKey("prod", "follow-up", []string{"c_5", "p_7"})
	for _, same := range [][]string{{"p_7", "c_5"}, {" c_5 ", "p_7", "c_5", ""}} {
		if got := ArgsKey("prod", " follow-up\n", same); got != k {
			t.Errorf("chats %q: key differs", same)
		}
	}
	for name, other := range map[string]string{
		"profile": ArgsKey("dev", "follow-up", []string{"c_5", "p_7"}),
		"task":    ArgsKey("prod", "Follow-up", []string{"c_5", "p_7"}),
		"chats":   ArgsKey("prod", "follow-up", []string{"c_5"}),
		"joined":  ArgsKey("prod", "follow-up", []string{"c_5p_7"}),
	} {
		if other == k {
			t.Errorf("%s: same key", name)
		}
	}
	if len(k) != 64 {
		t.Errorf("key %q", k)
	}
}

func receipt(at time.Time) Receipt {
	return Receipt{At: at.Unix(), Profile: "prod", TaskID: "follow-up", TaskName: "Follow up", Schedule: "cron 0 9 * * *",
		Fingerprint: "abc", Chats: []Chat{{JID: "123@lid", Label: "Marco Rossi (+393331234567)"}}}
}

func TestReceiptRoundTripAndConsumeOnce(t *testing.T) {
	hooks := filepath.Join(t.TempDir(), "hooks")
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	key := ArgsKey("prod", "follow-up", []string{"p_1"})
	if err := WriteReceipt(hooks, key, receipt(now)); err != nil {
		t.Fatal(err)
	}
	// Many takers at once: exactly one gets it.
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := TakeReceipt(hooks, key, now.Add(time.Minute))
			if err == nil {
				mu.Lock()
				got++
				mu.Unlock()
				if r.TaskID != "follow-up" || len(r.Chats) != 1 || r.Chats[0].JID != "123@lid" || r.Profile != "prod" {
					t.Errorf("receipt %+v", r)
				}
			} else if !errors.Is(err, ErrNoReceipt) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got != 1 {
		t.Fatalf("%d takers got the receipt", got)
	}
	if entries, _ := os.ReadDir(ReceiptDir(hooks)); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if _, err := TakeReceipt(hooks, "../../etc", now); err == nil || errors.Is(err, ErrNoReceipt) {
		t.Fatalf("bad key: %v", err)
	}
}

func TestReceiptExpiry(t *testing.T) {
	hooks := filepath.Join(t.TempDir(), "hooks")
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	key := ArgsKey("prod", "t", []string{"c_1"})
	for name, tc := range map[string]struct {
		written, taken time.Time
		ok             bool
	}{
		"fresh":   {now, now.Add(ReceiptTTL - time.Second), true},
		"expired": {now, now.Add(ReceiptTTL + time.Second), false},
		"future":  {now.Add(2 * time.Minute), now, false},
	} {
		if err := WriteReceipt(hooks, key, receipt(tc.written)); err != nil {
			t.Fatal(err)
		}
		_, err := TakeReceipt(hooks, key, tc.taken)
		if (err == nil) != tc.ok || err != nil && !errors.Is(err, ErrNoReceipt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Writing prunes expired and corrupt receipts of other calls.
	other := ArgsKey("prod", "other", nil)
	if err := WriteReceipt(hooks, other, receipt(now)); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(ReceiptDir(hooks), ArgsKey("x", "y", nil)+".json")
	os.WriteFile(corrupt, []byte("{"), 0o600)
	if err := WriteReceipt(hooks, key, receipt(now.Add(ReceiptTTL+time.Minute))); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(ReceiptDir(hooks))
	if len(entries) != 1 || entries[0].Name() != key+".json" {
		t.Fatalf("after prune: %v", entries)
	}
}

// The receipt folder and files grant access to the user only.
func TestReceiptsAreUserOnly(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows ACLs")
	}
	hooks := filepath.Join(t.TempDir(), "hooks")
	key := ArgsKey("prod", "t", []string{"c_1"})
	if err := WriteReceipt(hooks, key, receipt(time.Now())); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ReceiptDir(hooks), filepath.Join(ReceiptDir(hooks), key+".json")} {
		if err := useracl.Check(p); err != nil {
			t.Error(err)
		}
	}
}
