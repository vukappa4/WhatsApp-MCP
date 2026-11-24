// Package autosend holds what the send gate and the daemon share about
// autosend approvals: the receipt the gate writes when it shows the user the
// approval card, and the key that ties it to the tool call's arguments.
//
// The gate writes a receipt only when it asks the user to approve exactly
// these arguments; the daemon stores an approval only by consuming one. A
// call that never passed through the gate (an older gate, a client without
// hooks) finds no receipt and approves nothing.
package autosend

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/vukappa4/whatsapp-mcp/internal/desktop"
	"github.com/vukappa4/whatsapp-mcp/internal/useracl"
)

// Tool names. Both start with whatsapp_send_: every install's PreToolUse
// matcher gates that range.
const (
	ToolAllow  = "whatsapp_send_autosend_allow"
	ToolRevoke = "whatsapp_send_autosend_revoke"
)

// ReceiptTTL is how long a receipt waits for the tool call it was written
// for: the user answers the card, then Claude Code calls the tool at once.
const ReceiptTTL = 2 * time.Minute

// MaxPerDay is the most sends the gate lets one approved task make to one
// chat in 24 hours without asking: a task gone wrong must not flood a chat,
// and WhatsApp bans for bulk sends.
const MaxPerDay = 20

// LogName is the autosend log in a profile's logs folder.
const LogName = "autosend.log"

// ErrNoReceipt means the gate wrote no receipt for these arguments, or it expired.
var ErrNoReceipt = errors.New("no approval receipt")

// ErrReceiptConflict means a fresh receipt for the same arguments says
// something else: two cards for one call cannot both stand.
var ErrReceiptConflict = errors.New("another approval card for the same arguments is waiting with other content")

// Chat is one approved destination: the JID the daemon sends to and the
// label the user saw.
type Chat struct {
	Ref   string `json:"ref"` // the c_N or p_N id approved
	JID   string `json:"jid"`
	Label string `json:"label"`
}

// Receipt is what the approval card showed the user.
type Receipt struct {
	At          int64  `json:"at"` // unix time the gate asked
	Profile     string `json:"profile"`
	TaskID      string `json:"task_id"`
	TaskName    string `json:"task_name"`
	TaskDir     string `json:"task_dir"` // the account/org folder of the task's scheduled-tasks.json
	Schedule    string `json:"schedule"`
	Fingerprint string `json:"fingerprint"`
	Chats       []Chat `json:"chats"`
}

// Args are the arguments of ToolAllow as the gate and the daemon read them.
type Args struct {
	Task  string   `json:"task"`
	Chats []string `json:"chats"`
}

// Normalize trims the chats, drops empty ones and duplicates, and sorts them.
func Normalize(chats []string) []string {
	var out []string
	for _, c := range chats {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ArgsKey ties a receipt to one profile and one set of arguments: the hex
// SHA-256 of the profile, the trimmed task and the normalized chats.
func ArgsKey(profile, task string, chats []string) string {
	b, _ := json.Marshal([]any{profile, strings.TrimSpace(task), Normalize(chats)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ReceiptDir is <hooks dir>/autosend.
func ReceiptDir(hooksDir string) string { return filepath.Join(hooksDir, "autosend") }

var keyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func receiptPath(hooksDir, key string) (string, error) {
	if !keyRe.MatchString(key) {
		return "", fmt.Errorf("bad receipt key %q", key)
	}
	return filepath.Join(ReceiptDir(hooksDir), key+".json"), nil
}

// sameContent reports whether two receipts show the same thing, whenever written.
func sameContent(a, b Receipt) bool {
	a.At, b.At = 0, 0
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// WriteReceipt stores r under key, readable by the user only. It replaces
// an expired receipt or one with the same content, and refuses with
// ErrReceiptConflict a fresh one that shows something else.
func WriteReceipt(hooksDir, key string, r Receipt) error {
	path, err := receiptPath(hooksDir, key)
	if err != nil {
		return err
	}
	if b, err := os.ReadFile(path); err == nil {
		var old Receipt
		if json.Unmarshal(b, &old) == nil && fresh(old.At, time.Unix(r.At, 0)) && !sameContent(old, r) {
			return ErrReceiptConflict
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := useracl.Restrict(dir, true); err != nil {
		return fmt.Errorf("restrict %s to the user: %w", dir, err)
	}
	PruneReceipts(hooksDir, time.Unix(r.At, 0))
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := path + "." + nonce() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := useracl.Restrict(tmp, false); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("restrict %s to the user: %w", tmp, err)
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func nonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fresh reports whether a receipt written at at is still valid at now:
// within ReceiptTTL, and not from the future (a minute of slack).
func fresh(at int64, now time.Time) bool {
	age := now.Sub(time.Unix(at, 0))
	return at != 0 && age >= -time.Minute && age <= ReceiptTTL
}

// takeMu makes TakeReceipt consume each receipt once. Only the daemon takes
// receipts, and each profile's daemon its own keys (ArgsKey holds the
// profile): a lock in the process is enough, where Windows file renames and
// deletes racing on one file can all report success.
var takeMu sync.Mutex

// TakeReceipt consumes the receipt stored under key: only one caller ever
// gets it, and it is gone afterwards. A missing, expired, future or
// unreadable receipt is ErrNoReceipt; a receipt it cannot delete is an error.
func TakeReceipt(hooksDir, key string, now time.Time) (Receipt, error) {
	path, err := receiptPath(hooksDir, key)
	if err != nil {
		return Receipt{}, err
	}
	takeMu.Lock()
	defer takeMu.Unlock()
	PruneReceipts(hooksDir, now)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Receipt{}, ErrNoReceipt
	}
	if err != nil {
		return Receipt{}, err
	}
	if err := os.Remove(path); err != nil {
		return Receipt{}, err
	}
	var r Receipt
	if len(b) > 1<<20 || json.Unmarshal(b, &r) != nil || !fresh(r.At, now) {
		return Receipt{}, ErrNoReceipt
	}
	return r, nil
}

// PruneReceipts deletes receipts that expired or cannot be read, and files
// left behind by an interrupted write.
func PruneReceipts(hooksDir string, now time.Time) {
	dir := ReceiptDir(hooksDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tmp") {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > ReceiptTTL {
				_ = os.Remove(p)
			}
			continue
		}
		var r Receipt
		b, err := os.ReadFile(p)
		if err != nil || len(b) > 1<<20 || json.Unmarshal(b, &r) != nil || !fresh(r.At, now) {
			_ = os.Remove(p)
		}
	}
}

// promptModes are the permission modes in which Claude Code shows a hook's
// "ask" to the user: default, acceptEdits, plan, and auto, where a hook's
// "ask" goes to the prompt and the classifier does not decide it. dontAsk
// denies or lets a mod settle it, bypassPermissions skips prompts, and a
// mode this version does not know is unknown: all of those are refused,
// for an approval and for an autosend.
var promptModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "auto": true}

// PromptMode reports whether mode is one in which the user sees every ask.
func PromptMode(mode string) bool { return promptModes[mode] }

// ModeProblem says why a permission mode rules out an approval or an
// autosend, "" when it does not. who names the holder ("the run"). An
// empty mode is refused.
func ModeProblem(who, mode string) string {
	switch {
	case promptModes[mode]:
		return ""
	case mode == "":
		return who + " has no permission mode"
	}
	return who + "'s permission mode is " + showMode(mode)
}

// TaskModeProblem says why the mode a task's runs use (desktop.EffectiveMode
// and its error) rules out an approval or an autosend, "" when it does not.
// A task without a mode runs in the default mode its settings give it: a
// settings file that cannot be read refuses.
func TaskModeProblem(m desktop.Mode, err error) string {
	switch {
	case err != nil:
		e := []rune(err.Error())
		if len(e) > 300 {
			e = append(e[:300], []rune("...")...)
		}
		return "the task has no permission mode, and the default mode from your Claude Code settings cannot be read (" + string(e) + ")"
	case promptModes[m.Name]:
		return ""
	case !m.FromSettings:
		return ModeProblem("the task", m.Name)
	}
	return "the task has no permission mode, and " + m.Source + " sets permissions.defaultMode to " + showMode(m.Name)
}

// ShowTaskMode is the task's mode as a card shows it: "default", or
// "default (from your settings)" when the task holds none.
func ShowTaskMode(m desktop.Mode) string {
	if m.FromSettings {
		return showMode(m.Name) + " (from your settings)"
	}
	return showMode(m.Name)
}

// showMode is a mode for a message: cut at 40 characters, and quoted when it
// holds more than letters and dots, which escapes what a file may hold
// beyond a plain name.
func showMode(mode string) string {
	if r := []rune(mode); len(r) > 40 {
		mode = string(r[:40]) + "..."
	}
	if strings.IndexFunc(mode, func(r rune) bool { return !unicode.IsLetter(r) && r != '.' }) >= 0 {
		mode = strconv.Quote(mode)
	}
	return mode
}
