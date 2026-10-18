package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/outgoing"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// ---- whatsapp_send_file ----

type sendFileIn struct {
	To             string `json:"to" jsonschema:"recipient: c_N (person or group), p_N, a phone number in any format, or the exact name of one person or group (case and accents ignored)"`
	Path           string `json:"path" jsonschema:"absolute path of a local file in an allowed folder: Desktop, Documents, Downloads, Pictures, Videos (Movies on macOS), Music, a cloud drive (OneDrive, iCloud Drive, Dropbox), your session's scratchpad (save files you generate there) or the WhatsApp media folder; the user can change the folders in config.json in the profile data dir. E.g. C:\\Users\\me\\Documents\\offerta.pdf or /Users/me/Documents/offerta.pdf"`
	Kind           string `json:"kind,omitempty" jsonschema:"auto (default): JPEG/PNG as photo, MP4 as video, anything else as document; image takes only JPEG/PNG, video only MP4; only voice_note sends a voice note: any audio, converted to Opus"`
	Caption        string `json:"caption,omitempty" jsonschema:"text shown under a photo, video or document; voice notes have none"`
	ReplyTo        string `json:"reply_to,omitempty" jsonschema:"m_N of a stored message in the same chat to reply to"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"any unique string; a call with the same key within 24 h returns the first result instead of sending again"`
}

// filePolicy is the send-file policy: s.Files, else the profile's, read
// from its config.json at each send so an edit applies without a restart.
func (s *Server) filePolicy() (outgoing.Policy, error) {
	if s.Files != nil {
		return *s.Files, nil
	}
	return outgoing.LoadPolicy(s.DataDir)
}

var mediaTypes = map[outgoing.Kind]whatsmeow.MediaType{
	outgoing.KindImage: whatsmeow.MediaImage, outgoing.KindVideo: whatsmeow.MediaVideo,
	outgoing.KindVoice: whatsmeow.MediaAudio, outgoing.KindDocument: whatsmeow.MediaDocument,
}

var kindNames = map[outgoing.Kind]string{
	outgoing.KindImage: "photo", outgoing.KindVideo: "video", outgoing.KindVoice: "voice note", outgoing.KindDocument: "document",
}

func (s *Server) sendFile(ctx context.Context, _ *mcp.CallToolRequest, in sendFileIn) (*mcp.CallToolResult, error) {
	kind, err := outgoing.ParseKind(in.Kind)
	if err != nil {
		return errResult("%v", err)
	}
	if kind == outgoing.KindVoice && strings.TrimSpace(in.Caption) != "" {
		return errResult("voice notes have no caption: send the text with whatsapp_send_message")
	}
	if n := len([]rune(in.Caption)); n > maxTextLen {
		return errResult("caption is %d characters: WhatsApp allows at most %d", n, maxTextLen)
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	fp := fingerprint(toolSendFile, strings.TrimSpace(in.To), strings.TrimSpace(in.Path), string(kind), in.Caption, strings.TrimSpace(in.ReplyTo))
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
	policy, err := s.filePolicy()
	if err != nil {
		return errResult("Not sent: the send-file settings cannot be read: %v", err)
	}
	// File names and paths can come from a sender (files in the media
	// folder): they, and errors quoting them, are wrapped.
	w := newWrapper(s.now())
	f, err := outgoing.Prepare(ctx, policy, in.Path, kind)
	if err != nil {
		return errResult("Not sent: %s", w.wrap(err.Error()))
	}
	if msg, err := s.precheck(ctx, r); err != nil || msg != "" {
		if err != nil {
			return errResult("Not sent: %v", err)
		}
		return errResult("%s", msg)
	}
	up, err := s.Sender.Upload(ctx, f.Data, mediaTypes[f.Kind])
	if err != nil {
		return errResult("Not sent: upload of %s failed: %v", w.wrap(f.FileName), err)
	}
	m := fileMessage(f, up, in.Caption, ci)
	what := fmt.Sprintf("the %s %s", kindNames[f.Kind], w.wrap(f.FileName))
	if f.Kind == outgoing.KindVoice {
		what = fmt.Sprintf("a %d s voice note from %s", f.Seconds, w.wrap(f.FileName))
	}
	return s.deliver(ctx, w, r, m, f.Data, what, idem{in.IdempotencyKey, fp})
}

// fileMessage builds the WhatsApp message for an uploaded file.
func fileMessage(f *outgoing.File, up whatsmeow.UploadResponse, caption string, ci *waE2E.ContextInfo) *waE2E.Message {
	size := up.FileLength
	if size == 0 {
		size = uint64(len(f.Data))
	}
	switch f.Kind {
	case outgoing.KindImage:
		x := &waE2E.ImageMessage{Caption: optional(caption), Mimetype: proto.String(f.Mimetype), URL: proto.String(up.URL),
			DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(size), JPEGThumbnail: f.Thumbnail, ContextInfo: ci}
		if f.Width > 0 {
			x.Width, x.Height = proto.Uint32(f.Width), proto.Uint32(f.Height)
		}
		return &waE2E.Message{ImageMessage: x}
	case outgoing.KindVideo:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: optional(caption), Mimetype: proto.String(f.Mimetype),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: proto.Uint64(size), ContextInfo: ci}}
	case outgoing.KindVoice:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String(f.Mimetype), PTT: proto.Bool(true),
			Seconds: proto.Uint32(f.Seconds), Waveform: f.Waveform, URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(size), ContextInfo: ci}}
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: optional(caption), Mimetype: proto.String(f.Mimetype),
		FileName: proto.String(f.FileName), Title: proto.String(f.FileName), URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
		MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(size), ContextInfo: ci}}
}

// ---- whatsapp_request_history ----

type requestHistoryIn struct {
	Chat  string `json:"chat" jsonschema:"c_N, p_N, phone number or exact person or group name"`
	Count int    `json:"count,omitempty" jsonschema:"how many older messages to ask for, 1-200, default 50"`
}

func (s *Server) requestHistory(ctx context.Context, _ *mcp.CallToolRequest, in requestHistoryIn) (*mcp.CallToolResult, error) {
	count := in.Count
	if count == 0 {
		count = 50
	}
	if count < 1 || count > 200 {
		return errResult("count must be between 1 and 200, got %d", in.Count)
	}
	c, msg := s.resolveChat(ctx, in.Chat)
	if c == nil {
		return errResult("%s", msg)
	}
	anchor, ok, err := oldestMessage(ctx, s.DB, c)
	if err != nil {
		return nil, err
	}
	if !ok {
		return errResult("%s has no stored message to start from: WhatsApp needs one to ask for older history", chatID(c.ID))
	}
	// One at a time with sends: the quota check and the record stay exact.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if msg := s.sendBlocked(); msg != "" {
		return errResult("%s", strings.Replace(msg, "Not sent", "Not requested", 1))
	}
	if msg, err := s.historyQuota(ctx, c.ID); err != nil || msg != "" {
		if err != nil {
			return nil, err
		}
		return errResult("%s", msg)
	}
	if err := s.Sender.RequestHistory(ctx, anchor, count); err != nil {
		return errResult("Not requested: %v", err)
	}
	if err := s.recordHistoryRequest(ctx, c.ID); err != nil {
		s.logger().Warn("record history request", "err", err)
	}
	w := newWrapper(s.now())
	oldest := w.ts(anchor.Timestamp.Unix())
	return textResult(fmt.Sprintf("Asked the phone for up to %d messages of %s (%s) older than %s, the oldest stored one. "+
		"This is asynchronous: the phone must be online, and the messages arrive in the background, usually within a minute. "+
		"Check later with whatsapp_read_chat chat=%s before=%q. If nothing older arrives, the phone has no older history for this chat.",
		count, chatID(c.ID), w.chatName(c), oldest, chatID(c.ID), anchor.Timestamp.In(rome).Add(time.Second).Format(time.RFC3339))), nil
}

// History requests reach the phone; keep them rare.
const (
	historyPerChatEvery = 10 * time.Minute
	historyPerDay       = 20
)

// historyQuota explains why a history request for chatID must wait, "" when
// it may go: at most one per chat every 10 minutes and 20 a day (Europe/Rome).
func (s *Server) historyQuota(ctx context.Context, chatID int64) (string, error) {
	now := s.now()
	last, err := store.GetStateTS(ctx, s.DB, store.KeyHistoryRequest+strconv.FormatInt(chatID, 10))
	if err != nil {
		return "", err
	}
	if !last.IsZero() && now.Sub(last) < historyPerChatEvery {
		return fmt.Sprintf("Not requested: older history of this chat was asked for at %s; ask again after %s (at most one request per chat every 10 minutes).",
			fmtTS(last.Unix(), now), fmtTS(last.Add(historyPerChatEvery).Unix(), now)), nil
	}
	day, n, err := s.historyDay(ctx)
	if err != nil {
		return "", err
	}
	if day == now.In(rome).Format("2006-01-02") && n >= historyPerDay {
		return fmt.Sprintf("Not requested: %d history requests today already, the daily limit; ask again tomorrow.", n), nil
	}
	return "", nil
}

// historyDay reads the day (Europe/Rome) and count of today's history requests.
func (s *Server) historyDay(ctx context.Context) (string, int, error) {
	v, _, err := store.GetState(ctx, s.DB, store.KeyHistoryRequestsDay)
	if err != nil {
		return "", 0, err
	}
	day, count, _ := strings.Cut(v, " ")
	n, _ := strconv.Atoi(count)
	return day, n, nil
}

// recordHistoryRequest counts a history request that was sent.
func (s *Server) recordHistoryRequest(ctx context.Context, chatID int64) error {
	now := s.now()
	if err := store.SetState(ctx, s.DB, store.KeyHistoryRequest+strconv.FormatInt(chatID, 10), strconv.FormatInt(now.Unix(), 10)); err != nil {
		return err
	}
	day, n, err := s.historyDay(ctx)
	if err != nil {
		return err
	}
	today := now.In(rome).Format("2006-01-02")
	if day != today {
		n = 0
	}
	return store.SetState(ctx, s.DB, store.KeyHistoryRequestsDay, fmt.Sprintf("%s %d", today, n+1))
}

// oldestMessage is the anchor of a history request: the chat's oldest stored message.
func oldestMessage(ctx context.Context, db *store.DB, c *store.ChatRow) (types.MessageInfo, bool, error) {
	var waID string
	var fromMe bool
	var ts int64
	err := db.QueryRowContext(ctx, `SELECT wa_id, from_me, ts FROM messages WHERE chat_id = ? AND kind != 'system'
		ORDER BY ts, id LIMIT 1`, c.ID).Scan(&waID, &fromMe, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return types.MessageInfo{}, false, nil
	}
	if err != nil {
		return types.MessageInfo{}, false, err
	}
	j, err := types.ParseJID(c.JID)
	if err != nil {
		return types.MessageInfo{}, false, err
	}
	return types.MessageInfo{MessageSource: types.MessageSource{Chat: j, IsFromMe: fromMe, IsGroup: c.Kind == "group"},
		ID: waID, Timestamp: time.Unix(ts, 0)}, true, nil
}
