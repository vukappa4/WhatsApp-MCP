package wa

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type sinkCalls struct{ live [][2]int64 }

func (s *sinkCalls) LiveMedia(messageID, mediaID int64) {
	s.live = append(s.live, [2]int64{messageID, mediaID})
}
func (s *sinkCalls) MediaRetry(*events.MediaRetry) {}

// A committed live message with media reaches the sink with its row ids;
// text messages do not.
func TestQueueLiveMedia(t *testing.T) {
	in := newIngestor(t)
	sink := &sinkCalls{}
	c := &Client{db: in.DB, ing: in}
	c.SetMediaSink(sink)
	ctx := context.Background()
	for _, ev := range []*events.Message{
		{Info: dmFromMarco("T1", t0), Message: textMsg("ciao")},
		{Info: dmFromMarco("V1", t0), Message: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg"), DirectPath: proto.String("/v/x")}}}}},
	} {
		if _, err := in.LiveMessage(ctx, ev, nil); err != nil {
			t.Fatal(err)
		}
		c.queueLiveMedia(ctx, ev)
	}
	if len(sink.live) != 1 {
		t.Fatalf("sink calls = %v", sink.live)
	}
	want := [2]int64{one[int64](t, in, `SELECT id FROM messages WHERE wa_id = 'V1'`), one[int64](t, in, `SELECT media_id FROM messages WHERE wa_id = 'V1'`)}
	if sink.live[0] != want {
		t.Fatalf("sink got %v, want %v", sink.live[0], want)
	}
}
