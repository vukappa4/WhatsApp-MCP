package outgoing

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // decode PNG for dimensions and thumbnails
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

// Kind is the WhatsApp message type a file is sent as.
type Kind string

const (
	KindAuto     Kind = "auto"
	KindImage    Kind = "image"
	KindVideo    Kind = "video"
	KindVoice    Kind = "voice_note"
	KindDocument Kind = "document"
)

// Limits are the largest uploads per kind. Image, video and voice notes
// follow WhatsApp's 16 MB media limit. Documents are capped at 100 MB: the
// upload holds the whole file in memory.
var Limits = map[Kind]int64{KindImage: 16 << 20, KindVideo: 16 << 20, KindVoice: 16 << 20, KindDocument: 100 << 20}

// maxVoiceInput caps the audio file a voice note is converted from.
const maxVoiceInput = 200 << 20

// File is a local file ready to upload.
type File struct {
	Kind      Kind
	Data      []byte // what is uploaded: the file, or the converted Opus for a voice note
	Mimetype  string
	FileName  string
	Width     uint32
	Height    uint32
	Thumbnail []byte // JPEG, images only
	Seconds   uint32 // voice notes
	Waveform  []byte // voice notes: 64 samples, 0-100
}

var extMime = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".mp4": "video/mp4", ".m4v": "video/mp4",
	".pdf": "application/pdf", ".txt": "text/plain", ".csv": "text/csv", ".zip": "application/zip",
	".doc": "application/msword", ".xls": "application/vnd.ms-excel", ".ppt": "application/vnd.ms-powerpoint",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".mp3":  "audio/mpeg", ".ogg": "audio/ogg", ".opus": "audio/ogg", ".m4a": "audio/mp4", ".wav": "audio/wav",
}

func mimeOf(path string, head []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	if m, ok := extMime[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return strings.TrimSpace(strings.Split(m, ";")[0])
	}
	if m := http.DetectContentType(head); m != "application/octet-stream" {
		return strings.TrimSpace(strings.Split(m, ";")[0])
	}
	return "application/octet-stream"
}

// ParseKind accepts auto, document, image, video and voice_note ("" is auto).
func ParseKind(s string) (Kind, error) {
	switch k := Kind(strings.TrimSpace(s)); k {
	case "", KindAuto:
		return KindAuto, nil
	case KindImage, KindVideo, KindVoice, KindDocument:
		return k, nil
	}
	return "", fmt.Errorf("kind must be auto, document, image, video or voice_note, got %q", s)
}

// afterOpen runs right after the policy opened the file. Tests use it to
// swap the file under the path.
var afterOpen = func(*Opened) {}

// Prepare opens path through the policy, picks the kind and reads or
// converts the file for upload. Everything is read from the one handle the
// policy checked: replacing the file after the check changes nothing.
func Prepare(ctx context.Context, p Policy, path string, kind Kind) (*File, error) {
	o, err := p.Open(path)
	if err != nil {
		return nil, err
	}
	defer o.Close()
	afterOpen(o)
	real, size := o.Final, o.Size
	if size == 0 {
		return nil, fmt.Errorf("%s is empty", real)
	}
	head := make([]byte, 512)
	n, _ := o.ReadAt(head, 0)
	head = head[:n]
	sniff := http.DetectContentType(head)
	ext := strings.ToLower(filepath.Ext(real))
	if kind == KindAuto {
		switch {
		case sniff == "image/jpeg" || sniff == "image/png":
			kind = KindImage
		case (ext == ".mp4" || ext == ".m4v") && sniff == "video/mp4":
			kind = KindVideo
		default:
			kind = KindDocument
		}
	}
	f := &File{Kind: kind, FileName: filepath.Base(real)}
	limit := Limits[kind]
	switch kind {
	case KindImage:
		if sniff != "image/jpeg" && sniff != "image/png" {
			return nil, fmt.Errorf("%s is not a JPEG or PNG image (detected %s): send it with kind=document", f.FileName, sniff)
		}
		f.Mimetype = sniff
	case KindVideo:
		if sniff != "video/mp4" {
			return nil, fmt.Errorf("%s is not an MP4 video (detected %s): send it with kind=document", f.FileName, sniff)
		}
		f.Mimetype = "video/mp4"
	case KindVoice:
		if size > maxVoiceInput {
			return nil, fmt.Errorf("%s is %s: voice notes are converted from audio files up to %s", f.FileName, mb(size), mb(maxVoiceInput))
		}
		v, err := convertOpened(ctx, o.File, f.FileName)
		if err != nil {
			return nil, err
		}
		if int64(len(v.Data)) > limit {
			return nil, fmt.Errorf("the voice note converted from %s is %s, over WhatsApp's %s limit: shorten the audio", f.FileName, mb(int64(len(v.Data))), mb(limit))
		}
		f.Data, f.Seconds, f.Waveform, f.Mimetype = v.Data, v.Seconds, v.Waveform, "audio/ogg; codecs=opus"
		return f, nil
	case KindDocument:
		f.Mimetype = mimeOf(real, head)
	}
	if size > limit {
		return nil, fmt.Errorf("%s is %s: the limit for kind=%s is %s", f.FileName, mb(size), kind, mb(limit))
	}
	data, err := io.ReadAll(io.NewSectionReader(o.File, 0, limit+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", real, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s grew past the limit for kind=%s (%s) while being read", f.FileName, kind, mb(limit))
	}
	f.Data = data
	if kind == KindImage {
		f.Width, f.Height, f.Thumbnail = imageMeta(f.Data)
	}
	return f, nil
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

// thumbSide is the longest side of an image thumbnail in pixels.
const thumbSide = 72

// imageMeta returns the dimensions and a small JPEG thumbnail; zero values
// when the image cannot be decoded.
func imageMeta(data []byte) (w, h uint32, thumb []byte) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, nil
	}
	w, h = uint32(cfg.Width), uint32(cfg.Height)
	if cfg.Width*cfg.Height > 50_000_000 {
		return w, h, nil // decoding would cost too much memory for a preview
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return w, h, nil
	}
	small := downscale(img, thumbSide)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 60}); err != nil {
		return w, h, nil
	}
	return w, h, buf.Bytes()
}

// downscale box-filters img so its longest side is at most side pixels.
func downscale(img image.Image, side int) image.Image {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := sw, sh
	if sw >= sh && sw > side {
		dw, dh = side, max(1, sh*side/sw)
	} else if sh > sw && sh > side {
		dw, dh = max(1, sw*side/sh), side
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0, y1 := b.Min.Y+y*sh/dh, b.Min.Y+max((y+1)*sh/dh, y*sh/dh+1)
		for x := 0; x < dw; x++ {
			x0, x1 := b.Min.X+x*sw/dw, b.Min.X+max((x+1)*sw/dw, x*sw/dw+1)
			var r, g, bl, a, n uint64
			// At most 8x8 samples per thumbnail pixel keeps large photos cheap.
			sy, sx := max(1, (y1-y0)/8), max(1, (x1-x0)/8)
			for yy := y0; yy < y1; yy += sy {
				for xx := x0; xx < x1; xx += sx {
					cr, cg, cb, ca := img.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			i := out.PixOffset(x, y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(bl/n>>8), uint8(a/n>>8)
		}
	}
	return out
}
