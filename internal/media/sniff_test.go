package media

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/childproc"
	"github.com/vukappa4/whatsapp-mcp/internal/config"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

func TestSniffAudio(t *testing.T) {
	for _, tc := range []struct {
		head []byte
		want string
	}{
		{[]byte("OggS\x00\x02"), "ogg"},
		{[]byte("RIFF\x24\x00\x00\x00WAVEfmt "), "wav"},
		{[]byte("\x00\x00\x00\x20ftypM4A \x00\x00"), "mov"},
		{[]byte("#!AMR\n"), "amr"},
		{[]byte{0x1A, 0x45, 0xDF, 0xA3, 0x01}, "matroska"},
		{[]byte("ID3\x04\x00"), "mp3"},
		{[]byte{0xFF, 0xFB, 0x90, 0x00}, "mp3"},
		{[]byte{0xFF, 0xF1, 0x50, 0x80}, "aac"},
		{[]byte("fLaC\x00"), "flac"},
		{[]byte("#EXTM3U\nhttp://example.com/a.mp3\n"), ""},
		{[]byte("ffconcat version 1.0\nfile 'C:/x.ogg'\n"), ""},
		{[]byte("v=0\no=- 0 0 IN IP4 127.0.0.1\n"), ""},
		{[]byte("<?xml version"), ""},
		{nil, ""},
	} {
		if got := childproc.SniffAudio(tc.head); got != tc.want {
			t.Errorf("SniffAudio(%q) = %q, want %q", tc.head, got, tc.want)
		}
	}
}

// playlistServer counts every request a playlist could make ffmpeg send.
func playlistServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func playlist(url string) []byte {
	return []byte(fmt.Sprintf("#EXTM3U\n#EXTINF:10,\n%s/leak.mp3\n", url))
}

// readyCLI is the real engine reporting itself ready: the sniffer runs before
// the engine is ever needed.
type readyCLI struct{ *CLITranscriber }

func (readyCLI) Check() error { return nil }

// A stranger's "audio" that is a playlist never reaches ffmpeg: it is stored
// as .bin, refused by the sniffer and recorded as not transcribed, and no
// request leaves the machine.
func TestPlaylistAudioIsNeverFetched(t *testing.T) {
	srv, hits := playlistServer(t)
	tools := t.TempDir()
	e := newEnv(t, func(o *Options) { o.Transcriber = readyCLI{&CLITranscriber{Choice: EngineParakeet, ToolsDir: tools}} })
	msg, mediaID, _ := e.add(1, "audio", "audio/x-mpegurl", "mix.m3u", "/v/mix.enc?x", 10)
	data := playlist(srv.URL)
	sum := sha256.Sum256(data)
	e.db.Exec(`UPDATE media SET size = ?, file_sha256 = ? WHERE id = ?`, len(data), sum[:], mediaID)
	e.src.set(func(f *fakeSource) { f.files["/v/mix.enc?x"] = data })

	r := e.get(msg, true)
	if r.Path == "" || !strings.HasSuffix(r.Path, "_mix.bin") {
		t.Fatalf("path = %q, want a .bin file", r.Path)
	}
	if !errors.Is(r.TranscriptErr, errUnsupportedAudio) {
		t.Fatalf("transcript error = %v", r.TranscriptErr)
	}
	if j := e.job(msg); j.HasTranscript || j.TranscriptError != errUnsupportedAudio.Error() {
		t.Fatalf("job = %+v", j)
	}
	// The background sweep does not try it again.
	if refs, _ := store.UntranscribedAudio(context.Background(), e.db, 10); len(refs) != 0 {
		t.Fatalf("refused audio queued again: %v", refs)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests left the machine", n)
	}
}

// The real ffmpeg with the daemon's arguments: a playlist renamed to look
// like audio fails to decode and fetches nothing; real Ogg audio decodes.
func TestFFmpegArgsNeverFetch(t *testing.T) {
	ff, err := childproc.FindFFmpeg(nil, config.FFmpegCandidates())
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	srv, hits := playlistServer(t)
	dir := t.TempDir()
	run := func(path, format string, limit time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		cmd := exec.CommandContext(ctx, ff, ffmpegArgs(path, format)...)
		childproc.Hide(cmd)
		cmd.Stdout = nil
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	m3u := filepath.Join(dir, "voice.ogg")
	if err := os.WriteFile(m3u, playlist(srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"ogg", "mp3", "mov", "wav"} {
		if err := run(m3u, format, 30*time.Second); err == nil {
			t.Errorf("a playlist decoded as %s", format)
		}
	}
	// Even a forced playlist demuxer cannot leave the file protocol (ffmpeg
	// keeps retrying the refused URL: a few seconds prove the point).
	_ = run(m3u, "hls", 5*time.Second)
	if n := hits.Load(); n != 0 {
		t.Fatalf("ffmpeg sent %d requests", n)
	}

	ogg := filepath.Join(dir, "tone.ogg")
	gen := exec.Command(ff, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "libopus", ogg)
	childproc.Hide(gen)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate Ogg Opus: %v %s", err, out)
	}
	format, err := audioDemuxer(ogg, true)
	if err != nil || format != "ogg" {
		t.Fatalf("audioDemuxer(ogg) = %q, %v", format, err)
	}
	if err := run(ogg, format, 30*time.Second); err != nil {
		t.Fatalf("real Ogg audio: %v", err)
	}
}
