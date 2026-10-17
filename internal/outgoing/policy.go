// Package outgoing prepares local files for sending: which paths may be
// read, which WhatsApp message kind a file becomes, size limits, voice-note
// conversion to Opus and image metadata.
package outgoing

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

// Policy decides which local files the send tools may read. A file must be
// inside one of Roots (an allowlist), and even there the denied directories
// and file names stay refused: secrets, keys, databases, repositories,
// browser profiles and the daemon's own data (its media folder excepted).
type Policy struct {
	Home         string // the user's home folder
	LocalAppData string // %LOCALAPPDATA% on Windows
	AppData      string // %APPDATA% on Windows
	// Library is ~/Library on macOS: app data, mail, keychains, cookies and
	// browser profiles. Only its cloud drive folders and the daemon's media
	// folders may be sent from.
	Library string
	AppRoot string // config.AppDir: every profile's data, the hooks and the tools
	DataDir string // the profile data dir
	// Roots are the folders files may be sent from. Empty allows nothing.
	Roots []string
	// Scratch are the folders where Claude Code keeps its sessions' temp
	// files: only their "scratchpad" folders (where sessions write files
	// they make) may be sent from. The rest holds session task transcripts.
	Scratch []string
}

// VideosFolder is the home folder for videos: Movies on macOS, Videos elsewhere.
var VideosFolder = map[bool]string{true: "Movies", false: "Videos"}[runtime.GOOS == "darwin"]

// cloudDrives are home-relative patterns of synced cloud folders: OneDrive
// on Windows; on macOS every File Provider drive (OneDrive, Dropbox, Google
// Drive, Box) and iCloud Drive, all inside ~/Library.
var (
	cloudDrives        = []string{"OneDrive*"}
	libraryCloudDrives = []string{filepath.Join("CloudStorage", "*"), filepath.Join("Mobile Documents", "com~apple~CloudDocs")}
)

// DefaultRoots are the folders a user sends files from: the profile's
// Desktop, Documents, Downloads, Pictures, Videos (Movies on macOS) and
// Music, every cloud drive folder in it and the daemon's media folder.
// Files Claude generates are allowed through Policy.Scratch.
func DefaultRoots(home, dataDir string) []string {
	var out []string
	if home != "" {
		for _, d := range []string{"Desktop", "Documents", "Downloads", "Pictures", VideosFolder, "Music"} {
			out = append(out, filepath.Join(home, d))
		}
		pats := slices.Clone(cloudDrives)
		if runtime.GOOS == "darwin" {
			for _, pat := range libraryCloudDrives {
				pats = append(pats, filepath.Join("Library", pat))
			}
		}
		for _, pat := range pats {
			ds, _ := filepath.Glob(filepath.Join(home, pat))
			for _, d := range ds {
				if fi, err := os.Stat(d); err == nil && fi.IsDir() {
					out = append(out, d)
				}
			}
		}
	}
	if dataDir != "" {
		out = append(out, filepath.Join(dataDir, "media"))
	}
	return out
}

// DefaultScratch lists where Claude Code keeps session temp files, whose
// scratchpad folders hold the files sessions generate: <temp>\claude on
// Windows; on macOS and Linux also claude-<uid> in temp and in /tmp.
func DefaultScratch(temp string) []string {
	var out []string
	if temp != "" {
		out = append(out, filepath.Join(temp, "claude"))
	}
	if runtime.GOOS != "windows" {
		uid := "claude-" + strconv.Itoa(os.Getuid())
		for _, t := range []string{temp, "/tmp"} {
			if t != "" {
				out = append(out, filepath.Join(t, uid))
			}
		}
	}
	return out
}

// LoadPolicy builds the policy of a profile from the environment and the
// profile's config.json (send_file_roots replaces the default folders,
// send_file_extra_roots adds to them). The OS's known folders (on Windows a
// Documents folder moved to another drive) join the defaults.
func LoadPolicy(dataDir string) (Policy, error) {
	home, _ := os.UserHomeDir()
	p := Policy{Home: home, LocalAppData: os.Getenv("LOCALAPPDATA"), AppData: os.Getenv("APPDATA"), DataDir: dataDir}
	p.AppRoot, _ = config.AppDir()
	if runtime.GOOS == "darwin" && home != "" {
		p.Library = filepath.Join(home, "Library")
	}
	s, err := config.LoadSettings(dataDir)
	if err != nil {
		return p, err
	}
	if len(s.SendFileRoots) > 0 {
		p.Roots = slices.Clone(s.SendFileRoots)
	} else {
		p.Roots = append(DefaultRoots(home, dataDir), knownFolders()...)
		p.Scratch = append(DefaultScratch(os.TempDir()), userTempScratch()...)
	}
	p.Roots = append(p.Roots, s.SendFileExtraRoots...)
	return p, nil
}

// deniedNames are base-name patterns (case-insensitive, filepath.Match syntax).
var deniedNames = []string{"*.pem", "*.key", "id_*", ".env*", "token", "session.db*", "whatsapp.db*",
	".git-credentials", ".netrc", "_netrc", ".npmrc", ".pypirc", "*.kdbx", "*.pfx", "*.p12", "*.ppk", "*.ovpn",
	"*.db", "*.db-*", "*.sqlite", "*.sqlite3", "*.sqlite-*", "*.sqlite3-*"}

// deniedDirs are folder names refused anywhere in a path (case-insensitive):
// repositories and tool credentials.
var deniedDirs = []string{".git", ".ssh", ".aws", ".azure", ".docker", ".kube", ".gnupg"}

// ErrDenied marks a path the policy refuses.
var ErrDenied = errors.New("path not allowed")

// browserDirs are profile directories holding cookies, passwords and tokens.
func (p Policy) browserDirs() []string {
	var out []string
	if p.LocalAppData != "" {
		for _, d := range []string{`Google\Chrome\User Data`, `Google\Chrome Beta\User Data`, `Microsoft\Edge\User Data`,
			`BraveSoftware\Brave-Browser\User Data`, `Chromium\User Data`, `Vivaldi\User Data`, `Mozilla\Firefox\Profiles`} {
			out = append(out, filepath.Join(p.LocalAppData, d))
		}
	}
	if p.AppData != "" {
		for _, d := range []string{`Mozilla\Firefox\Profiles`, `Opera Software`, `Thunderbird\Profiles`} {
			out = append(out, filepath.Join(p.AppData, d))
		}
	}
	return out
}

// Opened is a file the policy allowed, kept open: everything sent is read
// from this handle, so the file checked is the file sent.
type Opened struct {
	*os.File
	Final string // the OS's final path of the handle
	Size  int64
}

// Open opens path and returns the handle when the policy allows reading it.
// The name rules run on the path as given; then the file is opened once and
// the OS's final path of that handle (every symlink, junction, mount point
// and 8.3 name resolved) must pass the name rules and lie inside an allowed
// folder. The caller reads from the returned handle and closes it.
func (p Policy) Open(path string) (*Opened, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("path is required: an absolute path to a local file")
	}
	clean := filepath.Clean(path)
	if err := localPath(path, clean); err != nil {
		return nil, err
	}
	if err := p.denied(clean); err != nil {
		return nil, err
	}
	f, err := openNoBlock(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file not found: %s", clean)
		}
		if runtime.GOOS == "darwin" && errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("cannot read %s: %v. macOS may not have given whatsapp-mcp access to this folder: "+
				"allow it in System Settings > Privacy & Security > Files and Folders (or Full Disk Access), then try again", clean, err)
		}
		return nil, fmt.Errorf("cannot read %s: %v", clean, err)
	}
	o, err := p.checkOpened(f, clean)
	if err != nil {
		f.Close()
		return nil, err
	}
	return o, nil
}

func (p Policy) checkOpened(f *os.File, clean string) (*Opened, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", clean, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", clean)
	}
	final, links, err := finalPath(f)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %v", clean, err)
	}
	if err := localPath(final, filepath.Clean(final)); err != nil {
		return nil, fmt.Errorf("%s leads to %s: %w", clean, final, err)
	}
	if err := p.denied(final); err != nil {
		return nil, err
	}
	if links > 1 {
		return nil, fmt.Errorf("%w: %s has %d hard links, so its other names cannot be checked; copy it first", ErrDenied, clean, links)
	}
	if !p.allowed(final) {
		return nil, fmt.Errorf("%w: %s is outside the folders files may be sent from (%s); copy it into one of them, "+
			"or add its folder to send_file_extra_roots in %s", ErrDenied, final, p.rootList(), config.SettingsFile(p.DataDir))
	}
	return &Opened{File: f, Final: final, Size: st.Size()}, nil
}

// allowed reports whether final lies inside an allowed folder, comparing
// against each folder as written and as resolved (a folder that is itself
// a junction or redirected).
func (p Policy) allowed(final string) bool {
	for _, r := range p.Roots {
		if r == "" {
			continue
		}
		if under(final, r) {
			return true
		}
		if real, err := filepath.EvalSymlinks(r); err == nil && under(final, real) {
			return true
		}
	}
	for _, sc := range p.Scratch {
		if sc == "" {
			continue
		}
		for _, s := range []string{sc, evalOr(sc)} {
			if rel, ok := relUnder(final, s); ok {
				for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
					if strings.EqualFold(part, "scratchpad") {
						return true
					}
				}
			}
		}
	}
	return false
}

func evalOr(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

func (p Policy) rootList() string {
	roots := slices.Clone(p.Roots)
	for _, sc := range p.Scratch {
		if sc != "" {
			roots = append(roots, filepath.Join(sc, "...", "scratchpad"))
		}
	}
	if len(roots) == 0 {
		return "none configured"
	}
	return strings.Join(roots, ", ")
}

// ExamplePath is an absolute path in this OS's form, for messages.
var ExamplePath = map[bool]string{true: `C:\Users\me\Documents\offerta.pdf`, false: "/Users/me/Documents/offerta.pdf"}[runtime.GOOS == "windows"]

// localPath accepts only a plain absolute path on a local drive: no UNC or
// device paths (\\server\share, \\?\C:, \??\C:, //server), and on Windows no
// alternate data streams. Outside Windows ':' is an ordinary character.
func localPath(raw, clean string) error {
	slash := func(c byte) bool { return c == '\\' || c == '/' }
	for _, s := range []string{raw, clean} {
		if len(s) >= 2 && slash(s[0]) && (slash(s[1]) || s[1] == '?') {
			return fmt.Errorf("%w: network and device paths (%s) are not allowed; copy the file to a local folder first", ErrDenied, raw)
		}
	}
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("path must be absolute, e.g. %s; got %q", ExamplePath, raw)
	}
	if runtime.GOOS != "windows" {
		return nil
	}
	vol := filepath.VolumeName(clean)
	if len(vol) != 2 || vol[1] != ':' {
		return fmt.Errorf("%w: %s is not on a local drive letter", ErrDenied, raw)
	}
	if strings.Contains(clean[len(vol):], ":") {
		return fmt.Errorf("%w: alternate data streams (':' in %s) are not allowed", ErrDenied, raw)
	}
	return nil
}

// denied applies the directory and name rules to one absolute path.
func (p Policy) denied(path string) error {
	base := strings.ToLower(filepath.Base(path))
	for _, pat := range deniedNames {
		if ok, _ := filepath.Match(pat, base); ok {
			return fmt.Errorf("%w: files named like %s may hold secrets, keys or databases (%s)", ErrDenied, pat, path)
		}
	}
	dirs := strings.Split(filepath.Dir(filepath.Clean(path)), string(filepath.Separator))
	for _, d := range dirs {
		if slices.Contains(deniedDirs, strings.ToLower(d)) {
			return fmt.Errorf("%w: %s is inside a %s folder (repository or credentials)", ErrDenied, path, d)
		}
	}
	ownErr := fmt.Errorf("%w: %s is inside the WhatsApp MCP data dir; only its media folder may be sent", ErrDenied, path)
	if p.AppRoot != "" && under(path, p.AppRoot) && !underAnyMedia(path, p.AppRoot) {
		return ownErr
	}
	ownMedia := p.DataDir != "" && under(path, filepath.Join(p.DataDir, "media"))
	if p.DataDir != "" && under(path, p.DataDir) && !ownMedia {
		return ownErr
	}
	if p.Library != "" && under(path, p.Library) && !ownMedia && !(p.AppRoot != "" && underAnyMedia(path, p.AppRoot)) && !p.inLibraryCloud(path) {
		return fmt.Errorf("%w: %s holds app data, mail, keychains and browser profiles; only its cloud drive folders may be sent from", ErrDenied, p.Library)
	}
	if p.Home != "" {
		if under(path, filepath.Join(p.Home, ".config")) {
			return fmt.Errorf("%w: %s holds credentials", ErrDenied, filepath.Join(p.Home, ".config"))
		}
		if rel, ok := relUnder(path, p.Home); ok {
			first := strings.ToLower(strings.SplitN(rel, string(filepath.Separator), 2)[0])
			if strings.HasPrefix(first, ".claude") {
				return fmt.Errorf("%w: %s holds Claude Code settings and credentials", ErrDenied, filepath.Join(p.Home, first))
			}
		}
	}
	for _, d := range p.browserDirs() {
		if under(path, d) {
			return fmt.Errorf("%w: %s is a browser profile", ErrDenied, d)
		}
	}
	return nil
}

// inLibraryCloud reports whether path is inside a cloud drive folder of
// Library (CloudStorage/<drive>, iCloud Drive).
func (p Policy) inLibraryCloud(path string) bool {
	rel, ok := relUnder(path, p.Library)
	if !ok {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, pat := range libraryCloudDrives {
		n := len(strings.Split(pat, string(filepath.Separator)))
		if len(parts) <= n {
			continue
		}
		if m, _ := filepath.Match(strings.ToLower(pat), strings.ToLower(filepath.Join(parts[:n]...))); m {
			return true
		}
	}
	return false
}

// underAnyMedia reports whether path is inside <root>/<profile>/media for any profile.
func underAnyMedia(path, root string) bool {
	rel, ok := relUnder(path, root)
	if !ok {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) >= 3 && strings.EqualFold(parts[1], "media")
}

// relUnder returns path relative to dir when path is strictly inside dir
// (case-insensitive). It compares folder by folder: lowercasing can change a
// name's byte length ("İ" becomes "i" plus a combining dot), so a byte
// offset in a lowercased copy is no offset in the original.
func relUnder(path, dir string) (string, bool) {
	sep := string(filepath.Separator)
	pp := strings.Split(strings.TrimSuffix(filepath.Clean(path), sep), sep)
	dp := strings.Split(strings.TrimSuffix(filepath.Clean(dir), sep), sep)
	if len(pp) <= len(dp) {
		return "", false
	}
	for i := range dp {
		if !strings.EqualFold(pp[i], dp[i]) {
			return "", false
		}
	}
	return strings.Join(pp[len(dp):], sep), true
}

func under(path, dir string) bool {
	if strings.EqualFold(filepath.Clean(path), filepath.Clean(dir)) {
		return true
	}
	_, ok := relUnder(path, dir)
	return ok
}
