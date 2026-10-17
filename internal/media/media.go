// Package media downloads, stores and transcribes WhatsApp media files.
//
// New live media under EagerMaxSize is downloaded right after its message is
// committed; history media only on demand. A file the CDN no longer has is
// re-requested from the phone with a media retry receipt (on demand only).
// Voice notes and audio are transcribed one at a time after download.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

const (
	// EagerMaxSize bounds automatic downloads of new live media.
	EagerMaxSize = 50 << 20
	// eagerWindow is how far back the sweep looks for live media never downloaded.
	eagerWindow = 14 * 24 * time.Hour
	sweepEvery  = 10 * time.Minute
)

// errNotConnected means WhatsApp is not connected: nothing was tried.
var errNotConnected = errors.New("WhatsApp is not connected")

// ErrNoMedia means the message does not exist or carries no media.
var ErrNoMedia = errors.New("no media")

// Options configures a Manager.
type Options struct {
	DB          *store.DB
	Root        string // <data dir>\media
	Source      Source
	Transcriber Transcriber // nil: transcription off
	Log         *slog.Logger
	// Workers is the number of parallel downloads (default 3).
	Workers int
	// RetryTimeout bounds the wait for the phone's re-upload answer (default 60 s).
	RetryTimeout time.Duration
	Now          func() time.Time
}

// Manager owns the download and transcription queues.
type Manager struct {
	o           Options
	downloads   *workQueue
	transcripts *workQueue

	retryMu sync.Mutex
	retries map[string]chan *events.MediaRetry // by retryKey

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a Manager; Start runs it.
func New(o Options) *Manager {
	if o.Workers <= 0 {
		o.Workers = 3
	}
	if o.RetryTimeout <= 0 {
		o.RetryTimeout = 60 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &Manager{o: o, downloads: newQueue(2000), transcripts: newQueue(2000), retries: map[string]chan *events.MediaRetry{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

// Start runs the download workers, the transcription worker and the sweeper.
func (m *Manager) Start() {
	if err := os.MkdirAll(m.o.Root, 0o700); err != nil {
		m.o.Log.Error("create media dir", "dir", m.o.Root, "err", err)
	}
	for range m.o.Workers {
		m.loop(m.downloads, m.runDownload)
	}
	m.loop(m.transcripts, m.runTranscribe)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.sweeper()
	}()
}

// Stop cancels running work and waits for the workers.
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) loop(q *workQueue, run func(*task) error) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			t, ok := q.next(m.ctx)
			if !ok {
				return
			}
			q.finish(t, run(t))
		}
	}()
}

// LiveMedia queues the eager download of a live media message that was just
// committed. It never blocks.
func (m *Manager) LiveMedia(messageID, mediaID int64) {
	if _, ok := m.downloads.submit(mediaID, messageID, false); !ok {
		m.o.Log.Warn("media download queue full; the sweep retries later", "message", messageID)
	}
}

// Re-upload requests per media file: at most reuploadMax in reuploadWindow,
// persisted, so neither retries nor restarts flood the phone.
const (
	reuploadMax    = 2
	reuploadWindow = 24 * time.Hour
)

// retryKey names a re-upload request: the chat and, in groups, the sender of
// the message, and its WhatsApp id. WhatsApp ids are unique per sender only.
func retryKey(chat, sender types.JID, id string) string {
	s := ""
	if !sender.IsEmpty() {
		s = sender.ToNonAD().String()
	}
	return chat.ToNonAD().String() + "|" + s + "|" + id
}

// MediaRetry hands the phone's answer to a re-upload request to the download
// waiting for it: the one with the same chat, sender and id, else the only
// one waiting for that id.
func (m *Manager) MediaRetry(evt *events.MediaRetry) {
	m.retryMu.Lock()
	ch := m.retries[retryKey(evt.ChatID, evt.SenderID, evt.MessageID)]
	if ch == nil {
		var only chan *events.MediaRetry
		n := 0
		for k, c := range m.retries {
			if strings.HasSuffix(k, "|"+evt.MessageID) {
				only, n = c, n+1
			}
		}
		if n == 1 {
			ch = only
		}
	}
	m.retryMu.Unlock()
	if ch == nil {
		m.o.Log.Warn("media retry answer matches no waiting download", "message", evt.MessageID, "chat", evt.ChatID)
		return
	}
	select {
	case ch <- evt:
	default:
	}
}

// sweeper queues live media that were never downloaded and audio never
// transcribed: at start, when WhatsApp connects, and every sweepEvery.
func (m *Manager) sweeper() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	connected := false
	last := time.Time{}
	for {
		now := m.o.Now()
		c := m.o.Source != nil && m.o.Source.Connected()
		if (c && !connected) || now.Sub(last) >= sweepEvery {
			m.sweep()
			last = now
		}
		connected = c
		select {
		case <-m.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (m *Manager) sweep() {
	ctx := m.ctx
	refs, err := store.PendingLiveMedia(ctx, m.o.DB, m.o.Now().Add(-eagerWindow).Unix(), EagerMaxSize, 200)
	if err != nil && ctx.Err() == nil {
		m.o.Log.Warn("sweep pending media", "err", err)
	}
	for _, r := range refs {
		m.downloads.submit(r.MediaID, r.MessageID, false)
	}
	if m.o.Transcriber == nil || m.o.Transcriber.Check() != nil {
		return
	}
	refs, err = store.UntranscribedAudio(ctx, m.o.DB, 200)
	if err != nil && ctx.Err() == nil {
		m.o.Log.Warn("sweep untranscribed audio", "err", err)
	}
	for _, r := range refs {
		m.transcripts.submit(r.MediaID, r.MessageID, false)
	}
}

func isAudio(kind string) bool { return kind == "voice" || kind == "audio" }

func (m *Manager) setState(ctx context.Context, mediaID int64, state, rel, errText string) {
	err := store.Retry(ctx, func() error { return store.SetMediaState(ctx, m.o.DB, mediaID, state, rel, errText) })
	if err != nil {
		m.o.Log.Error("record media state", "media", mediaID, "state", state, "err", err)
	}
}

// failure is a download outcome to record: the state and why.
type failure struct {
	state string
	err   error
}

func (f *failure) Error() string { return f.err.Error() }
func (f *failure) Unwrap() error { return f.err }

func fail(state string, format string, args ...any) *failure {
	return &failure{state: state, err: fmt.Errorf(format, args...)}
}

// runDownload is one download task. Background tasks only download pending
// media (or failed fewer than store.MaxEagerAttempts times) under
// EagerMaxSize while connected, and never ask the phone to re-upload;
// on-demand tasks retry any state but unavailable.
func (m *Manager) runDownload(t *task) error {
	ctx := m.ctx
	j, ok, err := store.LoadMediaJobByMedia(ctx, m.o.DB, t.mediaID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoMedia
	}
	retryable := j.State == store.MediaPending || (j.State == store.MediaFailed && j.Attempts < store.MaxEagerAttempts)
	if !t.high && (!retryable || j.Size >= EagerMaxSize) {
		return nil
	}
	start := time.Now()
	err = m.download(ctx, j, t.high)
	var f *failure
	switch {
	case err == nil:
		m.o.Log.Info("media downloaded", "message", j.MessageID, "kind", j.Kind, "bytes", j.Size, "took", time.Since(start).Round(time.Millisecond))
		if isAudio(j.Kind) {
			m.transcripts.submit(j.MediaID, j.MessageID, false)
		}
	case errors.As(err, &f):
		m.setState(context.WithoutCancel(ctx), j.MediaID, f.state, "", f.err.Error())
		m.o.Log.Warn("media download failed", "message", j.MessageID, "state", f.state, "err", f.err)
	}
	return err
}

// download fetches one file. It returns nil once the file is stored, a
// *failure to record, or another error that leaves the state as it is
// (not connected, shutting down). The file streams to <file>.part on disk,
// never whole into memory, and is renamed once verified.
func (m *Manager) download(ctx context.Context, j store.MediaJob, onDemand bool) error {
	if j.State == store.MediaDownloaded && j.LocalPath != "" {
		if abs, err := resolve(m.o.Root, j.LocalPath); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return nil
			}
		}
		if err := store.ResetMediaFile(ctx, m.o.DB, j.MediaID); err != nil {
			return err
		}
	}
	rel, err := relPath(j.ChatJID, j.WAID, j.Kind, j.Filename, j.Mimetype)
	if err != nil {
		return fail(store.MediaFailed, "%v", err)
	}
	abs, err := resolve(m.o.Root, rel)
	if err != nil {
		return fail(store.MediaFailed, "%v", err)
	}
	// A file already in place (downloaded before a reingest) is adopted when it verifies.
	if fileMatches(abs, j) {
		m.adopt(ctx, j, rel, abs)
		return nil
	}
	if old, err := uncappedRelPath(j.ChatJID, j.WAID, j.Kind, j.Filename, j.Mimetype); err == nil && old != rel {
		if oldAbs, err := resolve(m.o.Root, old); err == nil && fileMatches(oldAbs, j) {
			m.adopt(ctx, j, old, oldAbs)
			return nil
		}
	}
	mt, mms, ok := mediaType(j.Kind)
	if !ok {
		return fail(store.MediaUnavailable, "a %s message has no downloadable file", j.Kind)
	}
	if len(j.MediaKey) == 0 {
		// A later copy of the message (history, redelivery) may still bring the key.
		return fail(store.MediaFailed, "no media key stored: the file cannot be decrypted yet")
	}
	if m.o.Source == nil || !m.o.Source.Connected() {
		return errNotConnected
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return fail(store.MediaFailed, "create folder: %v", err)
	}
	part := abs + ".part"
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fail(store.MediaFailed, "create file: %v", err)
	}
	stored := false
	defer func() {
		f.Close()
		if !stored {
			os.Remove(part)
		}
	}()
	d := &Downloadable{DirectPath: directPathOf(j.DirectPath, j.URL), MediaKey: j.MediaKey, SHA256: j.SHA256,
		EncSHA256: j.EncSHA256, Length: uint64(max(j.Size, 0)), Type: mt}
	if d.DirectPath == "" {
		err = whatsmeow.ErrNoURLPresent
	} else {
		err = fetchInto(ctx, m.o.Source, d, f)
	}
	verified := true
	// A hash mismatch at the stored path asks the phone too: the path can be
	// a re-upload that never verified.
	if goneFromServer(err) || (onDemand && hashMismatch(err)) {
		if !onDemand {
			return fail(store.MediaExpired, "WhatsApp's servers no longer have the file: request it on demand to ask the phone")
		}
		verified, err = m.reupload(ctx, j, d, mms, f)
	}
	if err != nil {
		var fl *failure
		if errors.As(err, &fl) || ctx.Err() != nil || errors.Is(err, errNotConnected) {
			return err
		}
		return fail(store.MediaFailed, "%v", err)
	}
	if verified && len(j.SHA256) == sha256.Size {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fail(store.MediaFailed, "%v", err)
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return fail(store.MediaFailed, "%v", err)
		}
		if !bytes.Equal(h.Sum(nil), j.SHA256) {
			return fail(store.MediaFailed, "downloaded file does not match its SHA-256")
		}
	}
	if fi, err := f.Stat(); err == nil && j.Size > 0 && fi.Size() != j.Size {
		m.o.Log.Warn("media size differs from the message", "message", j.MessageID, "got", fi.Size(), "want", j.Size, "hash_checked", verified)
	}
	if err := f.Close(); err != nil {
		return fail(store.MediaFailed, "save file: %v", err)
	}
	if err := os.Rename(part, abs); err != nil {
		return fail(store.MediaFailed, "save file: %v", err)
	}
	stored = true
	m.setState(context.WithoutCancel(ctx), j.MediaID, store.MediaDownloaded, rel, "")
	return nil
}

// fetchInto downloads d into f from the start, replacing anything there.
func fetchInto(ctx context.Context, src Source, d *Downloadable, f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return src.Download(ctx, d, f)
}

// adopt records a file already on disk, with its transcript sidecar if any.
func (m *Manager) adopt(ctx context.Context, j store.MediaJob, rel, abs string) {
	m.setState(context.WithoutCancel(ctx), j.MediaID, store.MediaDownloaded, rel, "")
	if j.HasTranscript {
		return
	}
	if s, ok := readSidecar(abs); ok {
		if err := store.SetTranscript(ctx, m.o.DB, j.MediaID, s.Text, s.Lang, s.Engine); err != nil {
			m.o.Log.Warn("restore transcript", "message", j.MessageID, "err", err)
		}
	}
}

// SaveSent keeps the bytes this device uploaded for a sent message: they go
// where a download of the message would put them (temp file, then rename)
// and the row is downloaded, so the file is served without fetching it back,
// and a rebuilt row adopts it by its SHA-256. The send already happened: an
// error leaves the row as it is, to be downloaded like any other. It holds
// the row in the download queue meanwhile, so a sweep never fetches the file
// back while it is written.
func (m *Manager) SaveSent(ctx context.Context, messageID int64, data []byte) error {
	j, ok, err := store.LoadMediaJob(ctx, m.o.DB, messageID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoMedia
	}
	release, err := m.downloads.claim(ctx, j.MediaID, messageID)
	if err != nil {
		return err
	}
	defer release()
	// A download that ran before the claim may have stored the file already.
	if j, _, err = store.LoadMediaJob(ctx, m.o.DB, messageID); err != nil {
		return err
	}
	if sum := sha256.Sum256(data); len(j.SHA256) == sha256.Size && !bytes.Equal(sum[:], j.SHA256) {
		return errors.New("the uploaded bytes do not match the message's SHA-256")
	}
	rel, err := relPath(j.ChatJID, j.WAID, j.Kind, j.Filename, j.Mimetype)
	if err != nil {
		return err
	}
	abs, err := resolve(m.o.Root, rel)
	if err != nil {
		return err
	}
	if !fileMatches(abs, j) {
		if err := writeFile(abs, data); err != nil {
			return err
		}
	}
	if err := store.Retry(ctx, func() error { return store.SetMediaState(ctx, m.o.DB, j.MediaID, store.MediaDownloaded, rel, "") }); err != nil {
		return err
	}
	if isAudio(j.Kind) {
		m.transcripts.submit(j.MediaID, j.MessageID, false)
	}
	return nil
}

// writeFile writes data to abs through <abs>.part, the temp name a download
// of the same file uses, so abs never holds a partial file and a crash leaves
// no file the next write does not replace.
func writeFile(abs string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	part := abs + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, abs)
	}
	if err != nil {
		os.Remove(part)
	}
	return err
}

// fileMatches reports whether path holds this media: same SHA-256, or the
// same size when no hash is stored.
func fileMatches(path string, j store.MediaJob) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if len(j.SHA256) != sha256.Size {
		fi, err := f.Stat()
		return err == nil && j.Size > 0 && fi.Size() == j.Size
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), j.SHA256)
}

// messageInfo rebuilds what a media retry receipt needs. An incoming DM uses
// the sender's JID: the chat the message actually came from.
func messageInfo(j store.MediaJob) (*types.MessageInfo, error) {
	chat, err := types.ParseJID(j.ChatJID)
	if err != nil {
		return nil, fmt.Errorf("chat %q: %w", j.ChatJID, err)
	}
	info := &types.MessageInfo{ID: j.WAID, MessageSource: types.MessageSource{Chat: chat, IsFromMe: j.FromMe, IsGroup: j.ChatKind == "group"}}
	if j.SenderJID != "" {
		if s, err := types.ParseJID(j.SenderJID); err == nil {
			info.Sender = s
			if j.ChatKind == "dm" && !j.FromMe {
				info.Chat = s
			}
		}
	}
	if info.IsGroup && info.Sender.IsEmpty() {
		return nil, errors.New("the group message has no stored sender")
	}
	return info, nil
}

// maxReencoded bounds a re-encoded re-upload, which is checked on its MAC in memory.
const maxReencoded = 256 << 20

// reupload asks the phone to upload the file again, waits for its answer and
// downloads from the new path into f. verified is false when the re-uploaded
// file differs from the original hashes and was accepted on its MAC.
func (m *Manager) reupload(ctx context.Context, j store.MediaJob, d *Downloadable, mms string, f *os.File) (verified bool, err error) {
	info, err := messageInfo(j)
	if err != nil {
		return false, fail(store.MediaFailed, "cannot ask the phone to re-upload: %v", err)
	}
	ok, next, err := store.TakeReuploadRequest(context.WithoutCancel(ctx), m.o.DB, j.MediaID, m.o.Now(), reuploadMax, reuploadWindow)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fail(store.MediaExpired, "WhatsApp's servers no longer have the file and the phone was already asked %d times in 24 h: ask again after %s",
			reuploadMax, next.Format("2006-01-02 15:04 MST"))
	}
	sender := types.JID{}
	if info.IsGroup {
		sender = info.Sender
	}
	key := retryKey(info.Chat, sender, info.ID)
	ch := make(chan *events.MediaRetry, 1)
	m.retryMu.Lock()
	m.retries[key] = ch
	m.retryMu.Unlock()
	defer func() {
		m.retryMu.Lock()
		if m.retries[key] == ch {
			delete(m.retries, key)
		}
		m.retryMu.Unlock()
	}()
	if err := m.o.Source.RequestReupload(ctx, info, j.MediaKey); err != nil {
		return false, fail(store.MediaFailed, "ask the phone to re-upload: %v", err)
	}
	var evt *events.MediaRetry
	select {
	case evt = <-ch:
	case <-time.After(m.o.RetryTimeout):
		return false, fail(store.MediaExpired, "WhatsApp's servers no longer have the file and the phone did not re-upload it within %s: is the phone online?", m.o.RetryTimeout)
	case <-ctx.Done():
		return false, ctx.Err()
	}
	notif, err := whatsmeow.DecryptMediaRetryNotification(evt, j.MediaKey)
	switch {
	case errors.Is(err, whatsmeow.ErrMediaNotAvailableOnPhone):
		return false, fail(store.MediaUnavailable, "the phone no longer has this file")
	case err != nil:
		return false, fail(store.MediaFailed, "phone answer: %v", err)
	}
	switch notif.GetResult() {
	case waMmsRetry.MediaRetryNotification_SUCCESS:
	case waMmsRetry.MediaRetryNotification_NOT_FOUND:
		return false, fail(store.MediaUnavailable, "the phone no longer has this file")
	default:
		return false, fail(store.MediaFailed, "the phone answered %s", notif.GetResult())
	}
	if d.DirectPath = directPathOf(notif.GetDirectPath(), ""); d.DirectPath == "" {
		return false, fail(store.MediaFailed, "the phone answered without a download path")
	}
	err = fetchInto(ctx, m.o.Source, d, f)
	switch {
	case err == nil:
		m.storeReuploadPath(ctx, j, notif.GetDirectPath())
		return true, nil
	case hashMismatch(err):
		raw, ferr := m.o.Source.FetchEncrypted(ctx, d.DirectPath, mms, maxReencoded)
		if ferr != nil {
			return false, fail(store.MediaFailed, "%v", ferr)
		}
		data, err := decryptWithMAC(raw, j.MediaKey, d.Type)
		if err != nil {
			return false, fail(store.MediaFailed, "%v", err)
		}
		if err := f.Truncate(0); err != nil {
			return false, err
		}
		if _, err := f.WriteAt(data, 0); err != nil {
			return false, fail(store.MediaFailed, "save file: %v", err)
		}
		m.o.Log.Info("accepted re-uploaded media on its MAC; hashes differ from the original", "message", j.MessageID)
		m.storeReuploadPath(ctx, j, notif.GetDirectPath())
		return false, nil
	case goneFromServer(err):
		return false, fail(store.MediaExpired, "the phone re-uploaded the file but the server still refuses it: %v", err)
	}
	return false, err
}

// storeReuploadPath records the path a re-upload was downloaded from: only a
// path that delivered the file replaces the stored one. A failed write only
// costs a phone round trip on the next download, never the file.
func (m *Manager) storeReuploadPath(ctx context.Context, j store.MediaJob, path string) {
	if err := store.SetMediaDirectPath(context.WithoutCancel(ctx), m.o.DB, j.MediaID, path); err != nil {
		m.o.Log.Warn("store re-upload path", "message", j.MessageID, "err", err)
	}
}

// sidecar is a transcript stored next to its audio file, so a reingest that
// rebuilds data.db keeps transcripts without running the engine again.
type sidecar struct {
	Text   string `json:"text"`
	Lang   string `json:"lang,omitempty"`
	Engine string `json:"engine"`
}

func sidecarPath(abs string) string { return abs + ".transcript.json" }

func readSidecar(abs string) (sidecar, bool) {
	var s sidecar
	b, err := os.ReadFile(sidecarPath(abs))
	if err != nil || json.Unmarshal(b, &s) != nil || s.Engine == "" {
		return s, false
	}
	return s, true
}

var errTranscriptionOff = errors.New("transcription is off (run --transcriber off)")

// runTranscribe is one transcription task. Background tasks skip media whose
// transcription already failed; on-demand tasks try again.
func (m *Manager) runTranscribe(t *task) error {
	ctx := m.ctx
	j, ok, err := store.LoadMediaJobByMedia(ctx, m.o.DB, t.mediaID)
	if err != nil {
		return err
	}
	if !ok || !isAudio(j.Kind) {
		return ErrNoMedia
	}
	if j.HasTranscript || (!t.high && j.TranscriptError != "") {
		return nil
	}
	if j.State != store.MediaDownloaded {
		return errors.New("the file is not downloaded")
	}
	abs := m.path(j)
	if abs == "" {
		// The file is gone or its path leaves the media root: the row is
		// pending again and downloads like one never fetched.
		if err := store.ResetMediaFile(ctx, m.o.DB, j.MediaID); err != nil {
			return err
		}
		return errors.New("the file is not downloaded")
	}
	if m.o.Transcriber == nil {
		return errTranscriptionOff
	}
	if err := m.o.Transcriber.Check(); err != nil {
		return err
	}
	start := time.Now()
	text, lang, err := m.o.Transcriber.Transcribe(ctx, abs, j.Kind == "voice", time.Duration(j.Duration)*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		m.o.Log.Warn("transcription failed", "message", j.MessageID, "err", err)
		if serr := store.SetTranscriptError(context.WithoutCancel(ctx), m.o.DB, j.MediaID, err.Error()); serr != nil {
			m.o.Log.Error("record transcription error", "err", serr)
		}
		return err
	}
	engine := m.o.Transcriber.Engine()
	err = store.Retry(ctx, func() error { return store.SetTranscript(ctx, m.o.DB, j.MediaID, text, lang, engine) })
	if err != nil {
		return err
	}
	if b, err := json.Marshal(sidecar{Text: text, Lang: lang, Engine: engine}); err == nil {
		if err := os.WriteFile(sidecarPath(abs), b, 0o600); err != nil {
			m.o.Log.Warn("write transcript sidecar", "err", err)
		}
	}
	m.o.Log.Info("transcribed", "message", j.MessageID, "audio_s", j.Duration, "took", time.Since(start).Round(time.Millisecond), "chars", len([]rune(text)))
	return nil
}

// Request asks Get for one message's media.
type Request struct {
	MessageID      int64
	Transcript     bool          // also transcribe voice notes and audio
	DownloadWait   time.Duration // how long to wait for a download
	TranscriptWait time.Duration // how long to wait for a transcription
}

// Result is what Get could do within its waits.
type Result struct {
	Job  store.MediaJob
	Path string // absolute path once downloaded

	Downloading   bool  // still downloading after the wait
	DownloadErr   error // why there is no file (not connected, expired...)
	Transcribing  bool  // still transcribing after the wait
	TranscriptErr error // why there is no transcript
	RetryIn       time.Duration
}

// Get downloads a message's media if needed and, when asked, transcribes it,
// waiting at most the request's waits. Unavailable media is not retried.
func (m *Manager) Get(ctx context.Context, r Request) (Result, error) {
	var res Result
	j, ok, err := store.LoadMediaJob(ctx, m.o.DB, r.MessageID)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, ErrNoMedia
	}
	if m.path(j) == "" && j.State != store.MediaUnavailable {
		deadline := time.Now().Add(r.DownloadWait)
		t, ok := m.downloads.submit(j.MediaID, j.MessageID, true)
		done := ok && wait(ctx, t, time.Until(deadline))
		if done && !t.high {
			// This request joined a background download, which never asks the
			// phone to re-upload: if it did not get the file, try on demand.
			if again, _, err := store.LoadMediaJob(ctx, m.o.DB, r.MessageID); err == nil && m.path(again) == "" && again.State != store.MediaUnavailable {
				t, _ = m.downloads.submit(j.MediaID, j.MessageID, true)
				done = wait(ctx, t, time.Until(deadline))
			}
		}
		if !ok {
			res.DownloadErr = errors.New("download queue is full")
		} else if done {
			res.DownloadErr = t.err
		} else {
			res.Downloading = true
			res.RetryIn = 10*time.Second + time.Duration(j.Size/(1<<20))*time.Second
		}
		if j, _, err = store.LoadMediaJob(ctx, m.o.DB, r.MessageID); err != nil {
			return res, err
		}
	}
	res.Path = m.path(j)
	if r.Transcript && isAudio(j.Kind) && res.Path != "" && !j.HasTranscript {
		switch {
		case m.o.Transcriber == nil:
			res.TranscriptErr = errTranscriptionOff
		case m.o.Transcriber.Check() != nil:
			res.TranscriptErr = m.o.Transcriber.Check()
		default:
			t, _ := m.transcripts.submit(j.MediaID, j.MessageID, true)
			if t == nil {
				res.TranscriptErr = errors.New("transcription queue is full")
			} else if done := wait(ctx, t, r.TranscriptWait); done {
				res.TranscriptErr = t.err
			} else {
				res.Transcribing = true
				ahead := m.transcripts.position(t)
				res.RetryIn = time.Duration(5+j.Duration/3+int64(ahead)*20) * time.Second
			}
			if j, _, err = store.LoadMediaJob(ctx, m.o.DB, r.MessageID); err != nil {
				return res, err
			}
		}
	}
	res.Job = j
	return res, nil
}

// path is the absolute file of a downloaded media, "" when there is none.
func (m *Manager) path(j store.MediaJob) string {
	if j.State != store.MediaDownloaded || j.LocalPath == "" {
		return ""
	}
	abs, err := resolve(m.o.Root, j.LocalPath)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(abs); err != nil {
		return ""
	}
	return abs
}

func wait(ctx context.Context, t *task, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-t.done:
		return true
	case <-timer.C:
	case <-ctx.Done():
	}
	return false
}

// Status summarizes media for whatsapp_status.
type Status struct {
	Counts          map[string]int
	DownloadQueue   int
	TranscriptQueue int
	Engine          string // "" when transcription is off
	Ready           bool
	Detail          string // why transcription is not ready
	FFmpeg          string // the ffmpeg the transcriber runs, when it reports one
}

// Status counts media per state and reports transcription readiness.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	counts, err := store.MediaStateCounts(ctx, m.o.DB)
	if err != nil {
		return Status{}, err
	}
	s := Status{Counts: counts, DownloadQueue: m.downloads.waiting(), TranscriptQueue: m.transcripts.waiting()}
	if m.o.Transcriber == nil {
		s.Detail = errTranscriptionOff.Error()
		return s, nil
	}
	s.Engine = m.o.Transcriber.Engine()
	if err := m.o.Transcriber.Check(); err != nil {
		s.Detail = err.Error()
	} else {
		s.Ready = true
	}
	if f, ok := m.o.Transcriber.(interface{ FFmpeg() (string, error) }); ok {
		s.FFmpeg, _ = f.FFmpeg()
	}
	return s, nil
}
