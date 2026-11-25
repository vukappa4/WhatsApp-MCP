package autosend

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/desktop"
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
		TaskDir: "org-1", Fingerprint: "abc", Chats: []Chat{{Ref: "p_1", JID: "123@lid", Label: "Marco Rossi (+393331234567)"}}}
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
		want           error
	}{
		"fresh":              {now, now.Add(ReceiptTTL - time.Second), nil},
		"expired":            {now, now.Add(ReceiptTTL + time.Second), ErrReceiptExpired},
		"expired a day ago":  {now, now.Add(ReceiptTTL + 23*time.Hour), ErrReceiptExpired},
		"pruned after a day": {now, now.Add(receiptKeep + time.Second), ErrNoReceipt},
		"future":             {now.Add(2 * time.Minute), now, ErrNoReceipt},
	} {
		if err := WriteReceipt(hooks, key, receipt(tc.written)); err != nil {
			t.Fatal(err)
		}
		if _, err := TakeReceipt(hooks, key, tc.taken); !errors.Is(err, tc.want) || (err == nil) != (tc.want == nil) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	// A receipt is taken once, expired or not.
	if err := WriteReceipt(hooks, key, receipt(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeReceipt(hooks, key, now.Add(ReceiptTTL+time.Minute)); !errors.Is(err, ErrReceiptExpired) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := TakeReceipt(hooks, key, now.Add(ReceiptTTL+time.Minute)); !errors.Is(err, ErrNoReceipt) {
		t.Fatalf("expired, taken again: %v", err)
	}
	// Writing prunes receipts of other calls that expired over a day ago,
	// and corrupt ones.
	other := ArgsKey("prod", "other", nil)
	if err := WriteReceipt(hooks, other, receipt(now)); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(ReceiptDir(hooks), ArgsKey("x", "y", nil)+".json")
	os.WriteFile(corrupt, []byte("{"), 0o600)
	if err := WriteReceipt(hooks, key, receipt(now.Add(receiptKeep+time.Minute))); err != nil {
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

// A fresh receipt is replaced only by one with the same content: a second
// card for the same arguments cannot swap what the first one showed.
func TestReceiptConflict(t *testing.T) {
	hooks := filepath.Join(t.TempDir(), "hooks")
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	key := ArgsKey("prod", "t", []string{"c_1"})
	if err := WriteReceipt(hooks, key, receipt(now)); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceipt(hooks, key, receipt(now.Add(time.Minute))); err != nil {
		t.Fatalf("same content: %v", err)
	}
	other := receipt(now.Add(time.Minute))
	other.Chats[0].JID = "999@lid"
	if err := WriteReceipt(hooks, key, other); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("other content: %v", err)
	}
	if r, err := TakeReceipt(hooks, key, now.Add(time.Minute)); err != nil || r.Chats[0].JID != "123@lid" {
		t.Fatalf("kept receipt %+v %v", r, err)
	}
	// Once the first expires, a new card may replace it.
	WriteReceipt(hooks, key, receipt(now))
	if err := WriteReceipt(hooks, key, func() Receipt { r := other; r.At = now.Add(ReceiptTTL + time.Second).Unix(); return r }()); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

// Only the modes in which the user sees every ask pass; an empty or unknown
// mode is refused, and a mode with odd characters is quoted.
func TestModeProblem(t *testing.T) {
	for mode, want := range map[string]string{
		"default": "", "acceptEdits": "", "plan": "", "auto": "",
		"":                  "the task has no permission mode",
		"bypassPermissions": "the task's permission mode is bypassPermissions",
		"dontAsk":           "the task's permission mode is dontAsk",
		"Default":           "the task's permission mode is Default",
		"auto\n":            `the task's permission mode is "auto\n"`,
	} {
		if got := ModeProblem("the task", mode); got != want || PromptMode(mode) != (want == "") {
			t.Errorf("%q: %q, PromptMode %v", mode, got, PromptMode(mode))
		}
	}
}

func TestTaskModeProblem(t *testing.T) {
	for _, tc := range []struct {
		m    desktop.Mode
		err  error
		want string
		card string
	}{
		{desktop.Mode{Name: "plan"}, nil, "", "plan"},
		{desktop.Mode{Name: "dontAsk"}, nil, "the task's permission mode is dontAsk", "dontAsk"},
		{desktop.Mode{Name: "default", FromSettings: true}, nil, "", "default (from your settings)"},
		{desktop.Mode{Name: "bypassPermissions", FromSettings: true, Source: "/u/settings.json"}, nil,
			"the task has no permission mode, and /u/settings.json sets permissions.defaultMode to bypassPermissions", ""},
		{desktop.Mode{}, errors.New("read /u/settings.json: bad"),
			"the task has no permission mode, and the default mode from your Claude Code settings cannot be read (read /u/settings.json: bad)", ""},
	} {
		if got := TaskModeProblem(tc.m, tc.err); got != tc.want {
			t.Errorf("%+v %v: %q", tc.m, tc.err, got)
		}
		if tc.card != "" && ShowTaskMode(tc.m) != tc.card {
			t.Errorf("%+v shows %q", tc.m, ShowTaskMode(tc.m))
		}
	}
}
