package media

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
)

// Source is the part of the WhatsApp client media downloads use. The daemon
// passes WhatsAppSource; tests pass a fake.
type Source interface {
	// Connected reports whether downloads and re-upload requests can work now.
	Connected() bool
	// Download fetches, decrypts and verifies a file into f, streaming
	// (whatsmeow's DownloadToFile).
	Download(ctx context.Context, d *Downloadable, f *os.File) error
	// RequestReupload asks the phone to upload the file again (a media retry
	// receipt); the answer arrives as an events.MediaRetry.
	RequestReupload(ctx context.Context, info *types.MessageInfo, mediaKey []byte) error
	// FetchEncrypted downloads the encrypted bytes at directPath as they
	// are, refusing files over limit bytes.
	FetchEncrypted(ctx context.Context, directPath, mmsType string, limit int64) ([]byte, error)
}

// Downloadable describes one encrypted file. It implements whatsmeow's
// DownloadableMessage and MediaTypeable.
type Downloadable struct {
	DirectPath string
	MediaKey   []byte
	SHA256     []byte
	EncSHA256  []byte
	Length     uint64
	Type       whatsmeow.MediaType
}

func (d *Downloadable) GetDirectPath() string             { return d.DirectPath }
func (d *Downloadable) GetMediaKey() []byte               { return d.MediaKey }
func (d *Downloadable) GetFileSHA256() []byte             { return d.SHA256 }
func (d *Downloadable) GetFileEncSHA256() []byte          { return d.EncSHA256 }
func (d *Downloadable) GetFileLength() uint64             { return d.Length }
func (d *Downloadable) GetMediaType() whatsmeow.MediaType { return d.Type }

// mediaType maps a stored message kind to whatsmeow's key-derivation type
// and the CDN's mms type. Stickers are images.
func mediaType(kind string) (whatsmeow.MediaType, string, bool) {
	switch kind {
	case "image", "sticker":
		return whatsmeow.MediaImage, "image", true
	case "video":
		return whatsmeow.MediaVideo, "video", true
	case "audio", "voice":
		return whatsmeow.MediaAudio, "audio", true
	case "document":
		return whatsmeow.MediaDocument, "document", true
	}
	return "", "", false
}

// directPathOf returns the download path of a media row: the stored direct
// path, else the path of its URL. The query string stays: it carries the CDN
// auth tokens, and whatsmeow appends "&hash=..." to it.
func directPathOf(directPath, url string) string {
	p := directPath
	if p == "" {
		_, rest, ok := strings.Cut(url, ".net/")
		if !ok {
			return ""
		}
		p = "/" + rest
	}
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	if !strings.Contains(p, "?") {
		p += "?"
	}
	return p
}

// goneFromServer reports a CDN answer that means the server no longer has
// the file: only the phone can upload it again.
func goneFromServer(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) ||
		errors.Is(err, whatsmeow.ErrNoURLPresent)
}

// hashMismatch reports a file whose hashes differ from the message's.
func hashMismatch(err error) bool {
	return errors.Is(err, whatsmeow.ErrInvalidMediaEncSHA256) || errors.Is(err, whatsmeow.ErrInvalidMediaSHA256)
}

// errDifferentKey is a re-upload that fails the message's media key: the
// phone encrypted it with a key it does not send (seen on history media a
// month old and more). It is not final: a later request may get a good copy.
var errDifferentKey = errors.New(reuploadKeyError)

const reuploadKeyError = "the phone re-uploaded the file encrypted with a key it does not share; ask again later, or forward it from the phone"

// decryptWithMAC decrypts a re-uploaded file whose hashes no longer match the
// message (the phone re-encoded it), authenticating it with the media key's
// MAC instead.
func decryptWithMAC(data, mediaKey []byte, t whatsmeow.MediaType) ([]byte, error) {
	const macLength = 10
	if len(data) <= macLength {
		return nil, whatsmeow.ErrTooShortFile
	}
	file, mac := data[:len(data)-macLength], data[len(data)-macLength:]
	keys := hkdfutil.SHA256(mediaKey, nil, []byte(t), 112)
	iv, cipherKey, macKey := keys[:16], keys[16:48], keys[48:80]
	h := hmac.New(sha256.New, macKey)
	h.Write(iv)
	h.Write(file)
	if !hmac.Equal(h.Sum(nil)[:macLength], mac) {
		return nil, errDifferentKey
	}
	return cbcutil.Decrypt(cipherKey, iv, file)
}

// WhatsAppSource downloads through the daemon's whatsmeow client. Client
// returns nil while the profile is not paired or not started.
type WhatsAppSource struct {
	Client func() *whatsmeow.Client
	HTTP   *http.Client
}

func (s WhatsAppSource) cli() (*whatsmeow.Client, error) {
	c := s.Client()
	if c == nil {
		return nil, errNotConnected
	}
	return c, nil
}

func (s WhatsAppSource) Connected() bool {
	c := s.Client()
	return c != nil && c.IsConnected() && c.IsLoggedIn()
}

func (s WhatsAppSource) Download(ctx context.Context, d *Downloadable, f *os.File) error {
	c, err := s.cli()
	if err != nil {
		return err
	}
	return c.DownloadToFile(ctx, d, f)
}

func (s WhatsAppSource) RequestReupload(ctx context.Context, info *types.MessageInfo, mediaKey []byte) error {
	c, err := s.cli()
	if err != nil {
		return err
	}
	return c.SendMediaRetryReceipt(ctx, info, mediaKey)
}

func (s WhatsAppSource) FetchEncrypted(ctx context.Context, directPath, mmsType string, limit int64) ([]byte, error) {
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://mmg.whatsapp.net"+directPath+"&mms-type="+mmsType+"&__wa-mms=", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Origin", "https://web.whatsapp.com")
	req.Header.Set("Referer", "https://web.whatsapp.com/")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("re-uploaded media download failed with status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		return nil, fmt.Errorf("re-uploaded file is over %d MB: forward it from the phone", limit>>20)
	}
	return b, err
}
