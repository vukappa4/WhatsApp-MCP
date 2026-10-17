package pairpage

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func get(t *testing.T, url, host string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPage(t *testing.T) {
	p, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if code, body := get(t, p.URL, ""); code != 200 || !strings.Contains(body, "Link a device") {
		t.Fatalf("page: %d %.80s", code, body)
	}
	base := p.URL[:strings.LastIndex(strings.TrimSuffix(p.URL, "/"), "/")+1]
	for _, u := range []string{base, base + "state", p.URL + "x"} {
		if code, _ := get(t, u, ""); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404 without the random path", u, code)
		}
	}
	if code, _ := get(t, p.URL, "evil.example:80"); code != http.StatusForbidden {
		t.Errorf("foreign Host: %d", code)
	}
	state := func() State {
		_, body := get(t, p.URL+"state", "")
		var s State
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := state(); s.Phase != "connecting" {
		t.Errorf("initial %+v", s)
	}
	p.QR("2@abc,def,ghi")
	if s := state(); s.Phase != "qr" || !strings.HasPrefix(s.QR, "<svg") {
		t.Errorf("qr %+v", s)
	}
	p.Paired("+39 333 123 4567")
	if s := state(); s.Phase != "paired" || s.QR != "" || s.Account != "+39 333 123 4567" {
		t.Errorf("paired %+v", s)
	}
	p.Failed("boom")
	if s := state(); s.Phase != "failed" || s.Detail != "boom" {
		t.Errorf("failed %+v", s)
	}
}

func TestSVG(t *testing.T) {
	s := SVG("2@" + strings.Repeat("x", 200))
	if !strings.HasPrefix(s, "<svg") || !strings.HasSuffix(s, "</svg>") || strings.Count(s, "h1v1h-1z") < 100 {
		t.Errorf("svg %.120s", s)
	}
}
