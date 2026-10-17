package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// MediaSink receives live media messages once they are committed, and the
// phone's answers to media re-upload requests. Both calls must not block.
type MediaSink interface {
	LiveMedia(messageID, mediaID int64)
	MediaRetry(evt *events.MediaRetry)
}

// SetMediaSink registers the media downloader. Call it before Start.
func (c *Client) SetMediaSink(s MediaSink) { c.media = s }

// WhatsApp is the whatsmeow client; nil before Start and on unpaired profiles.
func (c *Client) WhatsApp() *whatsmeow.Client { return c.wa }

// queueLiveMedia hands a just-committed live message with media to the sink.
func (c *Client) queueLiveMedia(ctx context.Context, evt *events.Message) {
	if c.media == nil {
		return
	}
	cont, ok := extract(unwrap(evt.Message))
	if !ok || cont.Media == nil {
		return
	}
	chat, err := c.ing.findChat(ctx, c.db, evt.Info.MessageSource)
	if err != nil || chat.id == 0 {
		return
	}
	f, found, err := store.LoadMessage(ctx, c.db, chat.id, evt.Info.ID)
	if err != nil || !found || f.MediaID == 0 {
		return
	}
	c.media.LiveMedia(f.ID, f.MediaID)
}
