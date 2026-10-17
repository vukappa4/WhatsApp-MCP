package mcpserver

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/media"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

const toolGetMedia = "whatsapp_get_media"

// Waits of one whatsapp_get_media call; past them the tool says when to call again.
const (
	mediaDownloadWait   = 20 * time.Second
	mediaTranscriptWait = 40 * time.Second
	// maxInlineImage bounds images returned as MCP image content.
	maxInlineImage = 1 << 20
)

// sniffImage names the image type of data by its magic bytes: JPEG, PNG,
// GIF or WebP, "" for anything else. The sender's mimetype is never trusted
// for what Claude is shown as a picture.
func sniffImage(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// inlineImage reads a downloaded picture for an image block: only a real
// JPEG, PNG, GIF or WebP up to maxInlineImage, with its sniffed type.
func inlineImage(path string) (*mcp.ImageContent, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxInlineImage+1))
	if err != nil || len(data) > maxInlineImage {
		return nil, false
	}
	mt := sniffImage(data)
	if mt == "" {
		return nil, false
	}
	return &mcp.ImageContent{Data: data, MIMEType: mt}, true
}

type getMediaIn struct {
	Message        string `json:"message" jsonschema:"m_N (from read_chat or search_messages) of a message with an image, video, voice note, audio, document or sticker"`
	Mode           string `json:"mode,omitempty" jsonschema:"auto (default): the file, plus the transcript or the picture; transcript: wait for the text of voice and audio; path: only download"`
	ResponseFormat string `json:"response_format,omitempty" jsonschema:"default concise; detailed adds WhatsApp ids, hashes, engine and error details"`
}

func (s *Server) getMedia(ctx context.Context, _ *mcp.CallToolRequest, in getMediaIn) (*mcp.CallToolResult, error) {
	id, ok := parseID(in.Message, "m_")
	if !ok {
		return errResult("message must be a message id like m_8812, got %q", in.Message)
	}
	mode := in.Mode
	switch mode {
	case "":
		mode = "auto"
	case "auto", "transcript", "path":
	default:
		return errResult("mode must be auto, transcript or path, got %q", in.Mode)
	}
	msg, found, err := store.MessageByID(ctx, s.DB, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return errResult("no message %s", in.Message)
	}
	if msg.Media == nil {
		return errResult("%s is a %s message without a file; read it with whatsapp_read_chat around=%s", in.Message, msg.Kind, in.Message)
	}
	audio := msg.Kind == "voice" || msg.Kind == "audio"
	if mode == "transcript" && !audio {
		return errResult("%s is a %s, not a voice note or audio: use mode auto or path", in.Message, msg.Kind)
	}
	if s.Media == nil {
		return errResult("media downloads are not available in this daemon")
	}
	res, err := s.Media.Get(ctx, media.Request{MessageID: id, Transcript: audio && mode != "path",
		DownloadWait: mediaDownloadWait, TranscriptWait: mediaTranscriptWait})
	if errors.Is(err, media.ErrNoMedia) {
		return errResult("%s has no media", in.Message)
	}
	if err != nil {
		return nil, err
	}
	fresh, err := s.freshness(ctx, msg.TS, msg.TS)
	if err != nil {
		return nil, err
	}
	j := res.Job
	w := newWrapper()
	full := detailed(in.ResponseFormat)

	var b strings.Builder
	b.WriteString(fresh + "\n")
	parts := []string{messageID(id) + " " + msg.Kind}
	if j.Duration > 0 {
		parts = append(parts, fmt.Sprintf("%d:%02d", j.Duration/60, j.Duration%60))
	}
	if j.Filename != "" {
		parts = append(parts, w.wrap(j.Filename))
	}
	if mt := safeMime(j.Mimetype); mt != "" {
		parts = append(parts, mt)
	}
	if sz := humanSize(j.Size); sz != "" {
		parts = append(parts, sz)
	}
	parts = append(parts, "from "+w.sender(&msg))
	if chat, ok, err := store.ChatByID(ctx, s.DB, msg.ChatID); err == nil && ok {
		parts = append(parts, fmt.Sprintf("in %s %s", chatID(chat.ID), w.chatName(&chat)))
	}
	parts = append(parts, fmtTS(msg.TS))
	b.WriteString(strings.Join(parts, " · ") + "\n")
	if msg.Text != "" {
		b.WriteString("Caption: " + w.wrap(clip(msg.Text, 800)) + "\n")
	}
	if full {
		fmt.Fprintf(&b, "WhatsApp id %s, media row %d, state %s, %d download attempts", j.WAID, j.MediaID, j.State, j.Attempts)
		if len(j.SHA256) > 0 {
			fmt.Fprintf(&b, ", sha256 %s", hex.EncodeToString(j.SHA256))
		}
		b.WriteString("\n")
	}

	// The file.
	switch {
	case res.Path != "":
		// The file name part comes from the sender.
		fmt.Fprintf(&b, "File: %s\n", w.wrap(res.Path))
	case res.Downloading:
		fmt.Fprintf(&b, "File: downloading; call again in about %d s.\n", max(5, int(res.RetryIn.Seconds())))
	default:
		b.WriteString("File: " + fileProblem(w, j, res.DownloadErr) + "\n")
	}

	// The transcript.
	if audio && mode != "path" {
		switch {
		case j.HasTranscript && j.Transcript == "":
			b.WriteString("Transcript: no speech detected.\n")
		case j.HasTranscript:
			label := "Transcript"
			if full {
				label += " (" + j.TranscriptEngine
				if j.TranscriptLang != "" {
					label += ", " + j.TranscriptLang
				}
				label += ")"
			}
			b.WriteString(label + ": " + w.wrap(j.Transcript) + "\n")
		case res.Transcribing:
			fmt.Fprintf(&b, "Transcript: transcribing; call again in about %d s with mode=transcript.\n", max(5, int(res.RetryIn.Seconds())))
		case res.Path == "":
		case res.TranscriptErr != nil:
			// Tool errors can quote the file name, which the sender chose.
			b.WriteString("Transcript: not available - " + w.wrap(res.TranscriptErr.Error()) + "\n")
		case j.TranscriptError != "":
			b.WriteString("Transcript: failed - " + w.wrap(j.TranscriptError) + "\n")
		}
	}

	result := textResult(strings.TrimRight(b.String(), "\n"))
	// Small pictures inline, so Claude sees them without opening the file.
	if mode == "auto" && res.Path != "" && (msg.Kind == "image" || msg.Kind == "sticker") {
		if pic, ok := inlineImage(res.Path); ok {
			result.Content = append(result.Content, pic)
		}
	}
	return result, nil
}

// fileProblem explains why a media file is not on disk and what to do. The
// reason is wrapped: file errors can quote the file name the sender chose.
func fileProblem(w wrapper, j store.MediaJob, err error) string {
	reason := j.Error
	if err != nil {
		reason = err.Error()
	}
	if reason != "" {
		reason = w.wrap(reason)
	}
	switch j.State {
	case store.MediaUnavailable:
		return "unavailable - " + reason + ". Ask the sender to send it again."
	case store.MediaExpired:
		return "expired - " + reason + ". Call again when the phone is online."
	case store.MediaFailed:
		return "download failed - " + reason + ". Calling again retries."
	}
	if reason == "" {
		reason = "not downloaded yet"
	}
	return "not downloaded - " + reason + "."
}
