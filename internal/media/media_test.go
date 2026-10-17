package media

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/gcmutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	"google.golang.org/protobuf/proto"

	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// fakeSource serves plaintext files by direct path. Paths in gone answer 404;
// a re-upload request is answered by onReupload (nil: no answer).
type fakeSource struct {
	mu         sync.Mutex
	connected  bool
	files      map[string][]byte
	gone       map[string]bool
	encrypted  map[string][]byte // FetchEncrypted answers
	mismatch   map[string]bool   // Download answers a hash error
	downloads  int
	reuploads  []*types.MessageInfo
	onReupload func(info *types.MessageInfo)
}

func newFake() *fakeSource {
	return &fakeSource{connected: true, files: map[string][]byte{}, gone: map[string]bool{},
		encrypted: map[string][]byte{}, mismatch: map[string]bool{}}
}

func (f *fakeSource) set(fn func(*fakeSource)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }

func (f *fakeSource) setReupload(cb func(*types.MessageInfo)) {
	f.set(func(f *fakeSource) { f.onReupload = cb })
}

func (f *fakeSource) downloadCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.downloads }

func (f *fakeSource) reuploadCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reuploads) }

func (f *fakeSource) firstReupload() *types.MessageInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reuploads[0]
}

func (f *fakeSource) Connected() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connected }

// Download writes whatever it serves, unverified: the manager's own hash
// check must catch a wrong file.
func (f *fakeSource) Download(_ context.Context, d *Downloadable, out *os.File) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads++
	switch {
	case f.gone[d.DirectPath]:
		return whatsmeow.ErrMediaDownloadFailedWith404
	case f.mismatch[d.DirectPath]:
		return whatsmeow.ErrInvalidMediaSHA256
	}
	data, ok := f.files[d.DirectPath]
	if !ok {
		return errors.New("no such file")
	}
	_, err := out.Write(data)
	return err
}

func (f *fakeSource) RequestReupload(_ context.Context, info *types.MessageInfo, _ []byte) error {
	f.mu.Lock()
	f.reuploads = append(f.reuploads, info)
	cb := f.onReupload
	f.mu.Unlock()
	if cb != nil {
		go cb(info)
	}
	return nil
}

func (f *fakeSource) FetchEncrypted(_ context.Context, directPath, _ string, _ int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.encrypted[directPath]; ok {
		return b, nil
	}
	return nil, errors.New("no such encrypted file")
}

// retryAnswer builds the phone's encrypted answer to a re-upload request.
func retryAnswer(t *testing.T, waID string, key []byte, result waMmsRetry.MediaRetryNotification_ResultType, path string) *events.MediaRetry {
	t.Helper()
	pt, err := proto.Marshal(&waMmsRetry.MediaRetryNotification{StanzaID: proto.String(waID), DirectPath: proto.String(path), Result: result.Enum()})
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, 12)
	ct, err := gcmutil.Encrypt(hkdfutil.SHA256(key, nil, []byte("WhatsApp Media Retry Notification"), 32), iv, pt, []byte(waID))
	if err != nil {
		t.Fatal(err)
	}
	return &events.MediaRetry{MessageID: waID, Ciphertext: ct, IV: iv}
}

// encryptMedia encrypts plaintext the way WhatsApp's CDN serves it.
func encryptMedia(t *testing.T, plain, key []byte, mt whatsmeow.MediaType) []byte {
	t.Helper()
	keys := hkdfutil.SHA256(key, nil, []byte(mt), 112)
	iv, cipherKey, macKey := keys[:16], keys[16:48], keys[48:80]
	ct, err := cbcutil.Encrypt(cipherKey, iv, plain)
	if err != nil {
		t.Fatal(err)
	}
	h := hmac.New(sha256.New, macKey)
	h.Write(iv)
	h.Write(ct)
	return append(ct, h.Sum(nil)[:10]...)
}

type fakeTranscriber struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeTranscriber) setErr(err error) { f.mu.Lock(); defer f.mu.Unlock(); f.err = err }

func (f *fakeTranscriber) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func (f *fakeTranscriber) Engine() string { return "fake" }
func (f *fakeTranscriber) Check() error   { return nil }
func (f *fakeTranscriber) Transcribe(_ context.Context, path string, _ bool, _ time.Duration) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", "", f.err
	}
	if _, err := os.Stat(path); err != nil {
		return "", "", err
	}
	return "ci vediamo domani in ufficio", "it", nil
}

type env struct {
	t    *testing.T
	db   *store.DB
	src  *fakeSource
	tr   *fakeTranscriber
	m    *Manager
	root string
	n    int
}

// newEnv starts a manager on a fresh database; opts adjust its options
// before it starts.
func newEnv(t *testing.T, opts ...func(*Options)) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO chats(id, jid, kind, created_at) VALUES (1, '393331111111@s.whatsapp.net', 'dm', 0),
		(2, '120363000000000009@g.us', 'group', 0)`); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, src: newFake(), tr: &fakeTranscriber{}, root: filepath.Join(t.TempDir(), "media")}
	o := Options{DB: db, Root: e.root, Source: e.src, Transcriber: e.tr, RetryTimeout: time.Second}
	for _, f := range opts {
		f(&o)
	}
	e.m = New(o)
	e.m.Start()
	t.Cleanup(e.m.Stop)
	return e
}

// add stores a media message whose file is served at path; it returns the
// message id, media id and plaintext.
func (e *env) add(chat int64, kind, mimetype, filename, path string, size int) (int64, int64, []byte) {
	e.t.Helper()
	e.n++
	data := []byte(strings.Repeat(kind, size/len(kind)+1)[:size])
	sum := sha256.Sum256(data)
	key := sha256.Sum256([]byte{byte(e.n)})
	res, err := e.db.Exec(`INSERT INTO media(mimetype, filename, size, duration, direct_path, media_key, file_sha256, file_enc_sha256)
		VALUES (?, ?, ?, 42, ?, ?, ?, ?)`, mimetype, filename, len(data), path, key[:], sum[:], sum[:])
	if err != nil {
		e.t.Fatal(err)
	}
	mediaID, _ := res.LastInsertId()
	sender := "393331111111@s.whatsapp.net"
	res, err = e.db.Exec(`INSERT INTO messages(chat_id, wa_id, sender_jid, ts, kind, media_id, source) VALUES (?, ?, ?, ?, ?, ?, 'live')`,
		chat, "3EB0"+strings.Repeat("A", e.n), sender, time.Now().Unix(), kind, mediaID)
	if err != nil {
		e.t.Fatal(err)
	}
	msgID, _ := res.LastInsertId()
	e.src.set(func(f *fakeSource) { f.files[path] = data })
	return msgID, mediaID, data
}

func (e *env) get(msgID int64, transcript bool) Result {
	e.t.Helper()
	r, err := e.m.Get(context.Background(), Request{MessageID: msgID, Transcript: transcript, DownloadWait: 5 * time.Second, TranscriptWait: 5 * time.Second})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) job(msgID int64) store.MediaJob {
	e.t.Helper()
	j, ok, err := store.LoadMediaJob(context.Background(), e.db, msgID)
	if err != nil || !ok {
		e.t.Fatalf("load job %d: %v %v", msgID, ok, err)
	}
	return j
}

func TestGetDownloadsVerifiesAndTranscribes(t *testing.T) {
	e := newEnv(t)
	msg, _, data := e.add(1, "voice", "audio/ogg; codecs=opus", "", "/v/voice.enc?ccb=1", 3000)
	r := e.get(msg, true)
	if r.DownloadErr != nil || r.TranscriptErr != nil || r.Path == "" {
		t.Fatalf("result = %+v", r)
	}
	want := filepath.Join(e.root, "393331111111@s.whatsapp.net", "3EB0A_voice.ogg")
	if r.Path != want {
		t.Fatalf("path = %s, want %s", r.Path, want)
	}
	got, err := os.ReadFile(r.Path)
	if err != nil || string(got) != string(data) {
		t.Fatalf("file content differs: %v", err)
	}
	j := r.Job
	if j.State != store.MediaDownloaded || j.Transcript != "ci vediamo domani in ufficio" || j.TranscriptLang != "it" || j.TranscriptEngine != "fake" {
		t.Fatalf("job = %+v", j)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'ufficio'`).Scan(&n)
	if n != 1 {
		t.Fatal("transcript not indexed")
	}
	if _, ok := readSidecar(r.Path); !ok {
		t.Fatal("no transcript sidecar")
	}
	// A second request serves the stored file without downloading again.
	before := e.src.downloadCount()
	if r := e.get(msg, true); r.Path != want || e.src.downloadCount() != before || e.tr.callCount() != 1 {
		t.Fatalf("second get downloaded or transcribed again: %+v", r)
	}
}

func TestEagerLiveDownloadAndLimits(t *testing.T) {
	e := newEnv(t)
	small, smallMedia, _ := e.add(2, "image", "image/jpeg", "", "/v/img.enc?x=1", 100)
	big, bigMedia, _ := e.add(2, "video", "video/mp4", "", "/v/big.enc?x=1", 10)
	if _, err := e.db.Exec(`UPDATE media SET size = ? WHERE id = ?`, EagerMaxSize, bigMedia); err != nil {
		t.Fatal(err)
	}
	e.m.LiveMedia(small, smallMedia)
	e.m.LiveMedia(big, bigMedia)
	deadline := time.Now().Add(5 * time.Second)
	for e.job(small).State != store.MediaDownloaded && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if j := e.job(small); j.State != store.MediaDownloaded || !strings.HasPrefix(j.LocalPath, "120363000000000009@g.us"+string(filepath.Separator)) {
		t.Fatalf("eager small image: %+v", j)
	}
	time.Sleep(100 * time.Millisecond)
	if j := e.job(big); j.State != store.MediaPending {
		t.Fatalf("a %d-byte video must wait for an on-demand request, state %s", EagerMaxSize, j.State)
	}
}

func TestRetryFlowUpdatesDirectPath(t *testing.T) {
	e := newEnv(t)
	msg, _, data := e.add(2, "document", "application/pdf", "Offerta finale.pdf", "/v/old.enc?x=1", 500)
	j := e.job(msg)
	e.src.set(func(f *fakeSource) { f.gone["/v/old.enc?x=1"] = true })
	e.src.set(func(f *fakeSource) { f.files["/v/new.enc?x=2"] = data })
	e.src.setReupload(func(info *types.MessageInfo) {
		e.m.MediaRetry(retryAnswer(t, info.ID, j.MediaKey, waMmsRetry.MediaRetryNotification_SUCCESS, "/v/new.enc?x=2"))
	})
	r := e.get(msg, false)
	if r.DownloadErr != nil || !strings.HasSuffix(r.Path, "3EB0A_Offerta finale.pdf") {
		t.Fatalf("result = %+v", r)
	}
	if r.Job.DirectPath != "/v/new.enc?x=2" {
		t.Fatalf("direct path not updated: %q", r.Job.DirectPath)
	}
	info := e.src.firstReupload()
	if info.ID != j.WAID || !info.IsGroup || info.Sender.String() != "393331111111@s.whatsapp.net" || info.Chat.String() != "120363000000000009@g.us" {
		t.Fatalf("retry receipt info = %+v", info)
	}
}

func TestRetryOutcomes(t *testing.T) {
	e := newEnv(t)

	// The phone no longer has the file: final, never retried.
	gone, _, _ := e.add(1, "image", "image/jpeg", "", "/v/a.enc?x", 50)
	e.src.set(func(f *fakeSource) { f.gone["/v/a.enc?x"] = true })
	e.src.setReupload(func(info *types.MessageInfo) {
		e.m.MediaRetry(&events.MediaRetry{MessageID: info.ID, Error: &events.MediaRetryError{Code: 2}})
	})
	if r := e.get(gone, false); r.Job.State != store.MediaUnavailable || !strings.Contains(r.Job.Error, "no longer has") {
		t.Fatalf("not on phone: %+v", r.Job)
	}
	calls := e.src.downloadCount()
	if r := e.get(gone, false); r.Job.State != store.MediaUnavailable || e.src.downloadCount() != calls {
		t.Fatal("unavailable media must not be downloaded again")
	}

	// No answer from the phone: expired, and an on-demand request tries again.
	e.src.setReupload(nil)
	silent, _, _ := e.add(1, "image", "image/jpeg", "", "/v/b.enc?x", 50)
	e.src.set(func(f *fakeSource) { f.gone["/v/b.enc?x"] = true })
	if r := e.get(silent, false); r.Job.State != store.MediaExpired || !strings.Contains(r.Job.Error, "phone online") {
		t.Fatalf("no answer: %+v", r.Job)
	}

	// The re-uploaded copy differs from the original hashes: accepted on its MAC.
	reenc, _, data := e.add(1, "image", "image/jpeg", "", "/v/c.enc?x", 64)
	j := e.job(reenc)
	e.src.set(func(f *fakeSource) { f.gone["/v/c.enc?x"] = true })
	e.src.set(func(f *fakeSource) { f.mismatch["/v/c2.enc?x"] = true })
	e.src.set(func(f *fakeSource) {
		f.encrypted["/v/c2.enc?x"] = encryptMedia(t, data, j.MediaKey, whatsmeow.MediaImage)
	})
	e.src.setReupload(func(info *types.MessageInfo) {
		e.m.MediaRetry(retryAnswer(t, info.ID, j.MediaKey, waMmsRetry.MediaRetryNotification_SUCCESS, "/v/c2.enc?x"))
	})
	if r := e.get(reenc, false); r.Job.State != store.MediaDownloaded {
		t.Fatalf("re-encoded re-upload: %+v", r.Job)
	}

	// Same, but encrypted with a key the phone does not share: failed, the
	// stored path stays the original one, and the next request asks again.
	other, _, data2 := e.add(1, "image", "image/jpeg", "", "/v/d.enc?x", 64)
	j2 := e.job(other)
	e.src.set(func(f *fakeSource) { f.gone["/v/d.enc?x"] = true })
	e.src.set(func(f *fakeSource) { f.mismatch["/v/d2.enc?x"] = true })
	e.src.set(func(f *fakeSource) {
		f.encrypted["/v/d2.enc?x"] = encryptMedia(t, data2, make([]byte, 32), whatsmeow.MediaImage)
	})
	answer := "/v/d2.enc?x"
	e.src.setReupload(func(info *types.MessageInfo) {
		e.m.MediaRetry(retryAnswer(t, info.ID, j2.MediaKey, waMmsRetry.MediaRetryNotification_SUCCESS, answer))
	})
	if r := e.get(other, false); r.Job.State != store.MediaFailed || !strings.Contains(r.Job.Error, "key it does not share") ||
		r.Job.DirectPath != "/v/d.enc?x" {
		t.Fatalf("foreign key re-upload: %+v", r.Job)
	}
	asked := e.src.reuploadCount()
	answer = "/v/d3.enc?x"
	e.src.set(func(f *fakeSource) { f.files["/v/d3.enc?x"] = data2 })
	if r := e.get(other, false); r.Job.State != store.MediaDownloaded || e.src.reuploadCount() != asked+1 || r.Job.DirectPath != "/v/d3.enc?x" {
		t.Fatalf("second request after a foreign key re-upload: %+v, re-upload requests %d -> %d", r.Job, asked, e.src.reuploadCount())
	}

	// A stored path whose file fails the hashes asks the phone on demand.
	stale, _, data3 := e.add(1, "image", "image/jpeg", "", "/v/f.enc?x", 64)
	j3 := e.job(stale)
	e.src.set(func(f *fakeSource) {
		f.mismatch["/v/f.enc?x"] = true
		f.files["/v/f2.enc?x"] = data3
	})
	e.src.setReupload(func(info *types.MessageInfo) {
		e.m.MediaRetry(retryAnswer(t, info.ID, j3.MediaKey, waMmsRetry.MediaRetryNotification_SUCCESS, "/v/f2.enc?x"))
	})
	if r := e.get(stale, false); r.Job.State != store.MediaDownloaded {
		t.Fatalf("hash mismatch at the stored path: %+v", r.Job)
	}

	// Background downloads never ask the phone: a 404 is recorded as expired.
	bg, bgMedia, _ := e.add(1, "image", "image/jpeg", "", "/v/e.enc?x", 50)
	e.src.set(func(f *fakeSource) { f.gone["/v/e.enc?x"] = true })
	n := e.src.reuploadCount()
	e.m.LiveMedia(bg, bgMedia)
	deadline := time.Now().Add(5 * time.Second)
	for e.job(bg).State == store.MediaPending && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if e.job(bg).State != store.MediaExpired || e.src.reuploadCount() != n {
		t.Fatalf("background 404: state %s, re-upload requests %d -> %d", e.job(bg).State, n, e.src.reuploadCount())
	}
}

func TestDownloadStates(t *testing.T) {
	e := newEnv(t)

	// Not connected: nothing is tried, the state stays pending.
	e.src.set(func(f *fakeSource) { f.connected = false })
	msg, _, _ := e.add(1, "image", "image/png", "", "/v/p.enc?x", 10)
	if r := e.get(msg, false); !errors.Is(r.DownloadErr, errNotConnected) || r.Job.State != store.MediaPending {
		t.Fatalf("offline: %+v", r)
	}
	e.src.set(func(f *fakeSource) { f.connected = true })

	// A hash mismatch fails; the next on-demand request tries again.
	bad, _, _ := e.add(1, "image", "image/png", "", "/v/bad.enc?x", 10)
	e.src.set(func(f *fakeSource) { f.files["/v/bad.enc?x"] = []byte("tampered!!") })
	if r := e.get(bad, false); r.Job.State != store.MediaFailed || r.Path != "" {
		t.Fatalf("hash mismatch: %+v", r.Job)
	}

	// No media key: can never be decrypted.
	nokey, nokeyMedia, _ := e.add(1, "image", "image/png", "", "/v/k.enc?x", 10)
	e.db.Exec(`UPDATE media SET media_key = NULL WHERE id = ?`, nokeyMedia)
	if r := e.get(nokey, false); r.Job.State != store.MediaFailed || !strings.Contains(r.Job.Error, "no media key") {
		t.Fatalf("no key: %+v", r.Job)
	}

	// A deleted file is downloaded again.
	r := e.get(msg, false)
	if r.Path == "" {
		t.Fatalf("download after reconnect: %+v", r)
	}
	os.Remove(r.Path)
	if r2 := e.get(msg, false); r2.Path != r.Path || r2.Job.State != store.MediaDownloaded {
		t.Fatalf("missing file not restored: %+v", r2)
	}
}

// After a reingest the media rows are new: files already on disk are adopted
// without a download, and their transcripts come back from the sidecar.
func TestAdoptsFilesAfterReingest(t *testing.T) {
	e := newEnv(t)
	msg, mediaID, _ := e.add(1, "voice", "audio/ogg", "", "/v/v.enc?x", 300)
	r := e.get(msg, true)
	if r.Job.Transcript == "" {
		t.Fatalf("first transcription: %+v", r)
	}
	if _, err := e.db.Exec(`UPDATE media SET state = 'pending', local_path = '', transcript = NULL WHERE id = ?`, mediaID); err != nil {
		t.Fatal(err)
	}
	downloads, calls := e.src.downloadCount(), e.tr.callCount()
	e.src.set(func(f *fakeSource) { f.connected = false })
	r = e.get(msg, true)
	if r.Path == "" || r.Job.Transcript != "ci vediamo domani in ufficio" || e.src.downloadCount() != downloads || e.tr.callCount() != calls {
		t.Fatalf("adoption: %+v (downloads %d->%d, transcriptions %d->%d)", r, downloads, e.src.downloadCount(), calls, e.tr.callCount())
	}
}

// Windows takes names of up to 255 UTF-16 units, longer in bytes than
// relPath now cuts them: a file stored under its uncapped name is adopted
// there instead of being downloaded again.
func TestAdoptsUncappedName(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows stores names over 255 bytes")
	}
	e := newEnv(t)
	name := strings.Repeat("報", 80) + ".pdf"
	msg, mediaID, _ := e.add(1, "document", "application/pdf", name, "/v/long.enc?x", 64)
	r := e.get(msg, false)
	if r.Path == "" {
		t.Fatalf("first download: %+v", r)
	}
	j := e.job(msg)
	old, err := uncappedRelPath(j.ChatJID, j.WAID, j.Kind, j.Filename, j.Mimetype)
	if err != nil || filepath.Base(old) == filepath.Base(r.Path) {
		t.Fatalf("uncapped name %q, %v: same as %q", old, err, r.Path)
	}
	oldAbs := filepath.Join(filepath.Dir(r.Path), filepath.Base(old))
	if err := os.Rename(r.Path, oldAbs); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`UPDATE media SET state = 'pending', local_path = '' WHERE id = ?`, mediaID); err != nil {
		t.Fatal(err)
	}
	downloads := e.src.downloadCount()
	e.src.set(func(f *fakeSource) { f.connected = false })
	if r = e.get(msg, false); r.Path != oldAbs || e.src.downloadCount() != downloads {
		t.Fatalf("adoption: %+v (downloads %d->%d)", r, downloads, e.src.downloadCount())
	}
}

// A sent file is kept as uploaded and served offline, transcript included.
// Bytes that are not the message's file, or a file that cannot be written,
// leave the row pending and no temp file behind.
func TestSaveSent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.src.set(func(f *fakeSource) { f.connected = false })
	voice, _, data := e.add(1, "voice", "audio/ogg; codecs=opus", "", "/v/sent.enc?x", 300)
	if err := e.m.SaveSent(ctx, voice, []byte("not the uploaded file")); err == nil || e.job(voice).State != store.MediaPending {
		t.Fatalf("wrong bytes: err %v, state %s", err, e.job(voice).State)
	}
	if err := e.m.SaveSent(ctx, voice, data); err != nil {
		t.Fatal(err)
	}
	r := e.get(voice, true)
	if got, err := os.ReadFile(r.Path); err != nil || string(got) != string(data) || !strings.HasPrefix(r.Path, e.root) ||
		r.Job.Transcript != "ci vediamo domani in ufficio" || e.src.downloadCount() != 0 {
		t.Fatalf("served %+v (read err %v, downloads %d)", r, err, e.src.downloadCount())
	}

	doc, _, docData := e.add(1, "document", "application/pdf", "offerta.pdf", "/v/doc.enc?x", 64)
	j := e.job(doc)
	rel, err := relPath(j.ChatJID, j.WAID, j.Kind, j.Filename, j.Mimetype)
	if err != nil {
		t.Fatal(err)
	}
	// A folder where the file belongs: the rename cannot replace it.
	if err := os.MkdirAll(filepath.Join(e.root, rel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.m.SaveSent(ctx, doc, docData); err == nil || e.job(doc).State != store.MediaPending || e.job(doc).LocalPath != "" {
		t.Fatalf("unwritable: err %v, job %+v", err, e.job(doc))
	}
	if left, _ := filepath.Glob(filepath.Join(e.root, filepath.Dir(rel), "*.part")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// While SaveSent writes a sent file, the row is held in the download queue:
// a sweep's download joins it instead of fetching the file back, and a
// download queued before it is taken over.
func TestSaveSentHoldsTheRow(t *testing.T) {
	ctx := context.Background()
	q := newQueue(10) // no workers: a submitted task stays queued
	queued, _ := q.submit(7, 70, false)
	release, err := q.claim(ctx, 7, 70)
	if err != nil {
		t.Fatal(err)
	}
	if joined, _ := q.submit(7, 70, true); joined != queued || q.waiting() != 0 {
		t.Fatalf("submit during the claim: %p (claimed %p), %d waiting", joined, queued, q.waiting())
	}
	// A second claim waits for the first.
	second := make(chan struct{})
	go func() {
		if r, err := q.claim(ctx, 7, 70); err == nil {
			r()
		}
		close(second)
	}()
	select {
	case <-second:
		t.Fatal("two claims on one row at once")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-queued.done
	<-second

	e := newEnv(t)
	msg, _, data := e.add(1, "document", "application/pdf", "offerta.pdf", "/v/held.enc?x", 64)
	if err := e.m.SaveSent(ctx, msg, data); err != nil || e.job(msg).State != store.MediaDownloaded || e.src.downloadCount() != 0 {
		t.Fatalf("save after release: err %v, state %s, downloads %d", err, e.job(msg).State, e.src.downloadCount())
	}
}

// A downloaded row whose path leaves the media root, or whose file is gone,
// is pending again when transcription reaches it: the engine never runs.
func TestTranscriptionResetsRowWithoutFile(t *testing.T) {
	e := newEnv(t)
	e.src.set(func(f *fakeSource) { f.connected = false })
	outside := filepath.Join(t.TempDir(), "memo.ogg")
	if err := os.WriteFile(outside, []byte("OggS"), 0o600); err != nil {
		t.Fatal(err)
	}
	abs, absID, _ := e.add(1, "voice", "audio/ogg", "", "/v/abs.enc?x", 300)
	gone, goneID, _ := e.add(1, "voice", "audio/ogg", "", "/v/gone.enc?x", 300)
	for id, path := range map[int64]string{absID: outside, goneID: "393331111111@s.whatsapp.net" + string(filepath.Separator) + "missing.ogg"} {
		if _, err := e.db.Exec(`UPDATE media SET state = 'downloaded', local_path = ? WHERE id = ?`, path, id); err != nil {
			t.Fatal(err)
		}
	}
	e.m.sweep()
	deadline := time.Now().Add(5 * time.Second)
	for (e.job(abs).State != store.MediaPending || e.job(gone).State != store.MediaPending) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for _, msg := range []int64{abs, gone} {
		if j := e.job(msg); j.State != store.MediaPending || j.LocalPath != "" || j.TranscriptError != "" {
			t.Fatalf("message %d: %+v", msg, j)
		}
	}
	if n := e.tr.callCount(); n != 0 {
		t.Fatalf("transcriber ran %d times", n)
	}
}

func TestTranscriptionErrorsAreRecorded(t *testing.T) {
	e := newEnv(t)
	e.tr.setErr(errors.New("engine crashed"))
	msg, _, _ := e.add(1, "voice", "audio/ogg", "", "/v/t.enc?x", 300)
	r := e.get(msg, true)
	if r.TranscriptErr == nil || r.Job.TranscriptError != "engine crashed" || r.Job.HasTranscript {
		t.Fatalf("failed transcription: %+v", r)
	}
	// An on-demand request tries again.
	e.tr.setErr(nil)
	if r := e.get(msg, true); r.Job.Transcript == "" || r.Job.TranscriptError != "" {
		t.Fatalf("retry: %+v", r.Job)
	}
}

func TestPathSafety(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	bad := []string{`/etc/passwd`, ``, `a/../../b`, `../x`}
	if runtime.GOOS == "windows" {
		// Outside Windows these are ordinary file names: '\' and ':' are no separators there.
		bad = append(bad, `..\x`, `a\..\..\x`, `C:\Windows\x`, `C:x`, `\\server\share\x`, `NUL`)
	}
	for _, rel := range bad {
		if _, err := resolve(root, rel); err == nil {
			t.Errorf("resolve(%q) accepted", rel)
		}
	}
	for _, tc := range []struct{ chat, id, kind, name, mime, want string }{
		{"393331111111@s.whatsapp.net", "3EB0ABC", "document", `..\..\evil.pdf`, "application/pdf", `393331111111@s.whatsapp.net\3EB0ABC__.._evil.pdf`},
		{"120363@g.us", "ID1", "document", "CON.txt", "text/plain", `120363@g.us\ID1__CON.txt`},
		{"120363@g.us", "ID2", "document", `a<b>:"c|?*.docx`, "application/octet-stream", `120363@g.us\ID2_a_b___c___.docx`},
		{"120363@g.us", "ID3", "image", "", "image/jpeg", `120363@g.us\ID3_image.jpg`},
		{"120363@g.us", "ID4", "document", "photo.jpeg", "image/jpeg", `120363@g.us\ID4_photo.jpeg`},
		{"120363@g.us", "ID5", "document", "v1.2 final", "application/pdf", `120363@g.us\ID5_v1.2 final.pdf`},
		{"..\\..\\x@g.us", "../ID6", "voice", "", "audio/ogg; codecs=opus", `x@g.us\ID6_voice.ogg`},
		{"120363@g.us", "ID7", "document", "\u202ereport\u202c.exe", "", `120363@g.us\ID7_report.bin`},
		// Extensions never come from the system mimetype registry (audio/x-mpegurl
		// is .m3u there), nor from a file name extension outside the safe list.
		{"120363@g.us", "ID9", "audio", "playlist.m3u", "audio/x-mpegurl", `120363@g.us\ID9_playlist.bin`},
		{"120363@g.us", "ID10", "document", "list.m3u8", "application/vnd.apple.mpegurl", `120363@g.us\ID10_list.bin`},
		{"120363@g.us", "ID11", "voice", "", "audio/x-mpegurl", `120363@g.us\ID11_voice.bin`},
		{"120363@g.us", "ID12", "audio", "song.m3u", "audio/mpeg", `120363@g.us\ID12_song.mp3`},
		{"120363@g.us", "ID13", "document", "notes.odt", "application/vnd.oasis.opendocument.text", `120363@g.us\ID13_notes.odt`},
		{"120363@g.us", "ID8", "document", strings.Repeat("x", 300) + ".pdf", "application/pdf", `120363@g.us\ID8_` + strings.Repeat("x", 80) + ".pdf"},
	} {
		got, err := relPath(tc.chat, tc.id, tc.kind, tc.name, tc.mime)
		if err != nil || got != filepath.FromSlash(strings.ReplaceAll(tc.want, `\`, "/")) {
			t.Errorf("relPath(%q, %q) = %q, %v; want %q", tc.name, tc.mime, got, err, tc.want)
			continue
		}
		if _, err := resolve(root, got); err != nil {
			t.Errorf("relPath output %q does not resolve: %v", got, err)
		}
	}
	if _, err := relPath("..", "..", "image", "", "image/jpeg"); err == nil {
		t.Error("empty ids must be refused")
	}
	// Names are capped in bytes: macOS and Linux allow 255 bytes per file
	// name, and 80 CJK letters or emoji are 240-320 bytes before the id,
	// the extension and the longest suffix a file gets.
	// A made-up id of any length leaves room too.
	for _, name := range []string{strings.Repeat("報", 80) + ".pdf", strings.Repeat("🎉", 80) + ".pdf", strings.Repeat("é", 80) + ".pdf", ""} {
		for _, id := range []string{strings.Repeat("A", 32), strings.Repeat("B", 240)} {
			got, err := relPath(strings.Repeat("1", 300)+"@g.us", id, "document", name, "application/pdf")
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range strings.Split(got, string(filepath.Separator)) {
				if n := len(part) + len(longestSuffix); n > 255 || !utf8.ValidString(part) {
					t.Errorf("relPath(%q, id %d bytes) = %q: %d bytes with suffix", name, len(id), part, n)
				}
			}
			if !strings.HasSuffix(got, ".pdf") {
				t.Errorf("relPath(%q) = %q lost the extension", name, got)
			}
		}
	}
}

func TestQueuePromotesOnDemandWork(t *testing.T) {
	q := newQueue(2)
	a, _ := q.submit(1, 1, false)
	q.submit(2, 2, false)
	if _, ok := q.submit(3, 3, false); ok {
		t.Fatal("background work beyond the cap must be dropped")
	}
	if b, _ := q.submit(2, 2, true); !b.high || q.position(b) != 1 {
		t.Fatal("on-demand request must promote the queued task")
	}
	if c, ok := q.submit(1, 1, false); !ok || c != a {
		t.Fatal("same media must share one task")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if t1, _ := q.next(ctx); t1.mediaID != 2 {
		t.Fatalf("first task = media %d, want the promoted one", t1.mediaID)
	}
}

// A failed background download is retried by the sweep, a bounded number of times.
func TestSweepRetriesFailedLiveMedia(t *testing.T) {
	e := newEnv(t)
	msg, mediaID, _ := e.add(1, "image", "image/png", "", "/v/s.enc?x", 10)
	e.src.set(func(f *fakeSource) { f.files["/v/s.enc?x"] = []byte("corrupted!") })
	if r := e.get(msg, false); r.Job.State != store.MediaFailed || !strings.Contains(r.Job.Error, "SHA-256") {
		t.Fatalf("setup: %+v", r.Job)
	}
	if _, err := os.Stat(filepath.Join(e.root, "393331111111@s.whatsapp.net", "3EB0A_image.png.part")); err == nil {
		t.Fatal("a rejected download must not leave its .part file")
	}
	refs, err := store.PendingLiveMedia(context.Background(), e.db, 0, EagerMaxSize, 10)
	if err != nil || len(refs) != 1 || refs[0].MediaID != mediaID {
		t.Fatalf("failed media not swept: %v %v", refs, err)
	}
	e.db.Exec(`UPDATE media SET attempts = ? WHERE id = ?`, store.MaxEagerAttempts, mediaID)
	if refs, _ = store.PendingLiveMedia(context.Background(), e.db, 0, EagerMaxSize, 10); len(refs) != 0 {
		t.Fatal("media failed too often must not be swept again")
	}
}

// The phone is asked to re-upload one file at most twice in 24 hours, across
// requests and restarts (the count is stored).
func TestReuploadRequestsCapped(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	e := newEnv(t, func(o *Options) { o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now } })
	msg, _, _ := e.add(1, "image", "image/jpeg", "", "/v/q.enc?x", 50)
	e.src.set(func(f *fakeSource) { f.gone["/v/q.enc?x"] = true })
	e.src.setReupload(nil) // the phone never answers
	for i := 1; i <= 2; i++ {
		if r := e.get(msg, false); r.Job.State != store.MediaExpired || e.src.reuploadCount() != i {
			t.Fatalf("request %d: %+v, receipts %d", i, r.Job, e.src.reuploadCount())
		}
	}
	r := e.get(msg, false)
	if e.src.reuploadCount() != 2 || !strings.Contains(r.Job.Error, "already asked 2 times") {
		t.Fatalf("third request in 24 h: receipts %d, %+v", e.src.reuploadCount(), r.Job)
	}
	mu.Lock()
	now = now.Add(24 * time.Hour)
	mu.Unlock()
	if e.get(msg, false); e.src.reuploadCount() != 3 {
		t.Fatalf("after 24 h: receipts %d", e.src.reuploadCount())
	}
}

// Two waiting downloads with the same WhatsApp id in different chats each
// get their own chat's answer.
func TestRetryAnswersRoutedByChat(t *testing.T) {
	m := New(Options{DB: nil})
	dm := types.NewJID("393331111111", types.DefaultUserServer)
	grp := types.NewJID("120363000000000009", types.GroupServer)
	a, b := make(chan *events.MediaRetry, 1), make(chan *events.MediaRetry, 1)
	m.retries[retryKey(dm, types.JID{}, "SAME")] = a
	m.retries[retryKey(grp, dm, "SAME")] = b
	m.MediaRetry(&events.MediaRetry{MessageID: "SAME", ChatID: grp, SenderID: dm})
	select {
	case <-b:
	default:
		t.Fatal("group answer not delivered to the group download")
	}
	if len(a) != 0 {
		t.Fatal("group answer delivered to the DM download")
	}
	// An answer naming neither chat is ambiguous: dropped, not guessed.
	m.MediaRetry(&events.MediaRetry{MessageID: "SAME"})
	if len(a)+len(b) != 0 {
		t.Fatal("ambiguous answer delivered")
	}
	// The only download waiting for an id gets its answer whatever the JID form.
	delete(m.retries, retryKey(grp, dm, "SAME"))
	m.MediaRetry(&events.MediaRetry{MessageID: "SAME", ChatID: types.NewJID("111", types.HiddenUserServer)})
	if len(a) != 1 {
		t.Fatal("single waiting download did not get its answer")
	}
}

// A panicking task fails with an error that keeps the panic value out, and
// its worker goes on with the next task: the daemon stays up.
func TestWorkerSurvivesPanic(t *testing.T) {
	m := New(Options{DB: nil})
	defer m.Stop()
	q := newQueue(10)
	m.loop(q, func(t *task) error {
		if t.mediaID == 1 {
			panic("boom")
		}
		return nil
	})
	for _, c := range []struct {
		id      int64
		wantErr bool
	}{{1, true}, {2, false}} {
		id, wantErr := c.id, c.wantErr
		task, _ := q.submit(id, id, true)
		select {
		case <-task.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("task %d never finished", id)
		}
		if (task.err != nil) != wantErr || task.err != nil && strings.Contains(task.err.Error(), "boom") {
			t.Fatalf("task %d: err %v, want error %v without the panic value", id, task.err, wantErr)
		}
	}
}
