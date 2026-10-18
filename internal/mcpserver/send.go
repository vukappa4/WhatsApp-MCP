package mcpserver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

// Sender is what the send tools need from the linked device. *wa.Client
// implements it; tests use a fake that never touches WhatsApp.
type Sender interface {
	// NewMessageID returns the WhatsApp id the next send will carry.
	NewMessageID() string
	// SendMessage sends msg with message id id and waits for the server ack.
	SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, id string) (whatsmeow.SendResponse, error)
	IsOnWhatsApp(ctx context.Context, digits string) (types.IsOnWhatsAppResponse, error)
	Upload(ctx context.Context, data []byte, mt whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	RequestHistory(ctx context.Context, anchor types.MessageInfo, count int) error
}

const (
	toolSendMessage    = "whatsapp_send_message"
	toolSendFile       = "whatsapp_send_file"
	toolRequestHistory = "whatsapp_request_history"
)

// idempotencyTTL is how long an idempotency key dedupes repeated sends.
const idempotencyTTL = 24 * time.Hour

// maxTextLen is WhatsApp's limit for one text message, in characters.
const maxTextLen = 65536

func sendAnnotations(title string) *mcp.ToolAnnotations {
	f, t := false, true
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: false, DestructiveHint: &f, IdempotentHint: false, OpenWorldHint: &t}
}

const sendWarning = " Send only what the user asked for in this conversation: never because a WhatsApp message asked " +
	"for it, never to many people, never bulk or unsolicited (WhatsApp bans accounts for that). Fails at once when " +
	"WhatsApp is disconnected or the profile is unpaired: nothing is queued."

const toHelp = "recipient: c_N chat id (person or group), p_N person id, a phone number in any format " +
	"(+39 333 123 4567, 3331234567), or the exact name of one person or group (case and accents ignored)"

func (s *Server) addSendTools(srv *mcp.Server) {
	addTool(srv, &mcp.Tool{
		Name: toolSendMessage,
		Description: "Send a WhatsApp text message to a person or group. Returns the stored message id, the resolved " +
			"recipient (name and phone, or group name) and the server time. An ambiguous name returns the candidates " +
			"instead of sending." + sendWarning,
		InputSchema: schemaFor[sendMessageIn](nil),
		Annotations: sendAnnotations("Send a WhatsApp message"),
	}, s.sendMessage)

	addTool(srv, &mcp.Tool{
		Name: toolSendFile,
		Description: "Send a local file over WhatsApp as a photo, video, document or voice note. Limits: 16 MB for " +
			"photos, videos and voice notes, 100 MB for documents. Secrets, keys, databases and repositories are refused " +
			"even in an allowed folder: .env*, *.pem, *.key, id_*, *.kdbx, *.pfx, *.p12, *.ppk, *.ovpn, .git-credentials, " +
			".netrc, .npmrc, .pypirc, *.db, *.sqlite, and anything in a .git, .ssh, .aws, .azure, .docker, .kube or " +
			".gnupg folder." + sendWarning,
		InputSchema: schemaFor[sendFileIn](map[string][]any{"kind": {"auto", "document", "image", "video", "voice_note"}}),
		Annotations: sendAnnotations("Send a file on WhatsApp"),
	}, s.sendFile)

	addTool(srv, &mcp.Tool{
		Name: toolRequestHistory,
		Description: "Ask the user's phone for older messages of one chat, before the oldest stored one. Asynchronous: " +
			"the phone must be online, and the messages arrive in the background within a minute or so. Read them later " +
			"with whatsapp_read_chat. Use it only when the user needs messages older than what is stored: at most one " +
			"request per chat every 10 minutes and 20 a day.",
		InputSchema: schemaFor[requestHistoryIn](nil),
		Annotations: sendAnnotations("Request older WhatsApp history"),
	}, s.requestHistory)
}

// ---- recipients ----

// recipient is a resolved send target.
type recipient struct {
	jid        types.JID
	chatID     int64            // 0 when no chat is stored yet
	kind       string           // dm | group
	person     *store.PersonRef // dm
	groupName  string
	checkPhone string // digits to verify with IsOnWhatsApp before sending; "" when known
	first      bool   // no stored message with this person in any chat
}

// label renders the recipient for results: "Marco Rossi (+39 333 123 4567)" or the wrapped group name.
func (r *recipient) label(w wrapper) string {
	if r.kind == "group" {
		if r.groupName == "" {
			return "unnamed group"
		}
		return "group " + w.wrap(r.groupName)
	}
	if r.person == nil {
		return people.FormatPhone("+" + r.jid.User)
	}
	if r.person.IsMe {
		return "me (notes to self)"
	}
	l := w.label(r.person)
	if r.person.Source == people.SourcePhone && r.person.Phone != "" {
		l += " (not in the address book)"
	}
	return l
}

// resolveRecipient turns the to argument into one send target, or a message
// that explains why it cannot (unknown, ambiguous, not a person or group).
func (s *Server) resolveRecipient(ctx context.Context, arg string) (*recipient, string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, "to is required: " + toHelp, nil
	}
	if id, ok := parseID(arg, "c_"); ok {
		c, found, err := store.ChatByID(ctx, s.DB, id)
		if err != nil || !found {
			return nil, fmt.Sprintf("no chat %s: call whatsapp_list_chats for current ids", arg), err
		}
		return s.chatRecipient(ctx, &c)
	}
	if id, ok := parseID(arg, "p_"); ok {
		c, found, err := store.PersonCandidateByID(ctx, s.DB, id)
		if err != nil || !found {
			return nil, fmt.Sprintf("no person %s: use whatsapp_search_people to find the id", arg), err
		}
		return s.personRecipient(ctx, &c.PersonRef)
	}
	// Only a plain name may match a name: the send gate reads anything shaped
	// like an id or a JID as one, so a group named "P-Rossi" must never take
	// a mistyped "p_rossi".
	if strings.HasPrefix(arg, "c_") || strings.HasPrefix(arg, "p_") {
		return nil, fmt.Sprintf("%q is not a chat (c_N) or person (p_N) id: "+toHelp, arg), nil
	}
	if digits := phoneDigits(arg); digits != "" {
		if !people.IsPlausiblePhone(digits) {
			return nil, fmt.Sprintf("%q is not a valid phone number: write it with the country code, e.g. +39 333 123 4567", arg), nil
		}
		cs, err := store.PersonsByPhone(ctx, s.DB, digits)
		if err != nil {
			return nil, "", err
		}
		if len(cs) > 0 {
			return s.personRecipient(ctx, &cs[0].PersonRef)
		}
		return &recipient{jid: types.NewJID(digits, types.DefaultUserServer), kind: "dm", checkPhone: digits, first: true}, "", nil
	}
	return s.nameRecipient(ctx, arg)
}

func (s *Server) chatRecipient(ctx context.Context, c *store.ChatRow) (*recipient, string, error) {
	switch c.Kind {
	case "dm":
		if c.Person == nil {
			return nil, fmt.Sprintf("chat %s has no person", chatID(c.ID)), nil
		}
		return s.personRecipient(ctx, c.Person)
	case "group":
		j, err := types.ParseJID(c.JID)
		if err != nil {
			return nil, "", err
		}
		return &recipient{jid: j, chatID: c.ID, kind: "group", groupName: c.Name}, "", nil
	}
	return nil, fmt.Sprintf("chat %s is a %s: only people and groups can receive messages", chatID(c.ID), c.Kind), nil
}

func (s *Server) personRecipient(ctx context.Context, p *store.PersonRef) (*recipient, string, error) {
	act, err := store.GetPersonActivity(ctx, s.DB, p.ID, 0)
	if err != nil {
		return nil, "", err
	}
	r := &recipient{kind: "dm", person: p}
	if act.DM != nil {
		r.chatID = act.DM.Chat.ID
	}
	r.first = (act.DM == nil || act.DM.Messages == 0) && act.GroupsTotal == 0
	var lids []string
	for _, j := range act.JIDs {
		if j.Kind == "lid" {
			lids = append(lids, j.JID)
		}
	}
	// A DM goes to the account (LID): a stored number can be out of date, or
	// WhatsApp may have given it to someone else. The number is used only for
	// a person with no known LID.
	switch {
	case len(lids) > 1:
		return nil, fmt.Sprintf("%s holds %d WhatsApp accounts (LIDs), so which one is meant is unknown: cannot send. "+
			"Rebuilding data.db (whatsapp-mcp reingest) splits them into separate people.", personID(p.ID), len(lids)), nil
	case len(lids) == 1:
		if r.jid, err = types.ParseJID(lids[0]); err != nil {
			return nil, "", err
		}
	case p.Phone != "":
		digits := strings.TrimPrefix(p.Phone, "+")
		r.jid = types.NewJID(digits, types.DefaultUserServer)
		if r.chatID == 0 {
			r.checkPhone = digits
		}
	}
	if r.jid.IsEmpty() {
		return nil, fmt.Sprintf("%s has no phone number or WhatsApp id stored: cannot send", personID(p.ID)), nil
	}
	return r, "", nil
}

// nameRecipient accepts only an exact name (case and accents ignored) that
// names one person or one group; anything else lists the candidates.
func (s *Server) nameRecipient(ctx context.Context, arg string) (*recipient, string, error) {
	ms, err := s.matchPeople(ctx, arg)
	if err != nil {
		return nil, "", err
	}
	var exact []personMatch
	selfNamed := false // an exact match only through a name the person chose (push or business name)
	fq := fold(arg)
	for _, m := range ms {
		if m.score >= scoreExact {
			exact = append(exact, m)
			if fold(m.FullName) != fq && fold(m.FirstName) != fq {
				selfNamed = true
			}
		}
	}
	if selfNamed {
		// Anyone can call themselves "Giulia": a saved contact whose name
		// starts with the query is a candidate too, so the send never goes
		// to a stranger by default.
		for _, m := range ms {
			if m.Saved && m.score >= scorePrefix && m.score < scoreExact {
				exact = append(exact, m)
			}
		}
	}
	var groups []store.ChatRow
	if !strings.Contains(arg, "@") { // a JID names a person, never a group by its name
		if groups, err = s.groupsNamed(ctx, arg); err != nil {
			return nil, "", err
		}
	}
	w := newWrapper(s.now())
	switch {
	case len(exact) == 1 && len(groups) == 0:
		return s.personRecipient(ctx, &exact[0].PersonRef)
	case len(exact) == 0 && len(groups) == 1:
		return s.chatRecipient(ctx, &groups[0])
	case len(exact)+len(groups) > 1:
		var b strings.Builder
		fmt.Fprintf(&b, "Not sent: %q names %d recipients:\n", arg, len(exact)+len(groups))
		for i := range exact {
			b.WriteString(personLine(w, &exact[i]))
		}
		b.WriteString(candidateList(w, groups))
		b.WriteString("Call again with to set to one of these ids.")
		return nil, b.String(), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Not sent: no person or group is named exactly %q.", arg)
	if len(ms) > 0 {
		b.WriteString(" Closest people:\n")
		for i := range ms[:min(len(ms), 5)] {
			b.WriteString(personLine(w, &ms[i]))
		}
		b.WriteString("Call again with to set to one of these ids, or the phone number.")
	} else {
		b.WriteString(" Use whatsapp_search_people or whatsapp_list_chats to find the recipient.")
	}
	return nil, b.String(), nil
}

func personLine(w wrapper, m *personMatch) string {
	phone := "phone unknown"
	if m.Phone != "" {
		phone = people.FormatPhone(m.Phone)
	}
	return fmt.Sprintf("- %s %s, %s (%s, last interaction %s)\n", personID(m.ID), w.name(&m.PersonRef), phone,
		nameSource(&m.PersonCandidate), orNever(w.ts(m.LastAt)))
}

// groupsNamed returns the groups whose name equals name, ignoring case and accents.
func (s *Server) groupsNamed(ctx context.Context, name string) ([]store.ChatRow, error) {
	want := fold(name)
	if want == "" {
		return nil, nil
	}
	names, err := store.GroupNames(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	var out []store.ChatRow
	for id, n := range names {
		if fold(n) != want {
			continue
		}
		c, found, err := store.ChatByID(ctx, s.DB, id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b store.ChatRow) int { return cmp.Compare(b.LastMessageAt, a.LastMessageAt) })
	return out, nil
}

// ---- replies ----

// replyContext builds the quoted-message context for reply_to, which must
// be a stored message of the recipient's chat.
func (s *Server) replyContext(ctx context.Context, arg string, r *recipient) (*waE2E.ContextInfo, string, error) {
	if strings.TrimSpace(arg) == "" {
		return nil, "", nil
	}
	id, ok := parseID(arg, "m_")
	if !ok {
		return nil, fmt.Sprintf("reply_to must be a message id like m_8812, got %q", arg), nil
	}
	m, found, err := store.MessageByID(ctx, s.DB, id)
	if err != nil || !found {
		return nil, fmt.Sprintf("no message %s", arg), err
	}
	if r.chatID == 0 || m.ChatID != r.chatID {
		return nil, fmt.Sprintf("reply_to %s is in chat %s, not in the recipient's chat: reply in the chat the message belongs to", arg, chatID(m.ChatID)), nil
	}
	// The author as the message was addressed (phone JID or LID); my own
	// account's phone JID when nothing was stored.
	participant := m.SenderJID
	switch {
	case m.FromMe && participant == "":
		participant = s.WA.Status().AccountPN.ToNonAD().String()
	case participant == "" && r.kind == "dm":
		participant = r.jid.String()
	}
	ci := &waE2E.ContextInfo{StanzaID: proto.String(m.WAID), QuotedMessage: quoted(&m)}
	if participant != "" {
		ci.Participant = proto.String(participant)
	}
	return ci, "", nil
}

// quoted rebuilds the quoted message from the stored row: its kind and text
// are what WhatsApp shows in the reply bubble.
func quoted(m *store.MessageRow) *waE2E.Message {
	text := m.Text
	switch m.Kind {
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: optional(text)}}
	case "video":
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: optional(text)}}
	case "voice", "audio":
		a := &waE2E.AudioMessage{PTT: proto.Bool(m.Kind == "voice")}
		if m.Media != nil && m.Media.Duration > 0 {
			a.Seconds = proto.Uint32(uint32(m.Media.Duration))
		}
		return &waE2E.Message{AudioMessage: a}
	case "document":
		d := &waE2E.DocumentMessage{Caption: optional(text)}
		if m.Media != nil && m.Media.Filename != "" {
			d.FileName = proto.String(m.Media.Filename)
		}
		return &waE2E.Message{DocumentMessage: d}
	case "sticker":
		return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}
	}
	if text == "" {
		text = "[" + m.Kind + "]"
	}
	return &waE2E.Message{Conversation: proto.String(text)}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

// ---- delivery ----

// sendBlocked explains why nothing can be sent now; "" when the device is connected.
func (s *Server) sendBlocked() string {
	if s.Sender == nil || s.Ingest == nil {
		return "Not sent: this process has no WhatsApp client (sending needs the running daemon)."
	}
	if s.closing.Load() {
		return "Not sent: the daemon is stopping. Nothing was queued; try again in a minute, when whatsapp_status shows connected."
	}
	st := s.WA.Status()
	const tail = " Nothing was queued."
	if st.BannedUntil.After(s.now()) {
		return fmt.Sprintf("Not sent: WhatsApp temporarily banned this account (%s) until %s. Sends are refused until then: "+
			"sending during a ban risks a permanent one.%s", st.BanReason, fmtTS(st.BannedUntil.Unix(), s.now()), tail)
	}
	switch st.State {
	case wa.StateConnected:
		return ""
	case wa.StateNotPaired:
		return fmt.Sprintf("Not sent: this profile is not paired.%s %s", tail, s.relinkSteps())
	case wa.StateLoggedOut:
		return fmt.Sprintf("Not sent: WhatsApp logged this device out (%s).%s %s", st.Detail, tail, s.relinkSteps())
	}
	detail := ""
	if st.Detail != "" {
		detail = ", " + st.Detail
	}
	return fmt.Sprintf("Not sent: WhatsApp is %s since %s%s.%s Try again when whatsapp_status shows connected.",
		st.State, fmtTS(st.Since.Unix(), s.now()), detail, tail)
}

// precheck verifies that a phone number without a stored chat has a
// WhatsApp account, and addresses the number WhatsApp knows it by.
func (s *Server) precheck(ctx context.Context, r *recipient) (string, error) {
	if r.checkPhone == "" {
		return "", nil
	}
	res, err := s.Sender.IsOnWhatsApp(ctx, r.checkPhone)
	if err != nil {
		return "", fmt.Errorf("check whether +%s is on WhatsApp: %w", r.checkPhone, err)
	}
	if !res.IsIn {
		return fmt.Sprintf("Not sent: %s has no WhatsApp account.", people.FormatPhone("+"+r.checkPhone)), nil
	}
	switch {
	case res.PhoneNumber.Server == types.DefaultUserServer:
		r.jid = res.PhoneNumber.ToNonAD()
	case res.JID.Server == types.DefaultUserServer:
		r.jid = res.JID.ToNonAD()
	}
	return "", nil
}

// sendResult is what a send kept in its idempotency record. The tool result
// is text only (see addTool).
type sendResult struct {
	MessageID    string `json:"message_id,omitempty"`
	ChatID       string `json:"chat_id,omitempty"`
	Recipient    string `json:"recipient"`
	FirstContact bool   `json:"first_contact,omitempty"`
	StoreError   string `json:"store_error,omitempty"`
	// Unconfirmed: WhatsApp did not answer in time; the message may have arrived.
	Unconfirmed bool   `json:"unconfirmed,omitempty"`
	waID        string // the WhatsApp id the send carried, kept in the idempotency record
}

// sendTimeout bounds one send and its storage.
const sendTimeout = 2 * time.Minute

// idem is the idempotency key of a send and the fingerprint of its arguments.
type idem struct{ key, fp string }

// deliver sends msg, stores it through the live ingestion path and renders
// the result. With an idempotency key, an "unconfirmed" record is written
// before the send and replaced by the result after it, so no retry with that
// key can send a second copy, whatever happens in between.
// w wraps the response's sender-written text, what included. file holds the
// bytes uploaded for a media message: they are kept in the media folder.
func (s *Server) deliver(ctx context.Context, w wrapper, r *recipient, msg *waE2E.Message, file []byte, what string, k idem) (*mcp.CallToolResult, error) {
	// Once the message is on the wire, a cancelled MCP request must not
	// abandon it: send and store run to completion on their own deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	unconfirmed, _ := errResult("WhatsApp did not confirm the send in time. The message may or may not have been delivered: " +
		"check with whatsapp_read_chat before sending again.")
	waID := s.Sender.NewMessageID()
	if err := s.idemSave(ctx, k.key, k.fp, unconfirmed, &sendResult{Recipient: r.label(w), Unconfirmed: true, waID: waID}); err != nil {
		return errResult("Not sent: cannot record the idempotency key: %v", err)
	}
	resp, err := s.Sender.SendMessage(ctx, r.jid, msg, waID)
	if err != nil {
		if errors.Is(err, whatsmeow.ErrMessageTimedOut) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			s.logger().Warn("send not confirmed", "to", r.jid, "err", err)
			return unconfirmed, nil // the unconfirmed record stays under the key
		}
		if k.key != "" {
			if derr := store.DeleteState(ctx, s.DB, store.KeySendIdempotency+k.key); derr != nil {
				s.logger().Error("drop idempotency key", "err", derr)
			}
		}
		if errors.Is(err, wa.ErrNotSendable) {
			return errResult("Not sent: %v. Nothing was queued; try again when whatsapp_status shows connected.", err)
		}
		return errResult("Not sent: %v", err)
	}
	out := &sendResult{Recipient: r.label(w), FirstContact: r.first && r.kind == "dm"}
	rowID, storeErr := s.Ingest.StoreSent(ctx, r.jid, resp, msg)
	elsewhere := "" // the account WhatsApp delivered to, when it is another stored person
	if storeErr == nil {
		m, found, err := store.MessageByID(ctx, s.DB, rowID)
		if err == nil && found {
			out.MessageID, out.ChatID = messageID(rowID), chatID(m.ChatID)
			if c, ok, err := store.ChatByID(ctx, s.DB, m.ChatID); err == nil && ok && r.person != nil && c.Person != nil && c.Person.ID != r.person.ID {
				elsewhere = w.label(c.Person)
			}
		}
		// The send stands whatever happens here: a file not kept is
		// downloaded later like any other.
		if file != nil && s.Media != nil {
			if err := s.Media.SaveSent(ctx, rowID, file); err != nil {
				s.logger().Warn("keep sent file in the media folder", "message", messageID(rowID), "err", err)
			}
		}
	} else {
		s.logger().Error("store sent message", "id", resp.ID, "err", storeErr)
		out.StoreError = storeErr.Error()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Sent %s to %s", what, out.Recipient)
	if out.ChatID != "" {
		fmt.Fprintf(&b, " in %s", out.ChatID)
	}
	fmt.Fprintf(&b, " at %s (WhatsApp server time).", fmtTS(resp.Timestamp.Unix(), time.Time{}))
	if out.MessageID != "" {
		fmt.Fprintf(&b, " Stored as %s.", out.MessageID)
	} else {
		fmt.Fprintf(&b, " The message was sent but could not be stored (%s); it will appear after the next history sync. Do not send it again.", out.StoreError)
	}
	if elsewhere != "" {
		fmt.Fprintf(&b, "\nNote: WhatsApp delivered it to the account that has this number now, %s, and it is stored in "+
			"that chat: the number may have changed hands. Tell the user.", elsewhere)
	}
	if out.FirstContact {
		b.WriteString("\nNote: first message to this number - there is no earlier conversation with this person in any chat. " +
			"Messages to people who never wrote to this account raise WhatsApp's ban risk; do not repeat this for other new numbers.")
	}
	res := textResult(b.String())
	if err := s.idemSave(ctx, k.key, k.fp, res, out); err != nil {
		s.logger().Error("save idempotency key", "err", err)
	}
	return res, nil
}

func (s *Server) logger() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// ---- idempotency ----

type idemRecord struct {
	Fingerprint string     `json:"fp"`
	Text        string     `json:"text"`
	Out         sendResult `json:"out"`
	// WAID is the WhatsApp id the send carried: an unconfirmed send whose
	// message is later stored (a crash after the send, or a history sync
	// from the phone) is resolved as sent.
	WAID string `json:"wa_id,omitempty"`
}

func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// idemLookup returns the earlier result for key, or a message when the key
// was used for a different send.
func (s *Server) idemLookup(ctx context.Context, key, fp string) (*mcp.CallToolResult, string, error) {
	if key == "" {
		return nil, "", nil
	}
	if len(key) > 200 {
		return nil, "idempotency_key is too long: use at most 200 characters", nil
	}
	if err := store.DeleteStateBefore(ctx, s.DB, store.KeySendIdempotency, store.Now().Add(-idempotencyTTL)); err != nil {
		return nil, "", err
	}
	v, at, ok, err := store.GetStateAt(ctx, s.DB, store.KeySendIdempotency+key)
	if err != nil || !ok {
		return nil, "", err
	}
	var rec idemRecord
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		return nil, "", err
	}
	if rec.Fingerprint != fp {
		return nil, fmt.Sprintf("idempotency_key %q was already used at %s for a different send: use a new key for a new message", key, fmtTS(at.Unix(), s.now())), nil
	}
	if rec.Out.Unconfirmed {
		if resolved, err := resolveUnconfirmed(ctx, s.DB, key, &rec); err != nil {
			return nil, "", err
		} else if resolved {
			return textResult(fmt.Sprintf("Not sent again: idempotency_key %q was used at %s and that message was delivered. Earlier result:\n%s",
				key, fmtTS(at.Unix(), s.now()), rec.Text)), "", nil
		}
		res, _ := errResult("Not sent again: idempotency_key %q was used at %s and WhatsApp did not confirm that send. "+
			"Check with whatsapp_read_chat whether it arrived; to send anyway, use a new key.", key, fmtTS(at.Unix(), s.now()))
		return res, "", nil
	}
	return textResult(fmt.Sprintf("Not sent again: idempotency_key %q already sent this at %s. Earlier result:\n%s", key, fmtTS(at.Unix(), s.now()), rec.Text)), "", nil
}

func (s *Server) idemSave(ctx context.Context, key, fp string, res *mcp.CallToolResult, out *sendResult) error {
	if key == "" || out == nil {
		return nil
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	b, err := json.Marshal(idemRecord{Fingerprint: fp, Text: text, Out: *out, WAID: out.waID})
	if err != nil {
		return err
	}
	return store.SetState(ctx, s.DB, store.KeySendIdempotency+key, string(b))
}

// resolveUnconfirmed checks whether the message of an unconfirmed send was
// stored after all (sent by this account with the send's WhatsApp id). When
// it was, the record becomes a confirmed result and resolved is true.
func resolveUnconfirmed(ctx context.Context, db *store.DB, key string, rec *idemRecord) (resolved bool, err error) {
	if !rec.Out.Unconfirmed || rec.WAID == "" {
		return false, nil
	}
	m, found, err := store.SentMessageByWAID(ctx, db, rec.WAID)
	if err != nil || !found {
		return false, err
	}
	rec.Out.Unconfirmed = false
	rec.Out.MessageID, rec.Out.ChatID = messageID(m.ID), chatID(m.ChatID)
	rec.Text = fmt.Sprintf("Sent to %s in %s at %s (WhatsApp did not confirm in time; the message was stored later as %s).",
		rec.Out.Recipient, rec.Out.ChatID, fmtTS(m.TS, time.Time{}), rec.Out.MessageID)
	b, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	// Keep the record's time: it still expires 24 h after the send.
	return true, store.ReplaceStateValue(ctx, db, store.KeySendIdempotency+key, string(b))
}

// ResolveUnconfirmedSends turns every unconfirmed send record whose message
// was stored into a confirmed one. The daemon runs it at startup: a send cut
// by a crash or a stop may have been stored, or arrive with a history sync.
func ResolveUnconfirmedSends(ctx context.Context, db *store.DB) (int, error) {
	recs, err := store.StatePrefix(ctx, db, store.KeySendIdempotency)
	if err != nil {
		return 0, err
	}
	n := 0
	for key, v := range recs {
		var rec idemRecord
		if json.Unmarshal([]byte(v), &rec) != nil {
			continue
		}
		ok, err := resolveUnconfirmed(ctx, db, strings.TrimPrefix(key, store.KeySendIdempotency), &rec)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// Drain refuses new sends and waits up to timeout for the send in flight
// (sends run one at a time, each bounded by sendTimeout). It reports
// whether no send is still running.
func (s *Server) Drain(timeout time.Duration) bool {
	s.closing.Store(true)
	deadline := time.Now().Add(timeout)
	for {
		if s.sendMu.TryLock() {
			s.sendMu.Unlock()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---- whatsapp_send_message ----

type sendMessageIn struct {
	To             string `json:"to" jsonschema:"recipient: c_N (person or group), p_N, a phone number in any format (+39 333 123 4567, 3331234567), or the exact name of one person or group (case and accents ignored)"`
	Text           string `json:"text" jsonschema:"the message text, exactly as it should arrive"`
	ReplyTo        string `json:"reply_to,omitempty" jsonschema:"m_N of a stored message in the same chat to reply to (shown quoted)"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"any unique string; pass one to make retries safe: a call with the same key within 24 h returns the first result instead of sending again"`
}

func (s *Server) sendMessage(ctx context.Context, _ *mcp.CallToolRequest, in sendMessageIn) (*mcp.CallToolResult, error) {
	if strings.TrimSpace(in.Text) == "" {
		return errResult("text is required: the message to send")
	}
	if n := len([]rune(in.Text)); n > maxTextLen {
		return errResult("text is %d characters: WhatsApp allows at most %d per message", n, maxTextLen)
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	fp := fingerprint(toolSendMessage, strings.TrimSpace(in.To), in.Text, strings.TrimSpace(in.ReplyTo))
	if res, msg, err := s.idemLookup(ctx, in.IdempotencyKey, fp); err != nil || msg != "" || res != nil {
		if msg != "" {
			return errResult("%s", msg)
		}
		return res, err
	}
	r, msg, err := s.resolveRecipient(ctx, in.To)
	if err != nil || msg != "" {
		if err != nil {
			return nil, err
		}
		return errResult("%s", msg)
	}
	ci, msg, err := s.replyContext(ctx, in.ReplyTo, r)
	if err != nil || msg != "" {
		if err != nil {
			return nil, err
		}
		return errResult("Not sent: %s", msg)
	}
	if msg := s.sendBlocked(); msg != "" {
		return errResult("%s", msg)
	}
	if msg, err := s.precheck(ctx, r); err != nil || msg != "" {
		if err != nil {
			return errResult("Not sent: %v", err)
		}
		return errResult("%s", msg)
	}
	m := &waE2E.Message{Conversation: proto.String(in.Text)}
	if ci != nil {
		m = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(in.Text), ContextInfo: ci}}
	}
	what := "the message"
	if ci != nil {
		what = "the reply to " + in.ReplyTo
	}
	return s.deliver(ctx, newWrapper(s.now()), r, m, nil, what, idem{in.IdempotencyKey, fp})
}
