package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// intentCases is the accuracy table: prompts a user writes to Claude Code,
// and whether each asks to send something.
var intentCases = []struct {
	prompt string
	want   bool
}{
	// Italian: send requests.
	{"Invia a Marco: arrivo alle 8", true},
	{"inviagli il pdf della fattura", true},
	{"Mandagli un messaggio che arrivo tardi", true},
	{"manda a Giulia la foto di ieri", true},
	{"ok invialo", true},
	{"va bene, mandalo pure", true},
	{"Rispondi a Luca che ci sono", true},
	{"rispondigli di sì", true},
	{"RISPONDILE: perfetto, grazie!", true},
	{"scrivi a Marco che il meeting è spostato", true},
	{"scrivigli che lo chiamo dopo", true},
	{"Scrivi un messaggio a Paola per gli auguri", true},
	{"inoltra il messaggio di Anna nel gruppo Calcetto", true},
	{"puoi mandare la presentazione a Enzo?", true},
	{"potresti inviare il file a Sara?", true},
	{"leggi la chat di Marco e poi rispondi a lui", true},
	{"No, mandalo così com'è", true},
	{"digli che arrivo", true},
	{"glielo mandi tu?", true},
	{"Perché non hai risposto? Rispondi a Marco adesso", true},
	// English: send requests.
	{"Send Marco the address", true},
	{"please reply to her that I'm in", true},
	{"forward this to the team group", true},
	{"message him that we're late", true},
	{"text her back: see you at 8", true},
	{"Message Giulia about dinner", true},
	{"can you send the invoice to Paolo?", true},
	{"Write to Anna that the call moved", true},

	// Negatives: reading, questions about received messages, negations, instructions to Claude.
	{"non mandare niente", false},
	{"Non inviare ancora, prima fammi vedere la bozza", false},
	{"leggi i messaggi", false},
	{"cosa mi ha mandato Marco?", false},
	{"Marco mi ha mandato il file?", false},
	{"cosa ha risposto Giulia ieri?", false},
	{"riassumi la chat con Luca", false},
	{"rispondi in italiano", false},
	{"scrivi una funzione che legge il file", false},
	{"prepara una bozza di risposta per Marco", false},
	{"come faccio a mandare un vocale a Marco?", false},
	{"prima di inviare fammi vedere il testo", false},
	{"mandami il riassunto qui", false},
	{"what did Marco send me?", false},
	{"don't send anything yet", false},
	{"do not reply to him", false},
	{"What was his reply?", false},
	{"reply in English please", false},
	{"I'm looking forward to the weekend", false},
	{"show me the unread messages", false},
	{"did you send it?", false},
	{"dobbiamo mandare in produzione la release", false},
	{"Chi mi ha scritto oggi?", false},
	{"no need to reply to Anna", false},
	{"Prepara una bozza da inviare a Marco ma non inviarla", false},
	{"Scrivi un messaggio di benvenuto per la newsletter", false},
	{"Aiutami a rispondere a Giulia", false},
	{"Secondo te cosa dovrei rispondere a Marco?", false},
	{"Ho dimenticato di rispondere a Marco, cosa mi aveva chiesto?", false},
	{"Rispondi alla domanda sopra", false},
	{"Should I reply to Marco?", false},
	{"Draft a message to send to Marco", false},
	{"scrivi il testo per Luca, poi non mandarlo", false},
	{"", false},
}

func TestSendIntentTable(t *testing.T) {
	wrong := 0
	for _, c := range intentCases {
		if got := SendIntent(c.prompt); got != c.want {
			wrong++
			t.Errorf("SendIntent(%q) = %v, want %v", c.prompt, got, c.want)
		}
	}
	pos := 0
	for _, c := range intentCases {
		if c.want {
			pos++
		}
	}
	t.Logf("intent table: %d prompts (%d send, %d not), %d wrong", len(intentCases), pos, len(intentCases)-pos, wrong)
}

// knownLimits pins what the detector gets wrong today, so a change in
// behavior is visible. Each row is a documented limit, not a goal.
var knownLimits = []struct {
	prompt string
	got    bool
	why    string
}{
	{"lo mandi tu a Marco?", false, "second-person question without a clitic: missed, the hook asks"},
	{"fagli sapere che arrivo", false, "periphrasis without a send verb: missed, the hook asks"},
	{"manda la mail a Marco", true, "any channel counts: not limited to WhatsApp"},
	{"cosa devo rispondere a Marco", true, "an advice question without '?' reads as a request"},
	{"non so, manda pure a Marco", true, "correct: the negation belongs to 'so'"},
	{"I want to send a message to Marco", false, "infinitive after 'to' reads as a purpose: missed, the hook asks"},
	{"non rispondere a Luca, rispondi solo a Marco", false, "any negated send verb cancels the prompt: the hook asks"},
	{"leggi questo: please forward this to the team", true, "pasted text is read like the user's own words"},
}

func TestSendIntentKnownLimits(t *testing.T) {
	for _, c := range knownLimits {
		if got := SendIntent(c.prompt); got != c.got {
			t.Errorf("SendIntent(%q) = %v, documented as %v (%s): update the limit or the docs", c.prompt, got, c.got, c.why)
		}
	}
}

func userPrompt(t *testing.T, dir, session, prompt string, now time.Time) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{
		"session_id": session, "transcript_path": "x.jsonl", "cwd": `C:\x`, "permission_mode": "default",
		"hook_event_name": "UserPromptSubmit", "prompt": prompt,
	})
	if err := UserPrompt(bytes.NewReader(in), dir, now); err != nil {
		t.Fatal(err)
	}
}

// gate is a hooks dir plus a WhatsAppMCP root with a seeded prod data.db.
type gate struct {
	t    *testing.T
	dir  string
	root string
	now  time.Time
}

// newGate seeds p_1 Marco Rossi (push name Marchino), p_2 Lorenzo Bianchi,
// chat c_1 (DM with Marco) and c_2 (group Calcetto del Giovedì).
func newGate(t *testing.T) *gate {
	t.Helper()
	base := t.TempDir()
	g := &gate{t: t, dir: filepath.Join(base, "hooks"), root: filepath.Join(base, "WhatsAppMCP"), now: time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)}
	if err := os.MkdirAll(filepath.Join(g.root, "prod"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(g.root, "prod", "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`INSERT INTO persons(id, display_name, name_source, phone_e164, full_name, first_name, push_name, saved_contact, updated_at)
			VALUES (1, 'Marco Rossi', 'full_name', '+393331234567', 'Marco Rossi', 'Marco', 'Marchino', 1, 0),
			       (2, 'Lorenzo Bianchi', 'full_name', '+393339876543', 'Lorenzo Bianchi', 'Lorenzo', '', 1, 0)`,
		`INSERT INTO jids(jid, person_id, kind) VALUES ('393331234567@s.whatsapp.net', 1, 'pn'), ('123456789@lid', 1, 'lid'),
			('393339876543@s.whatsapp.net', 2, 'pn')`,
		`INSERT INTO chats(id, jid, kind, person_id, name, created_at) VALUES (1, '393331234567@s.whatsapp.net', 'dm', 1, '', 0),
			(2, '120363000000000001@g.us', 'group', NULL, 'Calcetto del Giovedì', 0),
			(3, '120363000000000002@g.us', 'group', NULL, 'Offerte Marco', 0)`,
		// The user wrote in Calcetto, never in "Offerte Marco".
		`INSERT INTO messages(chat_id, wa_id, from_me, ts, kind) VALUES (2, 'G1', 1, 0, 'text')`,
		// A stranger who calls themselves "Marco Evil" and never got a message.
		`INSERT INTO persons(id, display_name, name_source, phone_e164, push_name, saved_contact, updated_at)
			VALUES (3, 'Marco Evil', 'push_name', '+393335550000', 'Marco Evil', 0, 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func (g *gate) prompt(session, text string) {
	g.t.Helper()
	userPrompt(g.t, g.dir, session, text, g.now)
}

// pre runs the PreToolUse hook and returns the ask reason, "" when silent.
func (g *gate) pre(session, tool string, args map[string]any) string {
	g.t.Helper()
	in, _ := json.Marshal(map[string]any{
		"session_id": session, "transcript_path": "x.jsonl", "cwd": `C:\x`, "permission_mode": "default",
		"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": args, "tool_use_id": "toolu_01",
	})
	return g.preRaw(in, DBResolver{Root: g.root})
}

func (g *gate) preRaw(in []byte, r Resolver) string {
	g.t.Helper()
	var out bytes.Buffer
	if err := PreTool(bytes.NewReader(in), &out, PreToolEnv{Dir: g.dir, Now: g.now.Add(time.Minute), Resolver: r}); err != nil {
		g.t.Fatal(err)
	}
	if out.Len() == 0 {
		return ""
	}
	var got map[string]map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		g.t.Fatalf("output is not JSON: %q", out.String())
	}
	h := got["hookSpecificOutput"]
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "ask" || h["permissionDecisionReason"] == "" {
		g.t.Fatalf("decision = %v", h)
	}
	return h["permissionDecisionReason"]
}

const (
	send     = "mcp__whatsapp__whatsapp_send_message"
	sendFile = "mcp__whatsapp__whatsapp_send_file"
)

func TestHookFlow(t *testing.T) {
	g := newGate(t)
	to := map[string]any{"to": "p_1", "text": "ciao"}

	// No prompt yet: ask, naming the resolved recipient.
	if r := g.pre("s1", send, to); r != "WhatsApp: your last message did not ask to send anything. Confirm this send to Marco Rossi (+393331234567)." {
		t.Fatalf("reason = %q", r)
	}
	// The user asks to send to Marco: silent, the normal permission flow decides.
	g.prompt("s1", "rispondi a Marco che arrivo")
	if r := g.pre("s1", send, to); r != "" {
		t.Fatalf("flagged session asked: %q", r)
	}
	// Another session is not flagged.
	if r := g.pre("s2", send, to); !strings.Contains(r, "did not ask to send") {
		t.Fatalf("other session: %q", r)
	}
	// The flag expires after two hours.
	g.now = g.now.Add(FlagTTL + time.Minute)
	if r := g.pre("s1", send, to); !strings.Contains(r, "did not ask to send") {
		t.Fatalf("expired flag: %q", r)
	}
	g.now = g.now.Add(-FlagTTL - time.Minute)
	// The next prompt without send intent clears it.
	g.prompt("s1", "leggi i messaggi di oggi")
	if r := g.pre("s1", send, to); !strings.Contains(r, "did not ask to send") {
		t.Fatalf("cleared flag: %q", r)
	}
	// History requests are gated and named.
	if r := g.pre("s1", "mcp__whatsapp__whatsapp_request_history", map[string]any{"chat": "c_2"}); !strings.Contains(r, "Confirm this history request for group Calcetto del Giovedì.") {
		t.Fatalf("history: %q", r)
	}
	// Read tools are never gated, even if the matcher sends them here.
	if r := g.pre("s1", "mcp__whatsapp__whatsapp_read_chat", map[string]any{"chat": "c_1"}); r != "" {
		t.Fatalf("read tool: %q", r)
	}
	// Unreadable input fails closed.
	if r := g.preRaw([]byte("{not json"), DBResolver{Root: g.root}); !strings.Contains(r, "could not read this call") {
		t.Fatalf("bad input: %q", r)
	}
	// A hostile recipient cannot inject control characters or a novel into the prompt.
	r := g.pre("s3", send, map[string]any{"to": "Marco\u202e\n" + strings.Repeat("x", 300)})
	if strings.Contains(r, "\u202e") || strings.Contains(r, "\n") || len(r) > 400 {
		t.Fatalf("reason not cleaned: %q", r)
	}
}

// The flag authorizes only sends to a recipient the prompt names: by any
// name word, the phone's last six digits or the id. Anyone else asks, with
// the recipient named; a nickname asks once.
func TestHookRecipientMustBeNamed(t *testing.T) {
	g := newGate(t)
	for _, tc := range []struct {
		prompt, to string
		silent     bool
	}{
		{"rispondi a Marco che arrivo", "p_1", true},
		{"scrivi a rossi che arrivo", "c_1", true},                              // surname, DM chat id
		{"manda a MARCO il link", "+39 333 123 4567", true},                     // phone resolves to Marco
		{"manda il link al 333 123 4567", "p_1", true},                          // the number in the prompt
		{"manda il link a +39 3331234567", "393331234567@s.whatsapp.net", true}, // legacy JID
		{"invia a p_1 il link", "p_1", true},                                    // the id itself
		{"scrivi al gruppo calcetto che vengo", "c_2", true},                    // group name word
		{"manda a Marco Rossi il link", "Marco Rossi", true},                    // a name the server resolves exactly
		{"rispondi a Marchino che arrivo", "p_1", true},                         // push name counts
		{"rispondi a Luca che arrivo", "p_1", false},                            // someone else
		{"rispondi a Marco che arrivo", "p_2", false},                           // Claude picked the wrong person
		{"manda a Enzo il piano", "p_2", false},                                 // a nickname of Lorenzo: one confirmation
		{"manda al gruppo del calcetto il link", "c_1", false},                  // a group, not Marco
		{"manda del materiale a Paolo", "c_2", false},                           // stopwords never match (Calcetto del Giovedì)
		{"invia a 4567 il link", "p_1", false},                                  // too few digits
		// Names others choose identify no one: a stranger's push name, a group the user never wrote in.
		{"rispondi a Marco che arrivo", "p_3", false},
		{"rispondi a Marco che arrivo", "Marco Evil", false},
		{"rispondi a Marco Evil che arrivo", "Marco Evil", false},
		{"rispondi al 333 555 0000 che arrivo", "p_3", true}, // its number does
		{"manda a Marco le offerte", "c_3", false},
		{"manda le offerte in c_3", "c_3", true}, // its id does
	} {
		g.prompt("s", tc.prompt)
		r := g.pre("s", send, map[string]any{"to": tc.to, "text": "x"})
		if (r == "") != tc.silent {
			t.Errorf("prompt %q, to %q: reason %q, want silent=%v", tc.prompt, tc.to, r, tc.silent)
		}
		if !tc.silent && !strings.Contains(r, "does not name") {
			t.Errorf("prompt %q, to %q: reason %q does not say who", tc.prompt, tc.to, r)
		}
	}
	g.prompt("s", "manda a Enzo il piano")
	if r := g.pre("s", send, map[string]any{"to": "p_2"}); !strings.Contains(r, "does not name Lorenzo Bianchi (+393339876543)") {
		t.Fatalf("nickname reason: %q", r)
	}
	// A recipient the hook cannot look up asks, naming it.
	g.prompt("s", "rispondi a Marco")
	if r := g.pre("s", send, map[string]any{"to": "p_99"}); !strings.Contains(r, "cannot check the recipient p_99") {
		t.Fatalf("unknown person: %q", r)
	}
	// The dev server reads the dev profile, which does not exist here.
	if r := g.pre("s", "mcp__whatsapp-dev__whatsapp_send_message", map[string]any{"to": "p_1"}); !strings.Contains(r, "cannot check") {
		t.Fatalf("dev profile: %q", r)
	}
}

// The hook finds a name's recipients the way the server does (case, accents,
// emoji and punctuation ignored), so a stranger's self-chosen name never
// slips past it, and a name nobody has asks instead of passing.
func TestHookNameMatchesLikeTheServer(t *testing.T) {
	g := newGate(t)
	db, err := store.Open(context.Background(), filepath.Join(g.root, "prod", "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO persons(id, display_name, name_source, phone_e164, full_name, first_name, push_name, saved_contact, updated_at)
			VALUES (4, 'Sara 🌸', 'push_name', '+393335550001', '', '', 'Sara 🌸', 0, 0),
			       (5, 'Rispondi', 'push_name', '+393335550002', '', '', 'Rispondi', 0, 0),
			       (6, 'Élodie Martin', 'full_name', '+393335550003', 'Élodie Martin', 'Élodie', '', 1, 0)`,
		`INSERT INTO chats(id, jid, kind, person_id, name, created_at) VALUES (4, '120363000000000004@g.us', 'group', NULL, 'Partita', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for _, tc := range []struct {
		prompt, to, reason string // reason "" means silent
	}{
		{"manda a Sara che arrivo", "Sara", "does not name ~Sara 🌸 (+393335550001)"},
		{"rispondi a Marco che arrivo", "Rispondi!", "does not name ~Rispondi (+393335550002)"},
		{"manda la partita a tutti", "Partità", "does not name group Partita"},
		{"manda a Elodie il link", "elodie", ""},
		{"manda a Élodie il link", "ELODIE MARTIN", ""},
		{"manda a Del Piero il link", "Del Piero", "cannot check the recipient Del Piero"},
	} {
		g.prompt("s", tc.prompt)
		r := g.pre("s", send, map[string]any{"to": tc.to, "text": "x"})
		if tc.reason == "" && r != "" || !strings.Contains(r, tc.reason) {
			t.Errorf("prompt %q, to %q: reason %q, want %q", tc.prompt, tc.to, r, tc.reason)
		}
	}
}

// A file send also needs the prompt to mention a file: a file word, the
// extension or a word of the file name, never a folder name of the path.
func TestHookFileMustBeMentioned(t *testing.T) {
	g := newGate(t)
	path := filepath.Join(string(filepath.Separator)+"Users", "me", "Documents", "Business_Case_v2.pdf")
	for _, tc := range []struct {
		prompt string
		silent bool
	}{
		{"manda il business case a Marco", true},
		{"manda a Marco il pdf", true},
		{"invia a Marco il documento", true},
		{"manda la foto a Marco", true},
		{"manda a Marco l'allegato", true},
		{"rispondi a Marco che arrivo", false},
		{"manda a Marco quello che ti ho detto", false},
		{"manda a Marco quello che sta in Users", false},
	} {
		g.prompt("s", tc.prompt)
		r := g.pre("s", sendFile, map[string]any{"to": "p_1", "path": path})
		if (r == "") != tc.silent {
			t.Errorf("prompt %q: reason %q, want silent=%v", tc.prompt, r, tc.silent)
		}
		if !tc.silent && !strings.Contains(r, "does not mention a file. Confirm sending Business_Case_v2.pdf in this send to Marco Rossi") {
			t.Errorf("prompt %q: reason %q", tc.prompt, r)
		}
	}
	// Legacy send_file: recipient and media_path.
	g.prompt("s", "manda a 3331234567 il vocale")
	if r := g.pre("s", "mcp__whatsapp__send_audio_message", map[string]any{"recipient": "393331234567", "media_path": `C:\x\memo.ogg`}); r != "" {
		t.Fatalf("legacy audio: %q", r)
	}
}

// slowResolver never answers in time.
type slowResolver struct{}

func (slowResolver) Resolve(ctx context.Context, _, _ string) (Recipient, error) {
	<-ctx.Done()
	time.Sleep(100 * time.Millisecond)
	return Recipient{Names: []string{"Marco"}}, nil
}

// The hook never outlives its budget: a stuck lookup asks.
func TestHookLookupBudget(t *testing.T) {
	g := newGate(t)
	g.prompt("s", "rispondi a Marco")
	in, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": send, "tool_input": map[string]any{"to": "p_1"}})
	start := time.Now()
	r := g.preRaw(in, slowResolver{})
	if !strings.Contains(r, "lookup timed out") || time.Since(start) > PreToolBudget+2*time.Second {
		t.Fatalf("reason %q after %s", r, time.Since(start))
	}
}

// A locked data.db (the daemon mid-write) costs at most the busy timeout.
func TestHookLockedDatabase(t *testing.T) {
	g := newGate(t)
	db, err := store.Open(context.Background(), filepath.Join(g.root, "prod", "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	g.prompt("s", "rispondi a Marco")
	start := time.Now()
	r := g.pre("s", send, map[string]any{"to": "p_1"})
	if took := time.Since(start); took > PreToolBudget+time.Second {
		t.Fatalf("hook took %s", took)
	}
	if r != "" && !strings.Contains(r, "cannot check") {
		t.Fatalf("locked db: %q", r)
	}
}

func TestUserPromptFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	// Session ids that are not plain names are hashed, never used as paths.
	userPrompt(t, dir, `..\..\evil`, "invia a Marco", now)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || strings.Contains(entries[0].Name(), ".") {
		t.Fatalf("flag files: %v", entries)
	}
	// The flag holds the time and the folded prompt.
	userPrompt(t, dir, "s1", "Rispondi a NICOLÒ: arrivo!", now)
	if f, ok := readFlag(filepath.Join(dir, "s1"), now); !ok || f.Prompt != "rispondi a nicolo arrivo" || f.At != now.Unix() {
		t.Fatalf("flag = %+v %v", f, ok)
	}
	// Old flags of other sessions are pruned on the next prompt.
	userPrompt(t, dir, "old", "invia a Marco", now.Add(-3*time.Hour))
	userPrompt(t, dir, "new", "leggi", now)
	if _, err := os.Stat(filepath.Join(dir, "old")); !os.IsNotExist(err) {
		t.Fatalf("expired flag kept: %v", err)
	}
	if err := UserPrompt(strings.NewReader("garbage"), dir, now); err == nil {
		t.Fatal("garbage input accepted")
	}
	// Input that fails to parse still clears the session's flag when its id can be read.
	if err := UserPrompt(strings.NewReader(`{"session_id":"s1","prompt":`), dir, now); err == nil {
		t.Fatal("truncated input accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "s1")); !os.IsNotExist(err) {
		t.Fatalf("flag kept after unreadable input: %v", err)
	}
	// A corrupt flag is no flag.
	_ = os.WriteFile(filepath.Join(dir, "s2"), []byte("1760349600"), 0o600)
	if _, ok := readFlag(filepath.Join(dir, "s2"), now); ok {
		t.Fatal("corrupt flag accepted")
	}
}

// aceRe matches an allow ACE in SDDL and captures its SID token.
var aceRe = regexp.MustCompile(`\(A;[^;]*;[^;]*;[^;]*;[^;]*;([^)]+)\)`)

// The flag dir and files grant access to the user only.
func TestFlagsAreUserOnly(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows ACLs")
	}
	dir := filepath.Join(t.TempDir(), "hooks")
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	userPrompt(t, dir, "s1", "invia a Marco il file", now)
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, filepath.Join(dir, "s1")} {
		sddl, err := fileDACL(p)
		if err != nil {
			t.Fatal(err)
		}
		aces := aceRe.FindAllStringSubmatch(sddl, -1)
		if !strings.HasPrefix(sddl, "D:P") || strings.Count(sddl, "(") != 1 || len(aces) != 1 {
			t.Errorf("%s: DACL %s, want one ACE for %s", p, sddl, sid)
			continue
		}
		// SDDL writes some SIDs as aliases, like LA when the user is the built-in Administrator.
		if got, err := canonicalSID(aces[0][1]); err != nil || got != sid {
			t.Errorf("%s: DACL %s grants %s (%v), want %s", p, sddl, got, err, sid)
		}
	}
}

// The registered matcher and gatedTool agree: both gate exactly the send
// tools and the history request of any server named like whatsapp.
func TestPreToolMatcher(t *testing.T) {
	m := regexp.MustCompile(`^(?:` + PreToolMatcher + `)$`)
	for tool, gated := range map[string]bool{
		"mcp__whatsapp__whatsapp_send_message": true, "mcp__whatsapp__whatsapp_send_file": true,
		"mcp__whatsapp__whatsapp_request_history": true, "mcp__whatsapp__whatsapp_read_chat": false, "mcp__whatsapp__whatsapp_get_media": false,
		"mcp__whatsapp-dev__whatsapp_send_message": true, "mcp__whatsapp__send_message": true, "mcp__whatsapp__send_audio_message": true,
		"mcp__WhatsApp__whatsapp_send_message": true, "mcp__my-WHATSAPP__whatsapp_send_file": true,
		"mcp__whatsapp__list_messages": false, "mcp__claude_ai_Slack__slack_send_message": false, "mcp__claude_ai_Gmail__send_message": false,
	} {
		if m.MatchString(tool) != gated {
			t.Errorf("matcher on %s = %v", tool, !gated)
		}
		if _, _, g := gatedTool(tool); g != gated {
			t.Errorf("gatedTool(%s) = %v", tool, g)
		}
	}
}

// Windows PowerShell 5.1 pipes text with a UTF-8 byte order mark: both hooks
// read through it.
func TestHookInputWithBOM(t *testing.T) {
	g := newGate(t)
	bom := "\xEF\xBB\xBF"
	if err := UserPrompt(strings.NewReader(bom+`{"session_id":"s1","prompt":"rispondi a Marco che arrivo"}`), g.dir, g.now); err != nil {
		t.Fatal(err)
	}
	in := bom + `{"session_id":"s1","tool_name":"mcp__whatsapp__whatsapp_send_message","tool_input":{"to":"Marco"}}`
	if r := g.preRaw([]byte(in), DBResolver{Root: g.root}); r != "" {
		t.Fatalf("flagged session with BOM input: %q", r)
	}
}

// When the UserPromptSubmit hook did not run for the latest prompt (it could
// not start, it timed out), the earlier turn's flag does not authorize
// anything: the session transcript shows a newer prompt with other words.
func TestStaleFlagFromAnEarlierTurn(t *testing.T) {
	g := newGate(t)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	entry := func(at time.Time, content any) string {
		b, _ := json.Marshal(map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339Nano), "isSidechain": false,
			"message": map[string]any{"role": "user", "content": content}})
		return string(b) + "\n"
	}
	write := func(lines ...string) {
		if err := os.WriteFile(transcript, []byte(strings.Join(lines, "")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pre := func() string {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "transcript_path": transcript, "tool_name": send,
			"tool_input": map[string]any{"to": "p_1"}})
		return g.preRaw(in, DBResolver{Root: g.root})
	}
	g.prompt("s", "rispondi a Marco che arrivo")
	toolResult := entry(g.now.Add(30*time.Second), []map[string]any{{"type": "tool_result", "tool_use_id": "x", "content": "ok"}})
	// The flagged prompt is the newest typed one (tool results do not count): silent.
	write(entry(g.now, "rispondi a Marco che arrivo"), toolResult)
	if r := pre(); r != "" {
		t.Fatalf("current flag: %q", r)
	}
	// Not flushed yet: the transcript still ends with an older prompt. Silent.
	write(entry(g.now.Add(-time.Hour), "leggi i messaggi"))
	if r := pre(); r != "" {
		t.Fatalf("transcript behind the flag: %q", r)
	}
	// Shell commands typed with "!" are no prompt.
	write(entry(g.now, "rispondi a Marco che arrivo"), entry(g.now.Add(time.Minute), "<bash-input>ls</bash-input>"))
	if r := pre(); r != "" {
		t.Fatalf("shell command counted as a prompt: %q", r)
	}
	// A newer prompt the hook never saw: ask.
	write(entry(g.now, "rispondi a Marco che arrivo"), toolResult,
		entry(g.now.Add(time.Minute), []map[string]any{{"type": "text", "text": "leggi i messaggi di oggi"}}))
	if r := pre(); !strings.Contains(r, "send gate did not check your last message") {
		t.Fatalf("stale flag: %q", r)
	}
}

// A file is named by words of its own name, whichever separator its path uses.
func TestFileNamedInIgnoresFolders(t *testing.T) {
	for _, p := range []string{`/Users/mario/Desktop/Business_Case.pdf`, `C:\Users\mario\Desktop\Business_Case.pdf`} {
		if FileNamedIn(p, []string{"mario", "desktop", "users"}) {
			t.Errorf("%s: a folder name counted as the file's", p)
		}
		if !FileNamedIn(p, []string{"business"}) || !FileNamedIn(p, []string{"pdf"}) {
			t.Errorf("%s: its own name or extension not seen", p)
		}
	}
}
