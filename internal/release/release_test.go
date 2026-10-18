package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const repo = "vukappa4/WhatsApp-MCP"

func client(srv *httptest.Server) *Client {
	return &Client{APIBase: srv.URL, Repo: repo, UserAgent: "whatsapp-mcp/test (+https://github.com/vukappa4/WhatsApp-MCP)"}
}

// latestJSON is GitHub's answer for release v0.3.0, its downloads under
// base (%[1]s).
const latestJSON = `{"tag_name":"v0.3.0","prerelease":false,"draft":false,"html_url":"https://github.com/vukappa4/WhatsApp-MCP/releases/tag/v0.3.0",
"assets":[{"name":"whatsapp-mcp_windows_amd64.exe","browser_download_url":"%[1]s/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/whatsapp-mcp_windows_amd64.exe","size":37,"digest":"sha256:ab"},
{"name":"checksums.txt","browser_download_url":"%[1]s/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/checksums.txt","size":10,"digest":null}],"body":"notes"}`

// Latest asks the documented endpoint with GitHub's headers, a user agent
// naming the program, and no credentials even when GITHUB_TOKEN is set.
func TestLatest(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		fmt.Fprintf(w, latestJSON, "http://"+r.Host)
	}))
	defer srv.Close()
	r, err := client(srv).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/repos/vukappa4/WhatsApp-MCP/releases/latest" || got.Header.Get("Accept") != "application/vnd.github+json" ||
		got.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || !strings.HasPrefix(got.Header.Get("User-Agent"), "whatsapp-mcp/") ||
		got.Header.Get("Authorization") != "" {
		t.Fatalf("request %s %v", got.URL, got.Header)
	}
	if r.Tag != "v0.3.0" || r.Prerelease || r.Draft || !strings.HasSuffix(r.HTMLURL, "/tag/v0.3.0") || len(r.Assets) != 2 {
		t.Fatalf("release %+v", r)
	}
	a, ok := r.Asset("whatsapp-mcp_windows_amd64.exe")
	if !ok || a.Size != 37 || a.Digest != "sha256:ab" || a.URL != srv.URL+"/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/whatsapp-mcp_windows_amd64.exe" {
		t.Fatalf("asset %+v", a)
	}
	if c, _ := r.Asset(ChecksumsName); c.Digest != "" {
		t.Fatalf("null digest = %q", c.Digest)
	}
	if strings.Join(r.AssetNames(), ",") != "whatsapp-mcp_windows_amd64.exe,checksums.txt" {
		t.Fatalf("names %v", r.AssetNames())
	}
}

func TestByTagAndNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vukappa4/WhatsApp-MCP/releases/tags/v0.3.0-rc.1" {
			fmt.Fprint(w, `{"tag_name":"v0.3.0-rc.1","prerelease":true,"assets":[]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := client(srv)
	r, err := c.ByTag(context.Background(), "v0.3.0-rc.1")
	if err != nil || r.Tag != "v0.3.0-rc.1" || !r.Prerelease {
		t.Fatalf("ByTag = %+v, %v", r, err)
	}
	if _, err := c.ByTag(context.Background(), "v9.9.9"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "v9.9.9") {
		t.Fatalf("missing tag: %v", err)
	}
	if _, err := c.Latest(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("no release: %v", err)
	}
}

// ByTag refuses an answer for another release than the one asked for.
func TestByTagRefusesAnotherRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v0.4.0","assets":[]}`)
	}))
	defer srv.Close()
	if r, err := client(srv).ByTag(context.Background(), "v0.3.0"); err == nil || err.Error() != "asked GitHub for release v0.3.0, it answered with v0.4.0" {
		t.Fatalf("ByTag = %+v, %v", r, err)
	}
}

// Every asset must download from exactly its own URL among the repository's
// release downloads for the release's tag: on GitHub
// https://github.com/<repo>/releases/download/<tag>/<name>.
func TestAssetsMustBeTheReleasesDownloads(t *testing.T) {
	gh := &Client{APIBase: "https://api.github.com", Repo: repo}
	if got, want := gh.downloadPrefix("v0.3.0"), "https://github.com/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/"; got != want {
		t.Fatalf("GitHub's download prefix = %s, want %s", got, want)
	}
	for name, url := range map[string]string{
		"other host":   "https://example.invalid/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/x.exe",
		"other repo":   "{base}/someone/WhatsApp-MCP/releases/download/v0.3.0/x.exe",
		"other tag":    "{base}/vukappa4/WhatsApp-MCP/releases/download/v0.2.0/x.exe",
		"not download": "{base}/vukappa4/WhatsApp-MCP/archive/v0.3.0/x.exe",
		"prefix only":  "{base}/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/",
		"https":        "https://{host}/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/x.exe",
		"dot dot":      "{base}/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/../../../../attacker/repo/releases/download/v0.3.0/x.exe",
		"other asset":  "{base}/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/y.exe",
		"subfolder":    "{base}/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/sub/x.exe",
		"query":        "{base}/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/x.exe?x=1",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				u := strings.NewReplacer("{base}", "http://"+r.Host, "{host}", r.Host).Replace(url)
				fmt.Fprintf(w, `{"tag_name":"v0.3.0","assets":[{"name":"x.exe","browser_download_url":%q,"size":1}]}`, u)
			}))
			defer srv.Close()
			if r, err := client(srv).Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "asset x.exe downloads from ") {
				t.Fatalf("Latest = %+v, %v", r, err)
			}
		})
	}
	// An asset name with a path in it is refused, even with a URL that
	// spells the name out under the release's downloads.
	for _, name := range []string{"..", ".", "", "../x.exe", "x..exe", "a/x.exe", `a\x.exe`, "../../../../attacker/repo/releases/download/v0.3.0/x.exe"} {
		t.Run("name "+name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				u := "http://" + r.Host + "/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/" + name
				fmt.Fprintf(w, `{"tag_name":"v0.3.0","assets":[{"name":%q,"browser_download_url":%q,"size":1}]}`, name, u)
			}))
			defer srv.Close()
			if r, err := client(srv).Latest(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("asset name %q is not a plain file name", name)) {
				t.Fatalf("Latest = %+v, %v", r, err)
			}
		})
	}
	// A name GitHub escapes in the URL is still its own download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v0.3.0","assets":[{"name":"a b.exe","browser_download_url":"http://%s/vukappa4/WhatsApp-MCP/releases/download/v0.3.0/a%%20b.exe","size":1}]}`, r.Host)
	}))
	defer srv.Close()
	if r, err := client(srv).Latest(context.Background()); err != nil || len(r.Assets) != 1 {
		t.Fatalf("escaped name: %+v, %v", r, err)
	}
}

// Redirects go only to GitHub's API and download hosts over HTTPS, or to
// the test server APIBase names.
func TestRedirects(t *testing.T) {
	gh := &Client{APIBase: "https://api.github.com", Repo: repo}
	for u, want := range map[string]bool{
		"https://github.com/vukappa4/WhatsApp-MCP/releases/download/v1.0.0/x":                    true,
		"https://release-assets.githubusercontent.com/github-production-release-asset/1/2?sig=x": true,
		"https://objects.githubusercontent.com/github-production-release-asset-2e65be/1/2":       true,
		"https://api.github.com/repositories/1/releases/latest":                                  true,
		"https://GitHub.com/x":                          true,
		"http://github.com/x":                           false,
		"http://release-assets.githubusercontent.com/x": false,
		"https://github.com:8443/x":                     false,
		"https://github.com.evil.example/x":             false,
		"https://evil.example/x":                        false,
		"https://raw.githubusercontent.com/x":           false,
		"https://uploads.github.com/x":                  false,
	} {
		pu, err := neturl.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		if got := gh.allowedRedirect(pu); got != want {
			t.Errorf("redirect to %s allowed = %v, want %v", u, got, want)
		}
	}

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			hits++
			w.Write([]byte("x"))
		case "/" + repo + "/releases/download/v1.0.0/same":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/" + repo + "/releases/download/v1.0.0/away":
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
		}
	}))
	defer srv.Close()
	c := client(srv)
	base := srv.URL + "/" + repo + "/releases/download/v1.0.0/"
	if err := c.Download(context.Background(), Asset{Name: "x", URL: base + "same", Size: 1}, filepath.Join(t.TempDir(), "x"), sha([]byte("x"))); err != nil || hits != 1 {
		t.Fatalf("redirect on the test server: %v, %d hits", err, hits)
	}
	for _, to := range []string{"https://evil.example/x", "http://github.com/x", "http://localhost:1/x"} {
		dst := filepath.Join(t.TempDir(), "x")
		err := c.Download(context.Background(), Asset{Name: "x", URL: base + "away?to=" + neturl.QueryEscape(to), Size: 1}, dst, sha([]byte("x")))
		if err == nil || !strings.Contains(err.Error(), "refusing a redirect to ") {
			t.Errorf("redirect to %s: %v", to, err)
		}
		if _, serr := os.Stat(dst); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("refused redirect left %s", dst)
		}
	}
	if hits != 1 {
		t.Fatalf("a refused redirect reached the server: %d hits", hits)
	}
}

// GitHub's primary limit answers 403 or 429 with X-RateLimit-Remaining 0
// and the reset time; the secondary one Retry-After. A plain 403 is not a
// rate limit.
func TestRateLimit(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	for _, tc := range []struct {
		name    string
		code    int
		headers map[string]string
		limited bool
		reset   time.Time
	}{
		{"403 remaining 0", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(reset.Unix())}, true, reset},
		{"429 remaining 0", 429, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(reset.Unix())}, true, reset},
		{"429 retry-after", 429, map[string]string{"Retry-After": "120"}, true, time.Now().Add(2 * time.Minute)},
		{"403 retry-after", 403, map[string]string{"Retry-After": "60"}, true, time.Now().Add(time.Minute)},
		{"429 bare", 429, nil, true, time.Time{}},
		{"plain 403", 403, map[string]string{"X-RateLimit-Remaining": "12"}, false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			_, err := client(srv).Latest(context.Background())
			var rl *RateLimitError
			if errors.As(err, &rl) != tc.limited {
				t.Fatalf("err = %v (%T), limited want %v", err, err, tc.limited)
			}
			if !tc.limited {
				var se *StatusError
				if !errors.As(err, &se) || se.Code != 403 {
					t.Fatalf("err = %v, want a 403 StatusError", err)
				}
				return
			}
			if d := rl.Reset.Sub(tc.reset); d < -5*time.Second || d > 5*time.Second || rl.Reset.IsZero() != tc.reset.IsZero() {
				t.Fatalf("reset %v, want about %v", rl.Reset, tc.reset)
			}
		})
	}
}

func TestBadAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{"5xx", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 502) }, "502"},
		{"malformed", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"tag_name":`) }, "unexpected answer"},
		{"no tag", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"assets":[]}`) }, "names no tag"},
		{"oversized", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v1.0.0","body":"`+strings.Repeat("x", maxJSON)+`"}`)
		}, "larger than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			if _, err := client(srv).Latest(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// A server that is gone or never answers is a NetError: the user is told
// to check the network or the proxy.
func TestNetworkFailures(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c := client(srv)
	srv.Close()
	var ne *NetError
	if _, err := c.Latest(context.Background()); !errors.As(err, &ne) || !strings.HasPrefix(err.Error(), "contact GitHub: ") {
		t.Fatalf("closed server: %v", err)
	}

	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(block)
	c = client(slow)
	c.HTTP = NewHTTP(200 * time.Millisecond)
	start := time.Now()
	if _, err := c.Latest(context.Background()); !errors.As(err, &ne) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("silent server: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("header timeout took %s", took)
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// downloadServer serves body at /bin, announcing length when it is not negative.
func downloadServer(t *testing.T, h http.HandlerFunc) (*Client, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return client(srv), srv.URL + "/bin"
}

func TestDownload(t *testing.T) {
	body := bytes.Repeat([]byte("binary "), 10000)
	good := sha(body)
	serve := func(b []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("credentials sent")
			}
			w.Write(b)
		}
	}
	flipped := append([]byte(nil), body...)
	flipped[100] ^= 1
	for _, tc := range []struct {
		name   string
		h      http.HandlerFunc
		size   int64
		want   string
		digest string
		err    string
	}{
		{"ok", serve(body), int64(len(body)), good, "", ""},
		{"ok with digest", serve(body), int64(len(body)), strings.ToUpper(good), "sha256:" + strings.ToUpper(good), ""},
		{"sha mismatch", serve(flipped), int64(len(body)), good, "", "checksums.txt says " + good + ": the download is corrupted or was altered on the way"},
		{"short body", serve(body[:len(body)-1]), int64(len(body)), good, "", "the release lists"},
		{"long body", serve(append(body, 'x')), int64(len(body)), good, "", "the release lists"},
		{"digest mismatch", serve(body), int64(len(body)), good, "sha256:" + sha([]byte("other")), "GitHub's digest of the asset says"},
		{"404", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, int64(len(body)), good, "", "404"},
		{"no size", serve(body), 0, good, "", "no size"},
		{"over 200 MB", serve(body), 200<<20 + 1, good, "", "the release lists 209715201 bytes, more than the 200 MB a release binary may have"},
		{"bad want", serve(body), int64(len(body)), "abc", "", "not a SHA-256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, url := downloadServer(t, tc.h)
			dst := filepath.Join(t.TempDir(), "whatsapp-mcp.exe")
			err := c.Download(context.Background(), Asset{Name: "whatsapp-mcp.exe", URL: url, Size: tc.size, Digest: tc.digest}, dst, tc.want)
			if tc.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				if b, _ := os.ReadFile(dst); !bytes.Equal(b, body) {
					t.Fatal("content differs")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if _, serr := os.Stat(dst); !errors.Is(serr, os.ErrNotExist) {
				t.Fatalf("failed download left %s: %v", dst, serr)
			}
		})
	}
}

// Download never overwrites an existing file.
func TestDownloadRefusesExistingFile(t *testing.T) {
	c, url := downloadServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) })
	dst := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(dst, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Download(context.Background(), Asset{Name: "f", URL: url, Size: 1}, dst, sha([]byte("x"))); err == nil {
		t.Fatal("existing file overwritten")
	}
	if b, _ := os.ReadFile(dst); string(b) != "keep" {
		t.Fatalf("existing file is now %q", b)
	}
}

// A download that stops sending is cancelled after the stall timeout; a
// slow but moving one goes on until the total timeout.
func TestDownloadStall(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c, url := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write(make([]byte, 10))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c.stall = 300 * time.Millisecond
	dst := filepath.Join(t.TempDir(), "f")
	start := time.Now()
	err := c.Download(context.Background(), Asset{Name: "f", URL: url, Size: 1000}, dst, sha(make([]byte, 1000)))
	if err == nil || !strings.Contains(err.Error(), "nothing received for 300ms") {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("stall detected after %s", took)
	}
	if _, serr := os.Stat(dst); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("stalled download left %s", dst)
	}

	body := make([]byte, 20)
	c, url = downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		for i := range body {
			w.Write(body[i : i+1])
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})
	c.stall = 300 * time.Millisecond
	dst = filepath.Join(t.TempDir(), "g")
	if err := c.Download(context.Background(), Asset{Name: "g", URL: url, Size: 20}, dst, sha(body)); err != nil {
		t.Fatalf("slow but moving download: %v", err)
	}

	// The same download is cancelled once the total time passes.
	c.total = 300 * time.Millisecond
	dst = filepath.Join(t.TempDir(), "h")
	start = time.Now()
	err = c.Download(context.Background(), Asset{Name: "h", URL: url, Size: 20}, dst, sha(body))
	if err == nil || err.Error() != "download h: not finished within 300ms" {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("total timeout hit after %s", took)
	}
	if _, serr := os.Stat(dst); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("cancelled download left %s", dst)
	}
}

func TestChecksums(t *testing.T) {
	a, b := sha([]byte("a")), sha([]byte("b"))
	for _, tc := range []struct {
		name, in string
		want     map[string]string
		err      string
	}{
		{"goreleaser", a + "  whatsapp-mcp_darwin_arm64\n" + b + "  whatsapp-mcp_windows_amd64.exe\n",
			map[string]string{"whatsapp-mcp_darwin_arm64": a, "whatsapp-mcp_windows_amd64.exe": b}, ""},
		{"binary mode, CRLF, uppercase", strings.ToUpper(a) + " *x.exe\r\n\r\n", map[string]string{"x.exe": a}, ""},
		{"identical duplicate", a + "  x\n" + a + "  x\n", map[string]string{"x": a}, ""},
		{"conflicting duplicate", a + "  x\n" + b + "  x\n", nil, "listed twice"},
		{"one space", a + " x\n", nil, "line 1"},
		{"short hex", a[:63] + "  x\n", nil, "line 1"},
		{"not hex", strings.Repeat("g", 64) + "  x\n", nil, "not hex"},
		{"no name", a + "  \n", nil, "line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChecksums([]byte(tc.in))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestChecksumsDownload(t *testing.T) {
	sum := sha([]byte("bin"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprintf(w, "%s  whatsapp-mcp_darwin_arm64\n", sum)
		case "/big":
			w.Write(bytes.Repeat([]byte("\n"), maxChecksums+1))
		}
	}))
	defer srv.Close()
	c := client(srv)
	r := &Release{Tag: "v1.0.0", Assets: []Asset{{Name: ChecksumsName, URL: srv.URL + "/ok"}}}
	if got, err := c.Checksums(context.Background(), r); err != nil || got["whatsapp-mcp_darwin_arm64"] != sum {
		t.Fatalf("Checksums = %v, %v", got, err)
	}
	r.Assets[0].URL = srv.URL + "/big"
	if _, err := c.Checksums(context.Background(), r); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized checksums: %v", err)
	}
	if _, err := c.Checksums(context.Background(), &Release{Tag: "v1.0.0"}); err == nil || !strings.Contains(err.Error(), "has no checksums.txt") {
		t.Fatalf("missing checksums: %v", err)
	}
}

func TestAssetName(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"windows", "amd64"}: "whatsapp-mcp_windows_amd64.exe", {"windows", "arm64"}: "whatsapp-mcp_windows_arm64.exe",
		{"darwin", "arm64"}: "whatsapp-mcp_darwin_arm64", {"darwin", "amd64"}: "whatsapp-mcp_darwin_amd64",
		{"linux", "amd64"}: "", {"windows", "386"}: "",
	} {
		got, err := AssetName(in[0], in[1])
		if got != want || (err == nil) != (want != "") {
			t.Errorf("AssetName(%s/%s) = %q, %v; want %q", in[0], in[1], got, err, want)
		}
	}
}

func TestVersionOrder(t *testing.T) {
	// Each is lower than the next.
	order := []string{
		"v0.2.0", "v0.10.0", "v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta",
		"v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0-rc.2", "v1.0.0-rc.10", "v1.0.0", "v1.0.1", "v1.1.0", "v2.0.0",
	}
	for i := range order {
		for j := range order {
			a, err := ParseVersion(order[i])
			if err != nil {
				t.Fatal(err)
			}
			b, err := ParseVersion(order[j])
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	a, _ := ParseVersion("v1.2.3+build.5")
	b, _ := ParseVersion("v1.2.3")
	if Compare(a, b) != 0 {
		t.Error("build metadata changes precedence")
	}
	for _, bad := range []string{"dev", "0.2.0", "v0.2", "v0.2.0.1", "v01.2.0", "v1.02.0", "v1.0.0-", "v1.0.0-rc..1", "v1.0.0-rc.01",
		"v1.0.0+", "v1.0.0-r_c", "V1.0.0", "v-1.0.0", "v1.0.0 ", "v99999999999999999999.0.0", ""} {
		if v, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) = %+v", bad, v)
		}
	}
	for _, ok := range []string{"v0.0.0", "v1.0.0-0", "v1.0.0-x-y.0", "v1.0.0-rc.1+20261018.sha-abc"} {
		if _, err := ParseVersion(ok); err != nil {
			t.Errorf("ParseVersion(%q): %v", ok, err)
		}
	}
}
