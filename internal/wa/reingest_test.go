package wa

import (
	"context"
	"database/sql"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

func imageMsg(caption, path string) *waE2E.Message {
	return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String(caption), Mimetype: proto.String("image/jpeg"),
		DirectPath: proto.String(path), MediaKey: []byte("key"), FileSHA256: []byte("sha"), FileLength: proto.Uint64(10)}}
}

func voiceMsg(path string) *waE2E.Message {
	return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(9), Mimetype: proto.String("audio/ogg"),
		DirectPath: proto.String(path), MediaKey: []byte("key2"), FileSHA256: []byte("sha2")}}
}

// reingest keeps media rows: downloads, local files, re-uploaded paths and
// transcripts survive, and the transcript is searchable again.
func TestReingestKeepsMediaAndTranscripts(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	live(t, in, dmFromMarco("IMG1", t0), imageMsg("la foto", "/v/img"))
	live(t, in, dmFromMarco("VOI1", t0.Add(1)), voiceMsg("/v/voice"))
	img := one[int64](t, in, `SELECT media_id FROM messages WHERE wa_id = 'IMG1'`)
	voice := one[int64](t, in, `SELECT media_id FROM messages WHERE wa_id = 'VOI1'`)
	if err := store.SetMediaState(ctx, in.DB, img, store.MediaDownloaded, `393331234567@s.whatsapp.net\IMG1_image.jpg`, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMediaDirectPath(ctx, in.DB, img, "/v/reuploaded"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMediaState(ctx, in.DB, voice, store.MediaDownloaded, `x\VOI1_voice.ogg`, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTranscript(ctx, in.DB, voice, "ci vediamo in ufficio", "it", "parakeet"); err != nil {
		t.Fatal(err)
	}

	if _, err := in.Reingest(ctx, nil); err != nil {
		t.Fatal(err)
	}
	got := one[string](t, in, `SELECT md.state || '|' || md.local_path || '|' || md.direct_path FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = 'IMG1'`)
	if got != `downloaded|393331234567@s.whatsapp.net\IMG1_image.jpg|/v/reuploaded` {
		t.Fatalf("image media after reingest: %q", got)
	}
	if got := one[string](t, in, `SELECT md.transcript FROM messages m JOIN media md ON md.id = m.media_id WHERE m.wa_id = 'VOI1'`); got != "ci vediamo in ufficio" {
		t.Fatalf("transcript after reingest: %q", got)
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'ufficio'`); n != 1 {
		t.Fatal("transcript not searchable after reingest")
	}
	if n := one[int](t, in, `SELECT COUNT(*) FROM media`); n != 2 {
		t.Fatalf("media rows = %d, want the same 2", n)
	}
}

// An interrupted reingest is visible and the next start finishes it.
func TestInterruptedReingestResumes(t *testing.T) {
	in := newIngestor(t)
	ctx := context.Background()
	live(t, in, dmFromMarco("IMG1", t0), imageMsg("la foto", "/v/img"))
	if resumed, err := in.ResumeReingest(ctx, nil); err != nil || resumed {
		t.Fatalf("nothing to resume: %v %v", resumed, err)
	}
	// The process dies right after emptying the derived tables.
	if err := in.DB.Tx(ctx, func(tx *sql.Tx) error { return store.ResetDerived(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.ReingestInterrupted(ctx, in.DB); !ok {
		t.Fatal("interrupted reingest not marked")
	}
	if resumed, err := in.ResumeReingest(ctx, nil); err != nil || !resumed {
		t.Fatalf("resume: %v %v", resumed, err)
	}
	if ok, _ := store.ReingestInterrupted(ctx, in.DB); ok {
		t.Fatal("marker kept after the rebuild")
	}
	if got := one[string](t, in, `SELECT text FROM messages WHERE wa_id = 'IMG1'`); got != "la foto" {
		t.Fatalf("message after resume: %q", got)
	}
}
