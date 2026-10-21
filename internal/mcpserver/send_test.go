package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/media"
	"github.com/vukappa4/whatsapp-mcp/internal/outgoing"
	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/testaudio"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

// A stranger whose WhatsApp profile name equals the query never wins over a
// saved contact whose name starts with it.
func TestSendNameNeverPicksStrangerOverContact(t *testing.T) {
	e := newSendEnv(t)
	ctx := context.Background()
	saved, err := people.Resolve(ctx, e.db, types.NewJID("393334444444", types.DefaultUserServer))
	if err != nil {
		t.Fatal(err)
	}
	if err := people.SetNames(ctx, e.db, saved, people.Names{FullName: "Giulia Verdi"}); err != nil {
		t.Fatal(err)
	}
	stranger, err := people.Resolve(ctx, e.db, types.NewJID("393335550000", types.DefaultUserServer))
	if err != nil {
		t.Fatal(err)
	}
	if err := people.SetNames(ctx, e.db, stranger, people.Names{PushName: "Giulia"}); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "giulia", "text": "ciao"})
	if !isErr || !strings.Contains(text, personID(saved)+" Giulia Verdi") || !strings.Contains(text, personID(stranger)+" ~Giulia") {
		t.Fatalf("stranger vs contact:\n%s", text)
	}
	e.fake.onWA["393334444444"] = true
	if text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "Giulia Verdi", "text": "ciao"}); isErr ||
		!strings.Contains(text, "Sent the message to Giulia Verdi (+39 333 444 4444) in c_") {
		t.Fatalf("full name:\n%s", text)
	}
}

// fakeSender records what the tools would send. It never talks to WhatsApp.
type fakeSender struct {
	mu       sync.Mutex
	sent     []fakeSend
	uploads  []whatsmeow.MediaType
	uploaded [][]byte // the bytes of each upload
	checked  []string
	onWA     map[string]bool // digits -> has an account
	history  []types.MessageInfo
	counts   []int
	sendErr  error
	serverTS time.Time
	ids      int
	block    chan struct{} // when set, SendMessage signals started and waits for it to close
	started  chan struct{}
	answer   map[types.JID]types.JID // the chat the server answers for a destination; the destination itself when absent
}

type fakeSend struct {
	to  types.JID
	msg *waE2E.Message
}

// NewMessageID numbers sends: 3EB0SENT01, 3EB0SENT02...
func (f *fakeSender) NewMessageID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids++
	return fmt.Sprintf("3EB0SENT%02d", f.ids)
}

func (f *fakeSender) SendMessage(_ context.Context, to types.JID, msg *waE2E.Message, id string) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block != nil {
		block := f.block
		f.mu.Unlock()
		f.started <- struct{}{}
		<-block
		f.mu.Lock()
	}
	if f.sendErr != nil {
		return whatsmeow.SendResponse{}, f.sendErr
	}
	f.sent = append(f.sent, fakeSend{to, msg})
	n := len(f.sent)
	chat := to
	if a, ok := f.answer[to]; ok {
		chat = a
	}
	return whatsmeow.SendResponse{ID: id, Timestamp: f.serverTS.Add(time.Duration(n) * time.Second), Chat: chat}, nil
}

func (f *fakeSender) IsOnWhatsApp(_ context.Context, digits string) (types.IsOnWhatsAppResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, digits)
	if !f.onWA[digits] {
		return types.IsOnWhatsAppResponse{}, nil
	}
	j := types.NewJID(digits, types.DefaultUserServer)
	return types.IsOnWhatsAppResponse{Query: "+" + digits, JID: j, PhoneNumber: j, IsIn: true}, nil
}

func (f *fakeSender) Upload(_ context.Context, data []byte, mt whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, mt)
	f.uploaded = append(f.uploaded, bytes.Clone(data))
	sum := sha256.Sum256(data)
	return whatsmeow.UploadResponse{URL: "https://mmg.whatsapp.net/x", DirectPath: "/v/t62/x", MediaKey: []byte{1}, FileSHA256: sum[:],
		FileEncSHA256: []byte{3}, FileLength: uint64(len(data))}, nil
}

func (f *fakeSender) RequestHistory(_ context.Context, anchor types.MessageInfo, count int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history = append(f.history, anchor)
	f.counts = append(f.counts, count)
	return nil
}

func (f *fakeSender) sends() []fakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSend(nil), f.sent...)
}

// status is switchable during a test.
type status struct {
	mu sync.Mutex
	st wa.Status
}

func (s *status) Status() wa.Status { s.mu.Lock(); defer s.mu.Unlock(); return s.st }
func (s *status) set(st wa.State, detail string) {
	s.mu.Lock()
	s.st.State, s.st.Detail = st, detail
	s.mu.Unlock()
}

type sendEnv struct {
	db     *store.DB
	fake   *fakeSender
	status *status
	cs     *mcp.ClientSession
	policy outgoing.Policy
	home   string
	srv    *Server
}

func newSendEnv(t *testing.T) *sendEnv {
	t.Helper()
	return newSendEnvWith(t, nil)
}

// newSendEnvWith runs configure on the environment before it serves.
func newSendEnvWith(t *testing.T, configure func(*sendEnv)) *sendEnv {
	t.Helper()
	db := seed(t)
	home := t.TempDir()
	e := &sendEnv{db: db, fake: &fakeSender{onWA: map[string]bool{}, serverTS: t0.Add(4 * time.Hour)},
		status: &status{st: wa.Status{State: wa.StateConnected, Since: t0, AccountPN: me, AccountLID: meLID}}, home: home,
		policy: outgoing.Policy{Home: home, LocalAppData: filepath.Join(home, "AppData", "Local"), AppData: filepath.Join(home, "AppData", "Roaming")}}
	e.policy.AppRoot = filepath.Join(e.policy.LocalAppData, "WhatsAppMCP")
	s := &Server{DB: db, WA: e.status, Profile: "dev", DataDir: filepath.Join(e.policy.AppRoot, "dev"), Version: "test",
		Now: func() time.Time { return t0.Add(5 * time.Hour) }, Sender: e.fake, Files: &e.policy,
		Ingest: &wa.Ingestor{DB: db, Self: func() (types.JID, types.JID) { return me, meLID }}}
	e.srv = s
	e.policy.DataDir = s.DataDir
	e.policy.Roots = outgoing.DefaultRoots(home, s.DataDir)
	e.policy.Scratch = outgoing.DefaultScratch(filepath.Join(home, "Temp"))
	if configure != nil {
		configure(e)
	}
	hs := httptest.NewServer(s.Handler(token, 8766))
	t.Cleanup(hs.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: hs.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	e.cs = cs
	return e
}

func (e *sendEnv) one(t *testing.T, q string, args ...any) string {
	t.Helper()
	var v string
	if err := e.db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func (e *sendEnv) id(t *testing.T, prefix, q string, args ...any) string {
	return prefix + e.one(t, q, args...)
}

func TestSendToolAnnotations(t *testing.T) {
	e := newSendEnv(t)
	tools, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		switch tl.Name {
		case toolSendMessage, toolSendFile, toolRequestHistory:
		default:
			continue
		}
		a := tl.Annotations
		if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Fatalf("%s annotations = %+v", tl.Name, a)
		}
		raw, _ := json.Marshal(tl)
		if strings.Contains(string(raw), "requiresUserInteraction") {
			t.Fatalf("%s asks for user interaction: %s", tl.Name, raw)
		}
		if tl.Name != toolRequestHistory && !strings.Contains(tl.Description, "never bulk") {
			t.Fatalf("%s description lacks the bulk-send warning", tl.Name)
		}
	}
}

func TestSendMessageRecipients(t *testing.T) {
	e := newSendEnv(t)
	marcoA := e.id(t, "p_", `SELECT person_id FROM jids WHERE jid = '393331111111@s.whatsapp.net'`)
	marcoB := e.id(t, "p_", `SELECT person_id FROM jids WHERE jid = '393332222222@s.whatsapp.net'`)

	// An ambiguous name lists the candidates with their phones and sends nothing.
	text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "marco", "text": "ciao"})
	if !isErr || !strings.Contains(text, marcoA+" ~Marco, +39 333 111 1111") || !strings.Contains(text, marcoB+" ~Marco, +39 333 222 2222") {
		t.Fatalf("ambiguous:\n%s", text)
	}
	// Unknown names and invalid numbers are errors too.
	if text, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": "Zorro", "text": "ciao"}); !isErr || !strings.Contains(text, "no person or group is named exactly") {
		t.Fatalf("unknown name:\n%s", text)
	}
	if text, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 12", "text": "ciao"}); !isErr {
		t.Fatalf("bad number:\n%s", text)
	}
	if n := len(e.fake.sends()); n != 0 {
		t.Fatalf("sent %d messages on errors", n)
	}

	// A phone in national format reaches the known person without a WhatsApp lookup.
	text, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": "333 111 1111", "text": "arrivo alle 8"})
	if isErr {
		t.Fatal(text)
	}
	sent := e.fake.sends()
	if len(sent) != 1 || sent[0].to.String() != "393331111111@s.whatsapp.net" || sent[0].msg.GetConversation() != "arrivo alle 8" {
		t.Fatalf("sent %+v", sent)
	}
	chatA := e.id(t, "c_", `SELECT chat_id FROM messages WHERE wa_id = 'A2'`)
	mid := e.id(t, "m_", `SELECT id FROM messages WHERE wa_id = '3EB0SENT01'`)
	if !strings.Contains(text, "Sent the message to ~Marco (+39 333 111 1111) in "+chatA+" at ") || !strings.HasSuffix(text, "Stored as "+mid+".") {
		t.Fatalf("result:\n%s", text)
	}
	if got := e.one(t, `SELECT from_me || source || text FROM messages WHERE wa_id = '3EB0SENT01'`); got != "1livearrivo alle 8" {
		t.Fatalf("stored %q", got)
	}
	if n := len(e.fake.checked); n != 0 {
		t.Fatalf("IsOnWhatsApp called %d times for a known chat", n)
	}
	// The read tools see it at once.
	if text, _ = call(t, e.cs, toolReadChat, map[string]any{"chat": chatA}); !strings.Contains(text, mid+" me: ") {
		t.Fatalf("read_chat after send:\n%s", text)
	}

	// Groups by exact name, case and accents ignored.
	text, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": "CALCETTO", "text": "chi c'è stasera?"})
	// The group name is chosen by its members: wrapped.
	if isErr || !regexp.MustCompile(`to group <untrusted_wa n=\w+>Calcetto</untrusted_wa n=\w+> in c_\d+ at `).MatchString(text) {
		t.Fatalf("group:\n%s", text)
	}
	if to := e.fake.sends()[1].to; to.Server != types.GroupServer {
		t.Fatalf("group send went to %s", to)
	}

	// A new number: checked on WhatsApp first, refused when it has no account.
	text, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 347 000 0001", "text": "ciao"})
	if !isErr || !strings.Contains(text, "+39 347 000 0001 has no WhatsApp account") || len(e.fake.sends()) != 2 {
		t.Fatalf("no account:\n%s", text)
	}
	e.fake.onWA["393470000002"] = true
	text, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": "0039 347 000 0002", "text": "buongiorno, sono Mario"})
	if isErr || !strings.Contains(text, "first message to this number") {
		t.Fatalf("first contact:\n%s", text)
	}
	if got := e.one(t, `SELECT COUNT(*) FROM chats c JOIN persons p ON p.id = c.person_id WHERE p.phone_e164 = '+393470000002'`); got != "1" {
		t.Fatalf("new DM chats = %s", got)
	}
	// The second message to that number is no longer a first contact.
	if text, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": "+393470000002", "text": "ti scrivo per..."}); isErr ||
		strings.Contains(text, "first message") {
		t.Fatalf("second message flagged as first contact:\n%s", text)
	}
	// c_N and p_N ids work too.
	if _, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": marcoB, "text": "ok"}); isErr {
		t.Fatal("p_N recipient failed")
	}
	if _, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": chatA, "text": "ok"}); isErr {
		t.Fatal("c_N recipient failed")
	}
}

// Only a plain name is matched against group names. A broken id or a JID
// that names no person is refused: the send gate reads those as an id or a
// number, so a group named after them would get a send the gate never checked.
func TestSendGroupNameOnlyForPlainNames(t *testing.T) {
	e := newSendEnv(t)
	if _, err := e.db.Exec(`INSERT INTO chats(jid, kind, name, created_at) VALUES
		('120363000000000091@g.us', 'group', 'P-Rossi', 0), ('120363000000000092@g.us', 'group', '393331111111 x', 0),
		('120363000000000093@g.us', 'group', 'C 7', 0)`); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"p_rossi", "p_ 8", "393331111111@x", "c_ 7"} {
		if text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": to, "text": "ciao"}); !isErr {
			t.Errorf("to %q was sent:\n%s", to, text)
		}
	}
	if n := len(e.fake.sends()); n != 0 {
		t.Fatalf("sent %d messages", n)
	}
	// The plain name still reaches the group.
	if text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "P Rossi", "text": "ciao"}); isErr {
		t.Fatalf("plain group name:\n%s", text)
	}
}

func TestSendReply(t *testing.T) {
	e := newSendEnv(t)
	a2 := e.id(t, "m_", `SELECT id FROM messages WHERE wa_id = 'A2'`)
	chatA := e.id(t, "c_", `SELECT chat_id FROM messages WHERE wa_id = 'A2'`)
	g1 := e.id(t, "m_", `SELECT id FROM messages WHERE wa_id = 'G1'`)

	// A message from another chat cannot be quoted.
	text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": chatA, "text": "sì", "reply_to": g1})
	if !isErr || !strings.Contains(text, "not in the recipient's chat") || len(e.fake.sends()) != 0 {
		t.Fatalf("cross-chat reply:\n%s", text)
	}
	text, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": chatA, "text": "sì, va bene", "reply_to": a2})
	if isErr {
		t.Fatal(text)
	}
	x := e.fake.sends()[0].msg.GetExtendedTextMessage()
	ci := x.GetContextInfo()
	if x.GetText() != "sì, va bene" || ci.GetStanzaID() != "A2" || ci.GetParticipant() != "393331111111@s.whatsapp.net" ||
		ci.GetQuotedMessage().GetConversation() != "versione 2" {
		t.Fatalf("reply context = %v", x)
	}
	if got := e.one(t, `SELECT reply_to_wa_id || '|' || reply_preview FROM messages WHERE wa_id = '3EB0SENT01'`); got != "A2|versione 2" {
		t.Fatalf("stored reply %q", got)
	}
	// Replying to my own message quotes me as the participant.
	mine := e.id(t, "m_", `SELECT id FROM messages WHERE wa_id = '3EB0SENT01'`)
	if _, isErr = call(t, e.cs, toolSendMessage, map[string]any{"to": chatA, "text": "anzi no", "reply_to": mine}); isErr {
		t.Fatal("reply to own message failed")
	}
	if p := e.fake.sends()[1].msg.GetExtendedTextMessage().GetContextInfo().GetParticipant(); p != me.String() {
		t.Fatalf("participant of own message = %s", p)
	}
}

func TestSendIdempotency(t *testing.T) {
	e := newSendEnv(t)
	args := map[string]any{"to": "+39 333 111 1111", "text": "bonifico fatto", "idempotency_key": "pay-42"}
	first, isErr := call(t, e.cs, toolSendMessage, args)
	if isErr || !strings.Contains(first, "Stored as m_") {
		t.Fatal(first)
	}
	// The result is stored and replayed later, so its time keeps the year.
	if !regexp.MustCompile(` at \d{1,2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}( [+-]\d{2}:\d{2})? \(WhatsApp server time\)`).MatchString(first) {
		t.Fatalf("send time lacks the year:\n%s", first)
	}
	again, isErr := call(t, e.cs, toolSendMessage, args)
	if isErr || len(e.fake.sends()) != 1 || !strings.Contains(again, "Not sent again") || !strings.Contains(again, first) {
		t.Fatalf("retry:\n%s", again)
	}
	// The same key for a different message is refused.
	args2 := map[string]any{"to": "+39 333 111 1111", "text": "altro", "idempotency_key": "pay-42"}
	if text, isErr := call(t, e.cs, toolSendMessage, args2); !isErr || !strings.Contains(text, "different send") {
		t.Fatalf("reused key:\n%s", text)
	}
	// After 24 hours the key is forgotten (it survives in the database until then, across restarts).
	old := store.Now
	store.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	defer func() { store.Now = old }()
	if _, isErr := call(t, e.cs, toolSendMessage, args); isErr || len(e.fake.sends()) != 2 {
		t.Fatalf("expired key did not send again: %d sends", len(e.fake.sends()))
	}
}

func TestSendRefusedWhenNotConnected(t *testing.T) {
	e := newSendEnv(t)
	cases := []struct {
		state  wa.State
		detail string
		want   string
	}{
		{wa.StateDisconnected, "", "Not sent: WhatsApp is disconnected since"},
		{wa.StateLoggedOut, "401", "logged this device out (401). Nothing was queued. To link again"},
		{wa.StateNotPaired, "", "not paired. Nothing was queued. To link again"},
		{wa.StateConnecting, "", "Not sent: WhatsApp is connecting"},
	}
	for _, c := range cases {
		e.status.set(c.state, c.detail)
		text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 333 111 1111", "text": "ciao"})
		if !isErr || !strings.Contains(text, c.want) || !strings.Contains(text, "Nothing was queued") {
			t.Fatalf("%s:\n%s", c.state, text)
		}
	}
	if text, isErr := call(t, e.cs, toolRequestHistory, map[string]any{"chat": "Calcetto"}); !isErr || !strings.Contains(text, "Not requested") {
		t.Fatalf("history while connecting:\n%s", text)
	}
	if n := len(e.fake.sends()) + len(e.fake.history); n != 0 {
		t.Fatalf("%d sends while not connected", n)
	}
	// A send that fails at WhatsApp is an error, and nothing is stored.
	e.status.set(wa.StateConnected, "")
	e.fake.sendErr = whatsmeow.ErrMessageTimedOut
	text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 333 111 1111", "text": "ciao"})
	if !isErr || !strings.Contains(text, "may or may not have been delivered") {
		t.Fatalf("timeout:\n%s", text)
	}
	if got := e.one(t, `SELECT COUNT(*) FROM messages WHERE from_me = 1`); got != "0" {
		t.Fatalf("stored %s messages after a failed send", got)
	}
	// An unconfirmed send with an idempotency key is never repeated under that key.
	keyed := map[string]any{"to": "+39 333 111 1111", "text": "ciao", "idempotency_key": "k1"}
	if _, isErr := call(t, e.cs, toolSendMessage, keyed); !isErr {
		t.Fatal("timeout not an error")
	}
	e.fake.sendErr = nil
	text, isErr = call(t, e.cs, toolSendMessage, keyed)
	if !isErr || !strings.Contains(text, "did not confirm that send") || len(e.fake.sends()) != 0 {
		t.Fatalf("retry after timeout:\n%s", text)
	}
	// A cancelled request may have put the message on the wire: same answer.
	e.fake.sendErr = context.Canceled
	if text, _ = call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 333 111 1111", "text": "x"}); !strings.Contains(text, "may or may not") {
		t.Fatalf("cancelled:\n%s", text)
	}
	// A send WhatsApp refused outright frees its key for a retry.
	e.fake.sendErr = errors.New("rejected")
	k2 := map[string]any{"to": "+39 333 111 1111", "text": "riprovo", "idempotency_key": "k2"}
	if text, _ = call(t, e.cs, toolSendMessage, k2); !strings.Contains(text, "Not sent: rejected") {
		t.Fatalf("rejected:\n%s", text)
	}
	e.fake.sendErr = nil
	if text, isErr = call(t, e.cs, toolSendMessage, k2); isErr || len(e.fake.sends()) != 1 {
		t.Fatalf("retry after a refused send:\n%s", text)
	}
}

func TestSendFile(t *testing.T) {
	e := newSendEnv(t)
	dir := filepath.Join(e.home, "Pictures")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	photo := filepath.Join(dir, "scontrino.png")
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 300, 200)))
	if err := os.WriteFile(photo, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, e.cs, toolSendFile, map[string]any{"to": "+39 333 111 1111", "path": photo, "caption": "eccolo"})
	if isErr {
		t.Fatal(text)
	}
	img := e.fake.sends()[0].msg.GetImageMessage()
	if img.GetCaption() != "eccolo" || img.GetMimetype() != "image/png" || img.GetWidth() != 300 || img.GetHeight() != 200 ||
		len(img.GetJPEGThumbnail()) == 0 || img.GetDirectPath() != "/v/t62/x" || e.fake.uploads[0] != whatsmeow.MediaImage {
		t.Fatalf("image message = %v", img)
	}
	// The file name can be a sender's (files from the media folder): wrapped.
	if !regexp.MustCompile(`Sent the photo <untrusted_wa n=(\w+)>scontrino.png</untrusted_wa n=(\w+)> to ~Marco`).MatchString(text) {
		t.Fatalf("result:\n%s", text)
	}
	stored := regexp.MustCompile(`Stored as m_(\d+)\.`).FindStringSubmatch(text)
	if stored == nil {
		t.Fatalf("no stored id:\n%s", text)
	}
	// Without a media manager the row stays pending, never pointing at the original.
	got := e.one(t, `SELECT m.kind || '|' || m.text || '|' || md.local_path || '|' || md.state FROM messages m JOIN media md ON md.id = m.media_id
		WHERE m.id = ?`, stored[1])
	if want := "image|eccolo||pending"; got != want {
		t.Fatalf("stored %q, want %q", got, want)
	}

	// Secrets are refused before anything is uploaded.
	secret := filepath.Join(e.home, ".ssh", "id_ed25519")
	_ = os.MkdirAll(filepath.Dir(secret), 0o700)
	_ = os.WriteFile(secret, []byte("key"), 0o600)
	own := filepath.Join(e.policy.LocalAppData, "WhatsAppMCP", "dev", "session.db")
	_ = os.MkdirAll(filepath.Dir(own), 0o700)
	_ = os.WriteFile(own, []byte("keys"), 0o600)
	outside := filepath.Join(e.home, "projects", "plan.txt")
	_ = os.MkdirAll(filepath.Dir(outside), 0o700)
	_ = os.WriteFile(outside, []byte("plan"), 0o600)
	for _, p := range []string{secret, own, outside} {
		text, isErr := call(t, e.cs, toolSendFile, map[string]any{"to": "+39 333 111 1111", "path": p})
		if !isErr || !strings.Contains(text, "not allowed") || !strings.HasPrefix(text, "Not sent: <untrusted_wa n=") {
			t.Fatalf("%s:\n%s", p, text)
		}
	}
	if text, isErr := call(t, e.cs, toolSendFile, map[string]any{"to": "+39 333 111 1111", "path": photo, "kind": "voice_note", "caption": "x"}); !isErr ||
		!strings.Contains(text, "voice notes have no caption") {
		t.Fatalf("voice caption:\n%s", text)
	}
	if len(e.fake.uploads) != 1 {
		t.Fatalf("uploads = %d", len(e.fake.uploads))
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH: voice note part skipped")
	}
	wav := filepath.Join(e.home, "Music", "memo.wav")
	_ = os.MkdirAll(filepath.Dir(wav), 0o700)
	if err := testaudio.WAV(wav, 44100, 1, 2, testaudio.Tone(300, 0.125)); err != nil {
		t.Fatal(err)
	}
	text, isErr = call(t, e.cs, toolSendFile, map[string]any{"to": "+39 333 111 1111", "path": wav, "kind": "voice_note"})
	if isErr {
		t.Fatal(text)
	}
	au := e.fake.sends()[1].msg.GetAudioMessage()
	if !au.GetPTT() || au.GetSeconds() != 2 || len(au.GetWaveform()) != 64 || au.GetMimetype() != "audio/ogg; codecs=opus" || e.fake.uploads[1] != whatsmeow.MediaAudio {
		t.Fatalf("voice message = %v", au)
	}
	if got := e.one(t, `SELECT m.kind || '|' || md.duration FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = '3EB0SENT02'`); got != "voice|2" {
		t.Fatalf("stored voice %q", got)
	}
}

// A sent file is served from the media folder as the bytes that were
// uploaded (a voice note as its Opus audio): whatsapp_get_media never
// downloads it, works offline, and still finds it after a reingest or when
// its media row is rebuilt.
func TestSentFileServedFromMediaFolder(t *testing.T) {
	src := &mediaSource{files: map[string][]byte{}} // offline, and the CDN has nothing
	root := ""
	e := newSendEnvWith(t, func(e *sendEnv) {
		root = filepath.Join(e.srv.DataDir, "media")
		// The clock is past the sweep's window: only get_media downloads.
		mgr := media.New(media.Options{DB: e.db, Root: root, Source: src, Now: func() time.Time { return t0.Add(30 * 24 * time.Hour) }})
		mgr.Start()
		t.Cleanup(mgr.Stop)
		e.srv.Media = mgr
	})
	ctx := context.Background()
	e.fake.onWA["393331111111"] = true
	docs := filepath.Join(e.home, "Documents")
	if err := os.MkdirAll(docs, 0o700); err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.7 offerta")
	original := filepath.Join(docs, "offerta.pdf")
	if err := os.WriteFile(original, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	send := func(args map[string]any) string {
		t.Helper()
		text, isErr := call(t, e.cs, toolSendFile, args)
		stored := regexp.MustCompile(`Stored as (m_\d+)\.`).FindStringSubmatch(text)
		if isErr || stored == nil {
			t.Fatalf("send:\n%s", text)
		}
		return stored[1]
	}
	served := func(id string) []byte {
		t.Helper()
		text, isErr := call(t, e.cs, toolGetMedia, map[string]any{"message": id, "mode": "path"})
		file := regexp.MustCompile(`File: <untrusted_wa n=\w+>(.+?)</untrusted_wa`).FindStringSubmatch(text)
		if isErr || file == nil {
			t.Fatalf("get_media %s:\n%s", id, text)
		}
		if rel, err := filepath.Rel(root, file[1]); err != nil || !filepath.IsLocal(rel) {
			t.Fatalf("served %s outside the media folder %s", file[1], root)
		}
		b, err := os.ReadFile(file[1])
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	check := func(when string, ids []string, want [][]byte) {
		t.Helper()
		for i, id := range ids {
			if got := served(id); !bytes.Equal(got, want[i]) {
				t.Fatalf("%s: %s served %d bytes, want the %d uploaded", when, id, len(got), len(want[i]))
			}
		}
		src.mu.Lock()
		defer src.mu.Unlock()
		if src.downloads != 0 {
			t.Fatalf("%s: %d downloads", when, src.downloads)
		}
	}

	ids := []string{send(map[string]any{"to": "+39 333 111 1111", "path": original})}
	want := [][]byte{pdf}
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		wav := filepath.Join(e.home, "Music", "memo.wav")
		_ = os.MkdirAll(filepath.Dir(wav), 0o700)
		if err := testaudio.WAV(wav, 44100, 1, 2, testaudio.Tone(300, 0.125)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, send(map[string]any{"to": "+39 333 111 1111", "path": wav, "kind": "voice_note"}))
		opus := e.fake.uploaded[1]
		if !bytes.HasPrefix(opus, []byte("OggS")) {
			t.Fatalf("voice note upload is not Ogg: % x", opus[:min(len(opus), 8)])
		}
		want = append(want, opus)
	} else {
		t.Log("ffmpeg not on PATH: voice note part skipped")
	}
	// The original may change or disappear: the media folder holds its own copy.
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	check("after send", ids, want)

	if _, err := e.srv.Ingest.Reingest(ctx, nil); err != nil {
		t.Fatal(err)
	}
	check("after reingest", ids, want)

	// A media row rebuilt from scratch adopts the file by its SHA-256.
	if _, err := e.db.Exec(`UPDATE media SET state = 'pending', local_path = '', attempts = 0`); err != nil {
		t.Fatal(err)
	}
	check("rebuilt row", ids, want)
}

// A media folder that cannot take the file never fails the send: the result
// is the usual one and the row stays pending, to be downloaded later.
func TestSentFileKeptOrNotTheSendStands(t *testing.T) {
	e := newSendEnvWith(t, func(e *sendEnv) {
		root := filepath.Join(e.home, "not-a-folder")
		if err := os.WriteFile(root, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		mgr := media.New(media.Options{DB: e.db, Root: root, Source: &mediaSource{}, Log: slog.New(slog.DiscardHandler),
			Now: func() time.Time { return t0.Add(30 * 24 * time.Hour) }})
		mgr.Start()
		t.Cleanup(mgr.Stop)
		e.srv.Media = mgr
	})
	doc := filepath.Join(e.home, "Documents", "offerta.pdf")
	_ = os.MkdirAll(filepath.Dir(doc), 0o700)
	if err := os.WriteFile(doc, []byte("%PDF-1.7"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, e.cs, toolSendFile, map[string]any{"to": "+39 333 111 1111", "path": doc})
	if isErr || !strings.Contains(text, "Stored as m_") || len(e.fake.sends()) != 1 {
		t.Fatalf("send:\n%s", text)
	}
	if got := e.one(t, `SELECT md.state || '|' || md.local_path FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = '3EB0SENT01'`); got != "pending|" {
		t.Fatalf("row %q", got)
	}
}

func TestRequestHistoryTool(t *testing.T) {
	e := newSendEnv(t)
	chatA := e.id(t, "c_", `SELECT chat_id FROM messages WHERE wa_id = 'A1'`)
	text, isErr := call(t, e.cs, toolRequestHistory, map[string]any{"chat": chatA})
	if isErr {
		t.Fatal(text)
	}
	if len(e.fake.history) != 1 || e.fake.history[0].ID != "A1" || e.fake.history[0].Chat.String() != "393331111111@s.whatsapp.net" ||
		e.fake.history[0].IsFromMe || e.fake.counts[0] != 50 || !e.fake.history[0].Timestamp.Equal(t0) {
		t.Fatalf("anchor = %+v count %v", e.fake.history, e.fake.counts)
	}
	if !strings.Contains(text, "asynchronous") || !strings.Contains(text, "phone must be online") || !strings.Contains(text, "whatsapp_read_chat chat="+chatA) ||
		!strings.Contains(text, "Asked the phone for up to 50 messages of "+chatA) {
		t.Fatalf("result:\n%s", text)
	}
	if text, isErr = call(t, e.cs, toolRequestHistory, map[string]any{"chat": chatA, "count": 1000}); !isErr {
		t.Fatalf("count 1000 accepted:\n%s", text)
	}
}

// History requests reach the phone: one per chat every 10 minutes, 20 a day.
func TestRequestHistoryRateLimit(t *testing.T) {
	e := newSendEnv(t)
	var mu sync.Mutex
	now := time.Date(2026, 10, 13, 8, 0, 0, 0, rome)
	e.srv.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	chatA := e.id(t, "c_", `SELECT chat_id FROM messages WHERE wa_id = 'A1'`)
	if text, isErr := call(t, e.cs, toolRequestHistory, map[string]any{"chat": chatA}); isErr {
		t.Fatal(text)
	}
	advance(9 * time.Minute)
	if text, isErr := call(t, e.cs, toolRequestHistory, map[string]any{"chat": chatA}); !isErr || !strings.Contains(text, "one request per chat every 10 minutes") {
		t.Fatalf("second request within 10 minutes:\n%s", text)
	}
	advance(2 * time.Minute)
	for i := 2; i <= historyPerDay; i++ {
		if text, isErr := call(t, e.cs, toolRequestHistory, map[string]any{"chat": chatA}); isErr {
			t.Fatalf("request %d: %s", i, text)
		}
		advance(11 * time.Minute)
	}
	if text, isErr := call(t, e.cs, toolRequestHistory, map[string]any{"chat": chatA}); !isErr || !strings.Contains(text, "daily limit") {
		t.Fatalf("request 21:\n%s", text)
	}
	if len(e.fake.history) != historyPerDay {
		t.Fatalf("requests sent = %d", len(e.fake.history))
	}
	advance(16 * time.Hour) // the next day in Rome
	if text, isErr := call(t, e.cs, toolRequestHistory, map[string]any{"chat": chatA}); isErr {
		t.Fatalf("next day: %s", text)
	}
}

// A send WhatsApp did not confirm is resolved as sent once its message is
// stored (it carried a known WhatsApp id): at the next lookup of its key and
// by the startup pass.
func TestUnconfirmedSendResolvedWhenStored(t *testing.T) {
	e := newSendEnv(t)
	ctx := context.Background()
	e.fake.onWA["393331111111"] = true
	e.fake.sendErr = whatsmeow.ErrMessageTimedOut
	keyed := map[string]any{"to": "+39 333 111 1111", "text": "ciao", "idempotency_key": "k1"}
	if text, isErr := call(t, e.cs, toolSendMessage, keyed); !isErr || !strings.Contains(text, "may or may not") {
		t.Fatalf("timeout:\n%s", text)
	}
	e.fake.sendErr = nil
	if text, isErr := call(t, e.cs, toolSendMessage, keyed); !isErr || !strings.Contains(text, "did not confirm that send") {
		t.Fatalf("still unknown:\n%s", text)
	}
	// The message turns up (stored after a crash, or by a history sync) under the id the send carried.
	to := types.NewJID("393331111111", types.DefaultUserServer)
	if _, err := e.srv.Ingest.StoreSent(ctx, to, whatsmeow.SendResponse{ID: "3EB0SENT01", Timestamp: t0.Add(4 * time.Hour), Chat: to},
		&waE2E.Message{Conversation: proto.String("ciao")}); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, e.cs, toolSendMessage, keyed)
	if isErr || !strings.Contains(text, "was delivered") || !strings.Contains(text, "stored later as m_") || len(e.fake.sends()) != 0 {
		t.Fatalf("resolved:\n%s", text)
	}

	// The startup pass resolves records nobody asked about again.
	e.fake.sendErr = whatsmeow.ErrMessageTimedOut
	call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 333 111 1111", "text": "secondo", "idempotency_key": "k2"})
	if _, err := e.srv.Ingest.StoreSent(ctx, to, whatsmeow.SendResponse{ID: "3EB0SENT02", Timestamp: t0.Add(4 * time.Hour), Chat: to},
		&waE2E.Message{Conversation: proto.String("secondo")}); err != nil {
		t.Fatal(err)
	}
	if n, err := ResolveUnconfirmedSends(ctx, e.db); err != nil || n != 1 {
		t.Fatalf("startup pass resolved %d, %v", n, err)
	}
	if v := e.one(t, `SELECT value FROM sync_state WHERE key = 'send_idempotency:k2'`); strings.Contains(v, `"unconfirmed":true`) {
		t.Fatalf("record still unconfirmed: %s", v)
	}
}

// During a temporary ban nothing is sent, whatever the connection state.
func TestSendRefusedDuringTemporaryBan(t *testing.T) {
	e := newSendEnv(t)
	e.fake.onWA["393331111111"] = true
	e.status.mu.Lock()
	e.status.st.BannedUntil, e.status.st.BanReason = t0.Add(6*time.Hour), "sent to too many people"
	e.status.mu.Unlock()
	text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 333 111 1111", "text": "ciao"})
	if !isErr || !strings.Contains(text, "temporarily banned this account (sent to too many people) until") || len(e.fake.sends()) != 0 {
		t.Fatalf("send during a ban:\n%s", text)
	}
	if text, _ := call(t, e.cs, toolStatus, nil); !strings.Contains(text, "Temporary ban: until") {
		t.Fatalf("status during a ban:\n%s", text)
	}
}

// Shutdown waits for the send on the wire and refuses new ones meanwhile.
func TestDrainWaitsForSendInFlight(t *testing.T) {
	e := newSendEnv(t)
	e.fake.onWA["393331111111"] = true
	e.fake.block, e.fake.started = make(chan struct{}), make(chan struct{}, 1)
	first := make(chan string, 1)
	go func() {
		text, _ := call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 333 111 1111", "text": "in volo"})
		first <- text
	}()
	<-e.fake.started
	drained := make(chan bool, 1)
	go func() { drained <- e.srv.Drain(10 * time.Second) }()
	select {
	case <-drained:
		t.Fatal("drained while a send was on the wire")
	case <-time.After(300 * time.Millisecond):
	}
	e.fake.mu.Lock()
	close(e.fake.block)
	e.fake.block = nil
	e.fake.mu.Unlock()
	if ok := <-drained; !ok {
		t.Fatal("drain gave up")
	}
	if text := <-first; !strings.Contains(text, "Sent the message") {
		t.Fatalf("send in flight:\n%s", text)
	}
	if text, isErr := call(t, e.cs, toolSendMessage, map[string]any{"to": "+39 333 111 1111", "text": "dopo"}); !isErr || !strings.Contains(text, "daemon is stopping") {
		t.Fatalf("send during shutdown:\n%s", text)
	}
}
