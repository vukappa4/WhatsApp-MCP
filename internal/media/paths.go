package media

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// extByMime maps the mimetypes WhatsApp sends to file extensions. Lookups
// ignore parameters (audio/ogg; codecs=opus is .ogg). The table is the only
// source of extensions: the system mimetype registry would turn a sender's
// audio/x-mpegurl into .m3u, a playlist ffmpeg follows to URLs.
var extByMime = map[string]string{
	"image/jpeg":                    ".jpg",
	"image/png":                     ".png",
	"image/webp":                    ".webp",
	"image/gif":                     ".gif",
	"image/heic":                    ".heic",
	"video/mp4":                     ".mp4",
	"video/3gpp":                    ".3gp",
	"video/quicktime":               ".mov",
	"audio/ogg":                     ".ogg",
	"audio/mpeg":                    ".mp3",
	"audio/mp4":                     ".m4a",
	"audio/aac":                     ".aac",
	"audio/amr":                     ".amr",
	"audio/wav":                     ".wav",
	"audio/x-wav":                   ".wav",
	"audio/webm":                    ".webm",
	"video/webm":                    ".webm",
	"application/pdf":               ".pdf",
	"application/zip":               ".zip",
	"text/plain":                    ".txt",
	"text/csv":                      ".csv",
	"application/msword":            ".doc",
	"application/vnd.ms-excel":      ".xls",
	"application/vnd.ms-powerpoint": ".ppt",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
}

// extFromMime returns the extension for a mimetype, "" when the table does
// not know it.
func extFromMime(mimetype string) string {
	return extByMime[strings.ToLower(strings.TrimSpace(strings.SplitN(mimetype, ";", 2)[0]))]
}

// safeExt lists the extensions a sender's file name may keep: the table's
// own plus common document and media names. Anything else (.m3u, .exe,
// .lnk, .hta...) is dropped, so no stored file opens as a playlist or runs
// as a program.
var safeExt = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range []string{".jpeg", ".odt", ".ods", ".odp", ".rtf", ".md", ".json", ".xml", ".heif", ".tif", ".tiff",
		".bmp", ".m4v", ".mkv", ".7z", ".rar", ".epub", ".vcf", ".ics"} {
		m[e] = true
	}
	for _, e := range extByMime {
		m[e] = true
	}
	return m
}()

// plainExt is the extension of name when it looks like one (a dot and 1-5
// letters or digits), "" otherwise: "v1.2 final" has none.
func plainExt(name string) string {
	e := filepath.Ext(name)
	if len(e) < 2 || len(e) > 6 {
		return ""
	}
	for _, r := range e[1:] {
		if !(r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))) {
			return ""
		}
	}
	return strings.ToLower(e)
}

var reservedNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func init() {
	for i := 1; i <= 9; i++ {
		reservedNames[fmt.Sprintf("COM%d", i)] = true
		reservedNames[fmt.Sprintf("LPT%d", i)] = true
	}
}

// sanitizeName makes one path component safe on Windows: no separators,
// reserved or control characters, no leading or trailing dots and spaces, not
// a reserved device name, at most max runes. It returns "" when nothing is left.
func sanitizeName(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f, unicode.Is(unicode.Cf, r), r == utf8.RuneError:
			continue
		case strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(out); len(r) > max {
		out = string(r[:max])
	}
	out = strings.Trim(out, ". ")
	stem := strings.ToUpper(strings.SplitN(out, ".", 2)[0])
	if reservedNames[strings.TrimSpace(stem)] {
		out = "_" + out
	}
	return out
}

// idPart keeps letters and digits only: WhatsApp ids and JIDs become safe
// file and directory names.
func idPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '@' || r == '.' || r == '-' || r == '_') {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), ".")
}

// maxFileNameBytes is the longest file name every supported file system
// takes: 255 bytes on macOS and Linux (Windows counts 255 UTF-16 units,
// never more than the bytes).
const maxFileNameBytes = 255

// longestSuffix is the longest ending added to a stored media file name
// (sidecarPath; downloads also use ".part").
const longestSuffix = ".transcript.json"

// maxIDBytes caps the chat JID and message id parts of a media path. Real
// ones are about 20 to 40 characters; the cap keeps room for a name however
// long an id a sender's client makes up.
const maxIDBytes = 64

// relPath is where a media file lives under the media root:
// <chat JID>\<WhatsApp message id>_<file name or kind><ext>. The extension
// comes from the mimetype table, or from the file name when it is a known
// safe one; anything else is .bin. The ids are cut to maxIDBytes and the name
// so the file name with longestSuffix fits maxFileNameBytes. JIDs and
// WhatsApp ids survive a reingest (row ids do not), so a rebuilt database
// finds the files it downloaded before.
func relPath(chatJID, waID, kind, filename, mimetype string) (string, error) {
	return mediaPath(chatJID, waID, kind, filename, mimetype, true)
}

// uncappedRelPath is relPath without the byte limits. Windows takes names of
// up to 255 UTF-16 units, so a file may sit under this longer name; download
// adopts it there too.
func uncappedRelPath(chatJID, waID, kind, filename, mimetype string) (string, error) {
	return mediaPath(chatJID, waID, kind, filename, mimetype, false)
}

func mediaPath(chatJID, waID, kind, filename, mimetype string, capped bool) (string, error) {
	dir, id := idPart(chatJID), idPart(waID)
	if dir == "" || id == "" {
		return "", fmt.Errorf("cannot name a file for chat %q message %q", chatJID, waID)
	}
	if capped { // idPart keeps ASCII only: any byte is a cut point
		dir, id = strings.TrimRight(dir[:min(len(dir), maxIDBytes)], "."), id[:min(len(id), maxIDBytes)]
	}
	ext := extFromMime(mimetype)
	name := sanitizeName(filename, 80)
	if fe := plainExt(name); fe != "" {
		// Voice notes and audio are fed to ffmpeg: their extension comes
		// from the mimetype table only.
		if safeExt[fe] && !isAudio(kind) && (ext == "" || fe == ext || (fe == ".jpeg" && ext == ".jpg")) {
			ext = fe
		}
		name = strings.TrimRight(strings.TrimSuffix(name, filepath.Ext(name)), ". ")
	}
	if ext == "" {
		ext = ".bin"
	}
	if capped {
		name = cutBytes(name, maxFileNameBytes-len(longestSuffix)-len(id)-len("_")-len(ext))
	}
	if name == "" {
		name = kind
	}
	return filepath.Join(dir, id+"_"+name+ext), nil
}

// cutBytes returns the longest prefix of s of at most n bytes that ends on a
// whole letter, without trailing dots and spaces (Windows drops them).
func cutBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) > n {
		s = s[:n]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return strings.TrimRight(s, ". ")
}

var errOutsideRoot = errors.New("media path escapes the media root")

// resolve turns a stored relative path into an absolute one under root and
// refuses anything that would leave it.
func resolve(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: %q", errOutsideRoot, rel)
	}
	abs := filepath.Join(root, rel)
	back, err := filepath.Rel(root, abs)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", errOutsideRoot, rel)
	}
	return abs, nil
}
