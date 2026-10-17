package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// messenger is the part of the whatsmeow client the send path uses.
type messenger interface {
	IsConnected() bool
	IsLoggedIn() bool
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	GenerateMessageID() types.MessageID
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
}

// ErrNotSendable means the linked device cannot send right now. Nothing is queued.
var ErrNotSendable = errors.New("whatsapp is not connected")

// ready fails unless the client is paired, logged in and connected.
func (c *Client) ready() (messenger, error) {
	st := c.Status()
	c.mu.RLock()
	m := c.msgr
	c.mu.RUnlock()
	switch {
	case !st.Paired():
		return nil, fmt.Errorf("%w: profile not paired", ErrNotSendable)
	case m == nil || !m.IsLoggedIn():
		return nil, fmt.Errorf("%w: logged out", ErrNotSendable)
	case st.State != StateConnected || !m.IsConnected():
		return nil, fmt.Errorf("%w: state %s", ErrNotSendable, st.State)
	}
	return m, nil
}

// NewMessageID returns a fresh WhatsApp message id for a send: knowing it
// before the send lets an unconfirmed send be recognized later.
func (c *Client) NewMessageID() string {
	c.mu.RLock()
	m := c.msgr
	c.mu.RUnlock()
	if m != nil {
		return string(m.GenerateMessageID())
	}
	return string(whatsmeow.GenerateMessageID())
}

// SendMessage sends msg to a user or group JID with the given message id
// (a fresh one when empty) and waits for the server ack.
func (c *Client) SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, id string) (whatsmeow.SendResponse, error) {
	m, err := c.ready()
	if err != nil {
		return whatsmeow.SendResponse{}, err
	}
	return m.SendMessage(ctx, to, msg, whatsmeow.SendRequestExtra{ID: types.MessageID(id)})
}

// IsOnWhatsApp asks WhatsApp whether +digits has an account.
func (c *Client) IsOnWhatsApp(ctx context.Context, digits string) (types.IsOnWhatsAppResponse, error) {
	m, err := c.ready()
	if err != nil {
		return types.IsOnWhatsAppResponse{}, err
	}
	rs, err := m.IsOnWhatsApp(ctx, []string{"+" + digits})
	if err != nil {
		return types.IsOnWhatsAppResponse{}, err
	}
	for _, r := range rs {
		if r.IsIn {
			return r, nil
		}
	}
	return types.IsOnWhatsAppResponse{}, nil
}

// Upload encrypts and uploads a media file.
func (c *Client) Upload(ctx context.Context, data []byte, mt whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	m, err := c.ready()
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}
	return m.Upload(ctx, data, mt)
}

// RequestHistory asks the phone for up to count messages older than anchor.
// The answer arrives later as an ON_DEMAND history sync notification, which
// goes through the same archive, download and ingest path as every blob.
func (c *Client) RequestHistory(ctx context.Context, anchor types.MessageInfo, count int) error {
	if _, err := c.ready(); err != nil {
		return err
	}
	if c.sendPeer == nil {
		return fmt.Errorf("%w: no client", ErrNotSendable)
	}
	// BuildHistorySyncRequest only builds a proto; it does not use the client.
	return c.sendPeer(ctx, (*whatsmeow.Client)(nil).BuildHistorySyncRequest(&anchor, count))
}

// StoreSent archives and ingests a message this device just sent, exactly
// like a live message from the account owner (raw event, then ingestion with
// source live). to is the JID the message was addressed to; resp is the
// server's answer. It returns the stored row id.
func (in *Ingestor) StoreSent(ctx context.Context, to types.JID, resp whatsmeow.SendResponse, msg *waE2E.Message) (int64, error) {
	chat := resp.Chat
	if chat.IsEmpty() {
		chat = to
	}
	chat = chat.ToNonAD()
	pn, lid := in.self()
	sender := resp.Sender
	if sender.IsEmpty() {
		sender = pn
		if chat.Server == types.HiddenUserServer && !lid.IsEmpty() {
			sender = lid
		}
	}
	ts := resp.Timestamp
	if ts.IsZero() {
		ts = in.now()
	}
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: sender.ToNonAD(), IsFromMe: true, IsGroup: chat.Server == types.GroupServer},
		ID:            resp.ID, Timestamp: ts,
	}
	if t := to.ToNonAD(); people.IsUser(chat) && people.IsUser(t) && t != chat {
		// The server answered with the account behind the number: link both,
		// unless that account is someone else stored here. Then the number
		// went to them, and the message is stored in their chat, unlinked.
		other, err := in.otherPerson(ctx, t, chat)
		if err != nil {
			return 0, fmt.Errorf("sent message %s: %w", resp.ID, err)
		}
		if other {
			in.log().Warn("the number sent to belongs to another stored person's account: not linking them", "to", t, "account", chat, "id", resp.ID)
		} else {
			info.RecipientAlt = t
		}
	}
	evt := &events.Message{Info: info, Message: msg, RawMessage: msg}
	payload, key, err := encodeMessage(evt, nil)
	if err != nil {
		return 0, err
	}
	// The message is on the wire: keep trying to archive it until the
	// send's deadline, so a short storage hiccup does not lose it.
	ev, err := in.archive(ctx, store.RawMessage, key, payload)
	for wait := time.Second; err != nil && ctx.Err() == nil; wait = min(wait*2, 15*time.Second) {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
			ev, err = in.archive(ctx, store.RawMessage, key, payload)
		}
	}
	if err != nil {
		return 0, fmt.Errorf("sent message %s: %w", resp.ID, err)
	}
	if !ev.Ingested {
		if err := in.applyAndMark(ctx, ev); err != nil {
			return 0, err
		}
	}
	var id int64
	err = in.DB.Tx(ctx, func(tx *sql.Tx) error {
		ref, err := in.findChat(ctx, tx, info.MessageSource)
		if err != nil || ref.id == 0 {
			return err
		}
		f, ok, err := store.LoadMessage(ctx, tx, ref.id, info.ID)
		if ok {
			id = f.ID
		}
		return err
	})
	if err == nil && id == 0 {
		err = fmt.Errorf("sent message %s was not stored", resp.ID)
	}
	return id, err
}

// otherPerson reports whether a and b are both stored, as two persons.
func (in *Ingestor) otherPerson(ctx context.Context, a, b types.JID) (bool, error) {
	pa, err := people.Lookup(ctx, in.DB, a)
	if err != nil || pa == 0 {
		return false, err
	}
	pb, err := people.Lookup(ctx, in.DB, b)
	return pb != 0 && pb != pa, err
}
