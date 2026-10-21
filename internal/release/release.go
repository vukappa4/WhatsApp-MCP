// Package release reads the project's GitHub releases: the latest one or
// one by tag, its checksums.txt, and downloads a release binary verified
// against it. It sends no credentials: the repository is public.
package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ChecksumsName is the release asset listing the SHA-256 of every binary.
const ChecksumsName = "checksums.txt"

const (
	apiTimeout    = 30 * time.Second // whole API call, body included
	headerTimeout = 30 * time.Second // wait for response headers
	stallTimeout  = 60 * time.Second // a download receiving no bytes for this long is cancelled
	totalTimeout  = 15 * time.Minute // a download not finished by then is cancelled
	maxJSON       = 1 << 20
	maxChecksums  = 64 << 10
	maxAsset      = 200 << 20 // a release binary is about 40 MB
)

// githubAPI is GitHub's API. Its release assets download from github.com,
// which redirects to one of downloadHosts.
const githubAPI = "https://api.github.com"

// downloadHosts are the hosts GitHub serves release assets from: github.com
// redirects a download to release-assets.githubusercontent.com or
// objects.githubusercontent.com.
var downloadHosts = []string{"github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"}

// ErrNoRelease means the repository has no published release.
var ErrNoRelease = errors.New("the repository has no published release")

// ErrNotFound means no release has the requested tag.
var ErrNotFound = errors.New("no release has this tag")

// RateLimitError means GitHub refused the call for its rate limit (60
// calls an hour per address without login). Reset is when calls are
// allowed again; zero when GitHub did not say.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	if e.Reset.IsZero() {
		return "GitHub's API rate limit is reached"
	}
	return "GitHub's API rate limit is reached until " + e.Reset.Local().Format("15:04")
}

// NetError is a failure to reach GitHub at all: no network, DNS, TLS, a
// proxy, a timeout.
type NetError struct{ Err error }

func (e *NetError) Error() string { return "contact GitHub: " + e.Err.Error() }
func (e *NetError) Unwrap() error { return e.Err }

// StatusError is an unexpected HTTP status.
type StatusError struct {
	URL    string
	Status string
	Code   int
}

func (e *StatusError) Error() string { return fmt.Sprintf("GET %s: %s", e.URL, e.Status) }

// ChecksumError is a download whose SHA-256 differs from the one its
// source (checksums.txt or GitHub's asset digest) gives.
type ChecksumError struct{ Name, Got, Want, Source string }

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("%s: SHA-256 %s, %s says %s: the download is corrupted or was altered on the way", e.Name, e.Got, e.Source, e.Want)
}

// Client reads the releases of Repo (owner/name) from the GitHub API at
// APIBase. It takes only assets whose URL is exactly their own download
// from the release, and follows redirects only to GitHub's hosts. Any APIBase but GitHub's is
// a test server, which then also serves the downloads.
type Client struct {
	APIBase   string // https://api.github.com
	Repo      string
	UserAgent string
	HTTP      *http.Client // nil: NewHTTP(30s)

	stall, total time.Duration // tests shorten the stall and total timeouts
}

// NewHTTP is a client on a copy of the default transport, so HTTPS_PROXY
// and the system's TLS roots apply, that waits at most header for the
// response headers.
func NewHTTP(header time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = header
	return &http.Client{Transport: tr}
}

// http is the client's HTTP client with its redirect policy.
func (c *Client) http() *http.Client {
	if c.HTTP == nil {
		c.HTTP = NewHTTP(headerTimeout)
	}
	hc := *c.HTTP
	hc.CheckRedirect = c.checkRedirect
	return &hc
}

// downloadBase is where the release downloads of APIBase's repositories
// are: github.com for GitHub's API, the server itself for a test server.
func (c *Client) downloadBase() string {
	base := strings.TrimRight(c.APIBase, "/")
	if base == githubAPI {
		return "https://github.com"
	}
	return base
}

// downloadPrefix starts the URL of every asset of the release tagged tag.
func (c *Client) downloadPrefix(tag string) string {
	return c.downloadBase() + "/" + c.Repo + "/releases/download/" + tag + "/"
}

// checkRedirect follows at most 10 redirects, each to APIBase's own host
// (GitHub's API redirects a renamed repository) or over HTTPS to one of
// downloadHosts. A test server's downloads are on APIBase's host.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !c.allowedRedirect(req.URL) {
		return fmt.Errorf("refusing a redirect to %s://%s: not a GitHub host", req.URL.Scheme, req.URL.Host)
	}
	return nil
}

func (c *Client) allowedRedirect(u *url.URL) bool {
	if api, err := url.Parse(c.APIBase); err == nil && u.Scheme == api.Scheme && strings.EqualFold(u.Host, api.Host) {
		return true
	}
	if u.Scheme != "https" || u.Port() != "" {
		return false
	}
	for _, h := range downloadHosts {
		if strings.EqualFold(u.Hostname(), h) {
			return true
		}
	}
	return false
}

// Release is a published release.
type Release struct {
	Tag               string
	Prerelease, Draft bool
	Assets            []Asset
}

// Asset is a file of a release. URL is its download URL, specific to the
// release's tag. Digest is GitHub's own "sha256:<hex>", empty on releases
// published before GitHub computed digests.
type Asset struct {
	Name, URL string
	Size      int64
	Digest    string
}

// Asset returns the release's asset called name.
func (r *Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// AssetNames lists the release's assets, for an error that names what is there.
func (r *Release) AssetNames() []string {
	names := make([]string, len(r.Assets))
	for i, a := range r.Assets {
		names[i] = a.Name
	}
	return names
}

// AssetName is the release binary for an OS and architecture, as
// .goreleaser.yaml names it.
func AssetName(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "windows/amd64", "windows/arm64":
		return "whatsapp-mcp_" + goos + "_" + goarch + ".exe", nil
	case "darwin/amd64", "darwin/arm64":
		return "whatsapp-mcp_" + goos + "_" + goarch, nil
	}
	return "", fmt.Errorf("releases carry binaries for Windows and macOS (amd64, arm64), not %s/%s", goos, goarch)
}

// Latest is the newest release that is neither a draft nor a prerelease.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	r, err := c.release(ctx, "/releases/latest")
	if se := (*StatusError)(nil); errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil, ErrNoRelease
	}
	return r, err
}

// LatestStable is Latest, refused when GitHub names as the latest release a
// draft, a prerelease or a tag that is not a plain vMAJOR.MINOR.PATCH, which
// release.yml never makes the latest: update installs it, and the daemon's
// check reports it, only when it is none of these.
func (c *Client) LatestStable(ctx context.Context) (*Release, error) {
	r, err := c.Latest(ctx)
	switch {
	case err != nil:
		return nil, err
	case r.Draft || r.Prerelease:
		return nil, &UnstableError{Tag: r.Tag, Flagged: true}
	case !PlainTag(r.Tag):
		return nil, &UnstableError{Tag: r.Tag}
	}
	return r, nil
}

// UnstableError is GitHub naming as the latest release a draft or a
// prerelease (Flagged), or a tag that is not a plain vMAJOR.MINOR.PATCH.
type UnstableError struct {
	Tag     string
	Flagged bool
}

func (e *UnstableError) Error() string {
	if e.Flagged {
		return "GitHub named " + e.Tag + ", a draft or prerelease, as the latest release"
	}
	return fmt.Sprintf("GitHub named %q as the latest release, not a vMAJOR.MINOR.PATCH tag", e.Tag)
}

// PageURL is the GitHub page of the release tagged tag. It is built here,
// never taken from GitHub's answer, so the page shown is the project's own.
func (c *Client) PageURL(tag string) string {
	return "https://github.com/" + c.Repo + "/releases/tag/" + tag
}

// ByTag is the release tagged tag, prereleases included.
func (c *Client) ByTag(ctx context.Context, tag string) (*Release, error) {
	r, err := c.release(ctx, "/releases/tags/"+tag)
	if se := (*StatusError)(nil); errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil, fmt.Errorf("%s: %w", tag, ErrNotFound)
	}
	if err == nil && r.Tag != tag {
		return nil, fmt.Errorf("asked GitHub for release %s, it answered with %s", tag, r.Tag)
	}
	return r, err
}

func (c *Client) release(ctx context.Context, path string) (*Release, error) {
	var in struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
		Assets     []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	endpoint := strings.TrimRight(c.APIBase, "/") + "/repos/" + c.Repo + path
	b, err := c.get(ctx, endpoint, "application/vnd.github+json", maxJSON)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("GET %s: unexpected answer: %w", endpoint, err)
	}
	if in.TagName == "" {
		return nil, fmt.Errorf("GET %s: the answer names no tag", endpoint)
	}
	r := &Release{Tag: in.TagName, Prerelease: in.Prerelease, Draft: in.Draft}
	prefix := c.downloadPrefix(in.TagName)
	for _, a := range in.Assets {
		if !plainName(a.Name) {
			return nil, fmt.Errorf("GET %s: asset name %q is not a plain file name", endpoint, a.Name)
		}
		// Exactly the asset's own download: a prefix alone would let
		// .../download/<tag>/../../ reach any path of the host.
		file := url.PathEscape(a.Name)
		want := prefix + file
		if len(a.URL) != len(want) || !strings.EqualFold(a.URL[:len(prefix)], prefix) || !strings.HasSuffix(a.URL, file) {
			return nil, fmt.Errorf("GET %s: asset %s downloads from %s, not from %s", endpoint, a.Name, a.URL, want)
		}
		r.Assets = append(r.Assets, Asset{Name: a.Name, URL: a.URL, Size: a.Size, Digest: a.Digest})
	}
	return r, nil
}

// plainName reports whether name is a file name of its own: not empty, no
// path separator, no "..".
func plainName(name string) bool {
	return name != "" && name != "." && !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

// get fetches url within apiTimeout and returns its body, refused when
// larger than limit.
func (c *Client) get(ctx context.Context, url, accept string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	resp, err := c.do(ctx, url, accept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &NetError{err}
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: answer larger than %d bytes", url, limit)
	}
	return b, nil
}

// do sends a GET and returns the response when its status is 200 OK.
func (c *Client) do(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, &NetError{err}
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	resp.Body.Close()
	if rl := rateLimit(resp, time.Now()); rl != nil {
		return nil, rl
	}
	return nil, &StatusError{URL: url, Status: resp.Status, Code: resp.StatusCode}
}

// rateLimit reads a rate limit refusal: 403 or 429 with Retry-After, or
// with X-RateLimit-Remaining 0 and the reset time in X-RateLimit-Reset. A
// 429 is one in any case; a plain 403 is not.
func rateLimit(resp *http.Response, now time.Time) *RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return &RateLimitError{Reset: now.Add(time.Duration(n) * time.Second)}
		}
		if t, err := http.ParseTime(s); err == nil {
			return &RateLimitError{Reset: t}
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		e := &RateLimitError{}
		if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			e.Reset = time.Unix(n, 0)
		}
		return e
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{}
	}
	return nil
}

// Checksums downloads the release's checksums.txt: asset name to
// lowercase SHA-256.
func (c *Client) Checksums(ctx context.Context, r *Release) (map[string]string, error) {
	a, ok := r.Asset(ChecksumsName)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s", r.Tag, ChecksumsName)
	}
	b, err := c.get(ctx, a.URL, "application/octet-stream", maxChecksums)
	if err != nil {
		return nil, err
	}
	sums, err := parseChecksums(b)
	if err != nil {
		return nil, fmt.Errorf("%s of %s: %w", ChecksumsName, r.Tag, err)
	}
	return sums, nil
}

// parseChecksums reads sha256sum output: "<hex>  <name>" (text mode) or
// "<hex> *<name>" (binary mode), LF or CRLF, hex in either case. A line in
// another form, or a name listed twice with different sums, is an error.
func parseChecksums(b []byte) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			continue
		}
		sum, rest, ok := strings.Cut(line, " ")
		if !ok || len(sum) != sha256.Size*2 || len(rest) < 2 || (rest[0] != ' ' && rest[0] != '*') {
			return nil, fmt.Errorf("line %d is not \"<sha256>  <name>\": %q", n, line)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("line %d: %q is not hex", n, sum)
		}
		sum, name := strings.ToLower(sum), rest[1:]
		if prev, ok := sums[name]; ok && prev != sum {
			return nil, fmt.Errorf("%s is listed twice with different sums", name)
		}
		sums[name] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sums, nil
}

// Download writes asset a to dst, a new file (mode 0700) it creates, and
// checks it: exactly a.Size bytes, SHA-256 wantSHA, and GitHub's digest of
// the asset when the release has one. It refuses an asset listed larger
// than 200 MB, and cancels a download receiving nothing for 60 seconds or
// not finished within 15 minutes. On any failure dst is removed.
func (c *Client) Download(ctx context.Context, a Asset, dst, wantSHA string) (err error) {
	if a.Size <= 0 {
		return fmt.Errorf("%s: the release lists no size", a.Name)
	}
	if a.Size > maxAsset {
		return fmt.Errorf("%s: the release lists %d bytes, more than the %d MB a release binary may have", a.Name, a.Size, maxAsset>>20)
	}
	want := strings.ToLower(wantSHA)
	if b, herr := hex.DecodeString(want); herr != nil || len(b) != sha256.Size {
		return fmt.Errorf("%s: %q is not a SHA-256", a.Name, wantSHA)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
		if err != nil {
			os.Remove(dst)
		}
	}()

	stall, total := c.stall, c.total
	if stall == 0 {
		stall = stallTimeout
	}
	if total == 0 {
		total = totalTimeout
	}
	errStalled, errTooLong := errors.New("stalled"), errors.New("too long")
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ctx, cancelTotal := context.WithTimeoutCause(ctx, total, errTooLong)
	defer cancelTotal()
	watchdog := time.AfterFunc(stall, func() { cancel(errStalled) })
	defer watchdog.Stop()
	failed := func(err error) error {
		switch context.Cause(ctx) {
		case errStalled:
			return fmt.Errorf("download %s: nothing received for %s", a.Name, stall)
		case errTooLong:
			return fmt.Errorf("download %s: not finished within %s", a.Name, total)
		}
		var ne *NetError
		if errors.As(err, &ne) {
			return err
		}
		return &NetError{err}
	}

	resp, err := c.do(ctx, a.URL, "application/octet-stream")
	if err != nil {
		var se *StatusError
		var rl *RateLimitError
		if errors.As(err, &se) || errors.As(err, &rl) {
			return err
		}
		return failed(err)
	}
	defer resp.Body.Close()
	h := sha256.New()
	body := &progressReader{r: resp.Body, progress: func() { watchdog.Reset(stall) }}
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, a.Size+1))
	if err != nil {
		return failed(err)
	}
	if n != a.Size {
		return fmt.Errorf("download %s: got %d bytes, the release lists %d", a.Name, n, a.Size)
	}
	closed = true
	if err := f.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return &ChecksumError{Name: a.Name, Got: got, Want: want, Source: ChecksumsName}
	}
	if d, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && !strings.EqualFold(d, got) {
		return &ChecksumError{Name: a.Name, Got: got, Want: strings.ToLower(d), Source: "GitHub's digest of the asset"}
	}
	return nil
}

// progressReader calls progress whenever a read returns bytes.
type progressReader struct {
	r        io.Reader
	progress func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress()
	}
	return n, err
}
