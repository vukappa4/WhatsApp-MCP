package outgoing

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vukappa4/whatsapp-mcp/internal/childproc"
	"github.com/vukappa4/whatsapp-mcp/internal/testaudio"
)

// native turns a test path written with backslashes into this OS's form.
func native(rel string) string { return filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")) }

// fakeHome builds a home with the Windows layout (%LOCALAPPDATA% and
// %APPDATA% inside it) in a temp dir and returns a policy over it plus a
// helper that creates files. The layout works on every OS: the policy only
// compares paths. The temp dir is resolved first (on macOS it is a symlink
// into /private), so paths given and final paths agree.
func fakeHome(t *testing.T) (Policy, func(rel string) string) {
	t.Helper()
	return fakeHomeNamed(t, "home")
}

// fakeHomeNamed is fakeHome with the home folder called name.
func fakeHomeNamed(t *testing.T, name string) (Policy, func(rel string) string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{Home: filepath.Join(root, name), LocalAppData: filepath.Join(root, name, "AppData", "Local"),
		AppData: filepath.Join(root, name, "AppData", "Roaming")}
	p.AppRoot = filepath.Join(p.LocalAppData, "WhatsAppMCP")
	p.DataDir = filepath.Join(p.AppRoot, "dev")
	for _, d := range []string{"Projects", "OneDrive - Personal"} {
		if err := os.MkdirAll(filepath.Join(p.Home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p.Roots = DefaultRoots(p.Home, p.DataDir)
	p.Scratch = DefaultScratch(filepath.Join(p.Home, "AppData", "Local", "Temp"))
	mk := func(rel string) string {
		t.Helper()
		path := filepath.Join(p.Home, native(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return p, mk
}

// check opens path through the policy and closes it again.
func (p Policy) check(path string) (string, int64, error) {
	o, err := p.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer o.Close()
	return o.Final, o.Size, nil
}

func TestPolicyAllowlist(t *testing.T) {
	p, mk := fakeHome(t)
	allowed := []string{
		`Documents\fattura.pdf`,
		`Desktop\foto vacanze.jpg`,
		`Downloads\offerta.docx`,
		`Pictures\a.png`, VideosFolder + `\b.mp4`, `Music\c.mp3`,
		`OneDrive - Personal\Lavoro\piano.xlsx`,
		`Documents\Lavoro\business case.pdf`,
		`AppData\Local\Temp\claude\C--proj\0b1c\scratchpad\grafico.png`,
		`AppData\Local\Temp\claude\C--proj\0b1c\scratchpad\out\deck.pdf`,
		`AppData\Local\WhatsAppMCP\dev\media\c_12\IMG-1.jpg`,
		`Documents\keynote.pptx`,    // ".key" is a suffix rule, not a substring
		`Documents\tokens-list.txt`, // only a file named exactly "token"
		`Documents\database-notes.txt`,
	}
	for _, rel := range allowed {
		path := mk(rel)
		got, size, err := p.check(path)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if !strings.EqualFold(got, path) || size != 1 {
			t.Errorf("%s: resolved %s size %d", rel, got, size)
		}
	}
	// Outside the allowed folders: refused, with the way to allow it.
	for _, rel := range []string{`projects\app\main.go`, `notes.txt`, `AppData\Roaming\Code\User\settings.json`,
		`AppData\Local\WhatsAppMCP\prod\media\c_3\voice.ogg`, `AppData\Local\Temp\other\x.pdf`,
		// Session transcripts next to the scratchpads.
		`AppData\Local\Temp\claude\C--proj\0b1c\tasks\a1.output`, `AppData\Local\Temp\claude\notes.txt`} {
		if _, _, err := p.check(mk(rel)); !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "send_file_extra_roots") {
			t.Errorf("%s: err = %v, want outside the allowed folders", rel, err)
		}
	}
	// No folders at all: nothing is allowed.
	empty := p
	empty.Roots = nil
	if _, _, err := empty.check(mk(`Documents\ok.pdf`)); !errors.Is(err, ErrDenied) {
		t.Errorf("no roots: %v", err)
	}
}

func TestPolicyDenylist(t *testing.T) {
	p, mk := fakeHome(t)
	denied := []string{
		`Projects\piano.pdf`, // a home folder that is not a default root
		`AppData\Local\WhatsAppMCP\dev\session.db`,
		`AppData\Local\WhatsAppMCP\dev\data.db`,
		`AppData\Local\WhatsAppMCP\dev\token`,
		`AppData\Local\WhatsAppMCP\dev\logs\whatsapp-mcp.log`,
		`AppData\Local\WhatsAppMCP\hooks\abc`,
		`.ssh\config`,
		`.aws\credentials`,
		`.config\gh\hosts.yml`,
		`.claude\settings.json`,
		`.claude.json`,
		`.claude-work\.credentials.json`,
		`AppData\Local\Google\Chrome\User Data\Default\Cookies`,
		`AppData\Local\Microsoft\Edge\User Data\Default\Login Data`,
		`AppData\Roaming\Mozilla\Firefox\Profiles\x.default\logins.json`,
		`Documents\server.pem`,
		`Documents\SERVER.KEY`,
		`Documents\id_ed25519`,
		`Documents\id_rsa.pub`,
		`projects\app\.env`,
		`projects\app\.env.local`,
		`Documents\.environment-notes.txt`,
		`Downloads\token`,
		`Downloads\session.db`,
		`old-bridge\store\whatsapp.db`,
		// Inside allowed folders, still secrets, keys, databases or repositories.
		`Documents\.git-credentials`, `Documents\.netrc`, `Documents\_netrc`, `Documents\.npmrc`, `Documents\.pypirc`,
		`Documents\Passwords.kdbx`, `Desktop\cert.pfx`, `Desktop\cert.P12`, `Documents\putty.ppk`, `Downloads\work.ovpn`,
		`Documents\app.db`, `Documents\app.db-wal`, `Documents\cache.sqlite`, `Documents\x.sqlite3`,
		`Documents\repo\.git\config`, `Documents\repo\.GIT\objects\ab\cdef`, `Documents\.docker\config.json`,
		`Documents\.kube\config`, `Documents\.azure\accessTokens.json`, `Documents\.ssh\id_backup.txt`,
		`Desktop\.env`, `Downloads\id_ed25519`, `Desktop\key.pem`,
	}
	for _, rel := range denied {
		path := mk(rel)
		if _, _, err := p.check(path); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: err = %v, want ErrDenied", rel, err)
		}
	}
	// Case changes and ".." do not get around the rules.
	if _, _, err := p.check(filepath.Join(p.Home, native(`Documents\..\.SSH\config`))); !errors.Is(err, ErrDenied) {
		t.Errorf("dotdot to .ssh: %v", err)
	}
	docs := mk(`Documents\ok.txt`)
	for _, bad := range []string{`relative\file.txt`, `\\server\share\x.pdf`, `//server/share/x.pdf`, `/\server\share\x.pdf`, "",
		`\\?\` + docs, `/\?\` + docs, `\??\` + docs, `\\.\` + docs, `\x.txt`} {
		if _, _, err := p.check(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, _, err := p.check(mk(`Documents\a.txt`) + ":hidden"); err == nil && runtime.GOOS == "windows" {
		t.Error("alternate data stream accepted")
	}
	if _, _, err := p.check(filepath.Join(p.Home, "missing.pdf")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing file: %v", err)
	}
	if _, _, err := p.check(filepath.Join(p.Home, "Documents")); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: %v", err)
	}
}

// A home folder whose name changes byte length when lowercased ("İ" becomes
// "i" plus a combining dot) keeps every rule: its media folder may be sent
// from, its .claude folder may not.
func TestPolicyNonASCIIHome(t *testing.T) {
	p, mk := fakeHomeNamed(t, "İlker")
	if _, _, err := p.check(mk(`AppData\Local\WhatsAppMCP\dev\media\c_12\doc.pdf`)); err != nil {
		t.Errorf("own media: %v", err)
	}
	p.Roots = append(p.Roots, p.Home)
	for _, rel := range []string{`.claude\notes.txt`, `.claude-work\notes.txt`, `AppData\Local\WhatsAppMCP\dev\notes.txt`} {
		if _, _, err := p.check(mk(rel)); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: err = %v, want ErrDenied", rel, err)
		}
	}
}

// A junction (mklink /J, no admin needed) into a denied folder is judged by
// its target, like the "Documents and Settings" junction Windows ships.
func TestPolicyFollowsJunctions(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are Windows only")
	}
	p, mk := fakeHome(t)
	mk(`.aws\credentials`)
	link := filepath.Join(p.Home, "Documents", "Documents and Settings")
	_ = os.MkdirAll(filepath.Dir(link), 0o700)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, filepath.Join(p.Home, ".aws")).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	if _, _, err := p.check(filepath.Join(link, "credentials")); !errors.Is(err, ErrDenied) {
		t.Fatalf("junction into .aws: %v", err)
	}
	// A junction inside an allowed folder to a folder outside: judged by the target.
	mk(`projects\secret-plan.txt`)
	out := filepath.Join(p.Home, "Desktop", "progetti")
	_ = os.MkdirAll(filepath.Dir(out), 0o700)
	if o, err := exec.Command("cmd", "/c", "mklink", "/J", out, filepath.Join(p.Home, "projects")).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, o)
	}
	if _, _, err := p.check(filepath.Join(out, "secret-plan.txt")); !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("junction out of Desktop: %v", err)
	}
}

// The file sent is the file checked: Prepare reads from the handle the policy
// opened, so swapping the file under the path after the check sends nothing new.
func TestPrepareReadsTheCheckedFile(t *testing.T) {
	p, mk := fakeHome(t)
	doc := mk(`Documents\offerta.txt`)
	_ = os.WriteFile(doc, []byte("offerta commerciale"), 0o600)
	secret := mk(`notes\secret.txt`)
	_ = os.WriteFile(secret, []byte("password: hunter2"), 0o600)
	swapped := false
	afterOpen = func(o *Opened) {
		// Windows refuses to move an open file; where it works, a secret takes its place.
		if os.Rename(doc, doc+".old") == nil {
			swapped = true
			_ = os.Rename(secret, doc)
		}
	}
	t.Cleanup(func() { afterOpen = func(*Opened) {} })
	f, err := Prepare(context.Background(), p, doc, KindDocument)
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Data) != "offerta commerciale" {
		t.Fatalf("sent %q (swapped=%v)", f.Data, swapped)
	}
}

// A hard link gives a secret a second, innocent name: refused.
func TestPolicyRefusesHardLinks(t *testing.T) {
	p, mk := fakeHome(t)
	secret := mk(`.ssh\id_ed25519`)
	alias := filepath.Join(p.Home, "Desktop", "notes.txt")
	_ = os.MkdirAll(filepath.Dir(alias), 0o700)
	if err := os.Link(secret, alias); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	if _, _, err := p.check(alias); !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("hard link: %v", err)
	}
}

// A symlink (or junction) into a denied folder is judged by its target.
func TestPolicyFollowsSymlinks(t *testing.T) {
	p, mk := fakeHome(t)
	target := mk(`.ssh\id_backup.txt`)
	link := filepath.Join(p.Home, "Desktop", "innocent.txt")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here (needs developer mode on Windows): %v", err)
	}
	if _, _, err := p.check(link); !errors.Is(err, ErrDenied) {
		t.Fatalf("symlink into .ssh: %v", err)
	}
}

func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, h/2, color.RGBA{R: 255, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareKinds(t *testing.T) {
	p, mk := fakeHome(t)
	ctx := context.Background()
	img := mk(`Pictures\shot.png`)
	writePNG(t, img, 640, 480)
	f, err := Prepare(ctx, p, img, KindAuto)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindImage || f.Mimetype != "image/png" || f.Width != 640 || f.Height != 480 {
		t.Fatalf("png: %+v", f)
	}
	if tc, _, err := image.DecodeConfig(bytes.NewReader(f.Thumbnail)); err != nil || tc.Width != 72 || tc.Height != 54 {
		t.Fatalf("thumbnail %dx%d: %v", tc.Width, tc.Height, err)
	}

	doc := mk(`Documents\report.pdf`)
	_ = os.WriteFile(doc, []byte("%PDF-1.7\n..."), 0o600)
	if f, err = Prepare(ctx, p, doc, KindAuto); err != nil || f.Kind != KindDocument || f.Mimetype != "application/pdf" || f.FileName != "report.pdf" {
		t.Fatalf("pdf: %+v %v", f, err)
	}
	// A PDF is never an image or a video.
	if _, err := Prepare(ctx, p, doc, KindImage); err == nil || !strings.Contains(err.Error(), "kind=document") {
		t.Fatalf("pdf as image: %v", err)
	}
	if _, err := Prepare(ctx, p, doc, KindVideo); err == nil {
		t.Fatal("pdf as video accepted")
	}
	// A JPEG extension on a text file is a document, not an image.
	fake := mk(`Pictures\not-really.jpg`)
	if f, err = Prepare(ctx, p, fake, KindAuto); err != nil || f.Kind != KindDocument {
		t.Fatalf("fake jpg: %+v %v", f, err)
	}

	big := filepath.Join(p.Home, VideosFolder, "big.png")
	if err := os.MkdirAll(filepath.Dir(big), 0o700); err != nil {
		t.Fatal(err)
	}
	fh, _ := os.Create(big)
	_ = fh.Truncate(Limits[KindImage] + 1)
	fh.Close()
	if _, err := Prepare(ctx, p, big, KindDocument); err != nil {
		t.Fatalf("17 MB document refused: %v", err)
	}
	if _, err := Prepare(ctx, p, big, KindImage); err == nil {
		t.Fatal("17 MB file accepted as image") // also not a PNG: either error is right
	}
	if _, err := ParseKind("sticker"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestImageSizeLimit(t *testing.T) {
	p, mk := fakeHome(t)
	img := mk(`Pictures\huge.png`)
	writePNG(t, img, 10, 10)
	old := Limits[KindImage]
	Limits[KindImage] = 10
	defer func() { Limits[KindImage] = old }()
	if _, err := Prepare(context.Background(), p, img, KindImage); err == nil || !strings.Contains(err.Error(), "limit for kind=image") {
		t.Fatalf("over limit: %v", err)
	}
}

// Voice notes: any audio becomes Ogg Opus 16 kHz mono with duration and waveform.
func TestConvertVoice(t *testing.T) {
	ffmpeg, err := childproc.FFmpeg()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "memo.wav")
	// 3 s: 1 s silence, 2 s tone, stereo 44.1 kHz.
	tone := testaudio.Tone(440, 0.8)
	silenceThenTone := func(t float64) float64 {
		if t < 1 {
			return 0
		}
		return tone(t)
	}
	if err := testaudio.WAV(src, 44100, 2, 3, silenceThenTone); err != nil {
		t.Fatal(err)
	}
	v, err := ConvertVoice(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(v.Data, []byte("OggS")) || !bytes.Contains(v.Data[:200], []byte("OpusHead")) {
		t.Fatalf("not Ogg Opus: % x", v.Data[:16])
	}
	if i := bytes.Index(v.Data, []byte("OpusHead")); v.Data[i+9] != 1 { // OpusHead: magic, version, channel count
		t.Fatalf("channels = %d, want mono", v.Data[i+9])
	}
	if v.Seconds != 3 {
		t.Fatalf("seconds = %d, want 3", v.Seconds)
	}
	if len(v.Waveform) != 64 {
		t.Fatalf("waveform has %d samples", len(v.Waveform))
	}
	if v.Waveform[2] > 5 || v.Waveform[50] < 80 {
		t.Fatalf("waveform does not show silence then tone: %v", v.Waveform)
	}
	probe := exec.Command(filepath.Join(filepath.Dir(ffmpeg), "ffprobe"+filepath.Ext(ffmpeg)), "-v", "error", "-show_entries", "stream=codec_name,sample_rate,channels", "-of", "csv=p=0", "-")
	childproc.Hide(probe)
	probe.Stdin = bytes.NewReader(v.Data)
	if out, err := probe.Output(); err == nil && strings.TrimSpace(string(out)) != "opus,48000,1" && strings.TrimSpace(string(out)) != "opus,16000,1" {
		t.Fatalf("ffprobe: %s", out)
	}

	// Garbage in: a clear error, not a crash.
	bad := filepath.Join(dir, "bad.mp3")
	_ = os.WriteFile(bad, []byte("not audio"), 0o600)
	if _, err := ConvertVoice(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "not an audio file") {
		t.Fatalf("garbage: %v", err)
	}
	// Audio bytes ffmpeg cannot decode: its error, not a crash.
	broken := filepath.Join(dir, "broken.ogg")
	_ = os.WriteFile(broken, []byte("OggS but nothing else"), 0o600)
	if _, err := ConvertVoice(context.Background(), broken); err == nil || !strings.Contains(err.Error(), "cannot convert") {
		t.Fatalf("broken ogg: %v", err)
	}
	// A playlist pointing at a URL is never fetched.
	list := filepath.Join(dir, "list.m3u")
	_ = os.WriteFile(list, []byte("#EXTM3U\nhttp://127.0.0.1:9/x.mp3\n"), 0o600)
	if _, err := ConvertVoice(context.Background(), list); err == nil || !strings.Contains(err.Error(), "not an audio file") {
		t.Fatalf("playlist: %v", err)
	}
}

// config.json in the data dir adds or replaces the allowed folders.
func TestLoadPolicyReadsConfig(t *testing.T) {
	data := t.TempDir()
	extra := t.TempDir()
	write := func(js string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(data, "config.json"), []byte(js), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := LoadPolicy(data)
	if err != nil || !slices.Contains(p.Roots, filepath.Join(data, "media")) {
		t.Fatalf("defaults: %v %v", p.Roots, err)
	}
	file := filepath.Join(extra, "slides.pdf")
	_ = os.WriteFile(file, []byte("%PDF"), 0o600)
	if _, _, err := p.check(file); !errors.Is(err, ErrDenied) {
		t.Fatalf("outside folder allowed without config: %v", err)
	}
	write(`{"send_file_extra_roots": [` + strconv.Quote(extra) + `]}`)
	if p, err = LoadPolicy(data); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.check(file); err != nil {
		t.Fatalf("extra root: %v", err)
	}
	write(`{"send_file_roots": [` + strconv.Quote(extra) + `]}`)
	if p, err = LoadPolicy(data); err != nil || len(p.Roots) != 1 || p.Roots[0] != extra {
		t.Fatalf("replaced roots: %v %v", p.Roots, err)
	}
	for _, bad := range []string{`{"send_file_root": []}`, `{"send_file_roots": ["relative\dir"]}`, `not json`} {
		write(bad)
		if _, err := LoadPolicy(data); err == nil {
			t.Errorf("config %s accepted", bad)
		}
	}
}

// The macOS layout: Movies, the cloud drives inside ~/Library, and nothing
// else of ~/Library (browsers, mail, keychains), the daemon's own data
// included, except its media folders.
func TestPolicyMacLibrary(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "Users", "mario")
	lib := filepath.Join(home, "Library")
	p := Policy{Home: home, Library: lib, AppRoot: filepath.Join(lib, "Application Support", "WhatsAppMCP")}
	p.DataDir = filepath.Join(p.AppRoot, "prod")
	for _, d := range []string{`Library\CloudStorage\OneDrive-Personale`, `Library\CloudStorage\Dropbox`, `Library\Mobile Documents\com~apple~CloudDocs`} {
		if err := os.MkdirAll(filepath.Join(home, native(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Every home folder is allowed, so only the Library rule can refuse.
	p.Roots = append(DefaultRoots(home, p.DataDir), home)
	mk := func(rel string) string {
		t.Helper()
		path := filepath.Join(home, native(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, rel := range []string{`Library\CloudStorage\OneDrive-Personale\Lavoro\piano.xlsx`, `Library\CloudStorage\Dropbox\a.pdf`,
		`Library\Mobile Documents\com~apple~CloudDocs\Scansioni\b.pdf`, `Library\Application Support\WhatsAppMCP\prod\media\c_1\IMG-1.jpg`,
		`Library\Application Support\WhatsAppMCP\dev\media\c_2\IMG-2.jpg`, `Movies\clip.mp4`} {
		if _, _, err := p.check(mk(rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
	// Outside Windows ':' is an ordinary character in a file name.
	if runtime.GOOS != "windows" {
		if _, _, err := p.check(mk(`Documents\report: v2.pdf`)); err != nil {
			t.Errorf("colon in a name: %v", err)
		}
	}
	for _, rel := range []string{`Library\Application Support\Google\Chrome\Default\Cookies`, `Library\Keychains\login.keychain-db`,
		`Library\Mail\V10\x.emlx`, `Library\Messages\chat.txt`, `Library\Mobile Documents\com~apple~Notes\n.txt`,
		`Library\CloudStorage\notes.txt`, `Library\Application Support\WhatsAppMCP\prod\logs\whatsapp-mcp.log`,
		`Library\Application Support\WhatsAppMCP\dev\config.json`, `Library\Application Support\WhatsAppMCP\hooks\abc`} {
		if _, _, err := p.check(mk(rel)); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: err = %v, want ErrDenied", rel, err)
		}
	}
}

// A named pipe is refused at once: the open never waits for a writer.
func TestPolicyRefusesFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no named pipes in the file system on Windows")
	}
	p, mk := fakeHome(t)
	fifo := filepath.Join(filepath.Dir(mk(`Documents\x.txt`)), "pipe.pdf")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo: %v %s", err, out)
	}
	done := make(chan error, 1)
	go func() { _, _, err := p.check(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a named pipe hung")
	}
}
