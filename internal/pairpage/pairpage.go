// Package pairpage serves the pairing QR code as a local web page, the way
// WhatsApp Web shows it: an installer agent runs `pair` in the background,
// and the user scans the code from the browser window it opens.
package pairpage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"
)

// State is what the page shows.
type State struct {
	Phase   string `json:"phase"` // connecting, qr, paired, done, failed
	QR      string `json:"qr,omitempty"`
	Account string `json:"account,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Page is the running page server. Its URL holds a random path, and it
// answers only on 127.0.0.1 with its own Host header.
type Page struct {
	URL string

	mu    sync.Mutex
	state State
	srv   *http.Server
	host  string
	done  chan struct{}
}

// Start serves the page on a free port of 127.0.0.1.
func Start() (*Page, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		ln.Close()
		return nil, err
	}
	p := &Page{host: ln.Addr().String(), state: State{Phase: "connecting"}, done: make(chan struct{})}
	base := "/" + hex.EncodeToString(buf) + "/"
	p.URL = "http://" + p.host + base
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"{$}", p.serveHTML)
	mux.HandleFunc("GET "+base+"state", p.serveState)
	p.srv = &http.Server{Handler: p.guard(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = p.srv.Serve(ln); close(p.done) }()
	return p, nil
}

// guard refuses requests addressed to another host name (DNS rebinding).
func (p *Page) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != p.host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

func (p *Page) set(f func(*State)) {
	p.mu.Lock()
	f(&p.state)
	p.mu.Unlock()
}

// QR shows a new QR code.
func (p *Page) QR(code string) {
	svg := SVG(code)
	p.set(func(s *State) { s.Phase, s.QR = "qr", svg })
}

// Paired shows the linked account while the history arrives.
func (p *Page) Paired(account string) {
	p.set(func(s *State) { s.Phase, s.QR, s.Account = "paired", "", account })
}

// Done shows that pairing finished.
func (p *Page) Done() { p.set(func(s *State) { s.Phase, s.QR = "done", "" }) }

// Failed shows why pairing stopped.
func (p *Page) Failed(detail string) {
	p.set(func(s *State) { s.Phase, s.QR, s.Detail = "failed", "", detail })
}

// Close keeps serving long enough for the page to poll the final state once
// more, then stops.
func (p *Page) Close() {
	time.Sleep(3 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = p.srv.Shutdown(ctx)
	<-p.done
}

func (p *Page) serveState(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	s := p.state
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}

func (p *Page) serveHTML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:")
	_, _ = w.Write([]byte(pageHTML))
}

// SVG renders code as a QR code (error correction L, as WhatsApp Web) with
// a quiet zone of 4 modules.
func SVG(code string) string {
	c, err := qr.Encode(code, qr.L)
	if err != nil {
		return ""
	}
	const quiet = 4
	n := c.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

// Open opens url in the default browser.
func Open(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Link WhatsApp MCP</title>
<style>
  :root { --bg: #f4f1ec; --card: #fff; --ink: #1c1e21; --muted: #5f6368; --accent: #128c7e; --bad: #b3261e; }
  @media (prefers-color-scheme: dark) {
    :root { --bg: #111b21; --card: #1f2c33; --ink: #e9edef; --muted: #aebac1; --accent: #25d366; --bad: #f2b8b5; }
  }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--ink);
         font: 16px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; padding: 16px; }
  main { background: var(--card); border-radius: 16px; padding: 40px; max-width: 820px; width: 100%;
         box-shadow: 0 2px 24px rgba(0,0,0,.08); display: flex; gap: 40px; align-items: center; flex-wrap: wrap; }
  .text { flex: 1 1 320px; }
  h1 { font-size: 26px; font-weight: 500; margin: 0 0 24px; }
  ol { margin: 0; padding-left: 22px; }
  li { margin: 0 0 14px; }
  b { font-weight: 600; }
  .note { color: var(--muted); font-size: 14px; margin-top: 20px; }
  .qr { flex: 0 0 280px; width: 280px; height: 280px; display: grid; place-items: center; }
  .qr svg { width: 280px; height: 280px; border-radius: 8px; }
  .spin { width: 40px; height: 40px; border: 4px solid var(--muted); border-top-color: var(--accent);
          border-radius: 50%; animation: s 1s linear infinite; opacity: .6; }
  @keyframes s { to { transform: rotate(360deg); } }
  .ok { color: var(--accent); font-size: 64px; line-height: 1; }
  .bad { color: var(--bad); }
  [hidden] { display: none !important; }
</style>
</head>
<body>
<main>
  <div class="text">
    <h1 id="title">Link WhatsApp MCP to your phone</h1>
    <div id="steps">
      <ol>
        <li>Open <b>WhatsApp</b> on your phone.</li>
        <li>Tap <b>Settings</b> (iPhone) or the <b>&#8942;</b> menu (Android), then <b>Linked devices</b>.</li>
        <li>Tap <b>Link a device</b> and point the phone at this code.</li>
      </ol>
      <p class="note">The code refreshes by itself. Your messages stay on this computer.</p>
    </div>
    <p id="msg" hidden></p>
  </div>
  <div class="qr" id="qr"><div class="spin"></div></div>
</main>
<script>
  const $ = id => document.getElementById(id);
  let last = "connecting", stopped = false;
  function show(s) {
    last = s.phase;
    const steps = $("steps"), msg = $("msg"), qr = $("qr"), title = $("title");
    if (s.phase === "connecting" || s.phase === "qr") {
      steps.hidden = false; msg.hidden = true;
      qr.innerHTML = s.qr || '<div class="spin"></div>';
      return;
    }
    steps.hidden = true; msg.hidden = false; msg.className = "";
    if (s.phase === "paired") {
      title.textContent = "Linked" + (s.account ? " as " + s.account : "");
      msg.textContent = "Your phone is now sending your chat history. Keep WhatsApp open on the phone: this takes a few minutes. You can close this page.";
      qr.innerHTML = '<div class="ok">&#10003;</div>';
    } else if (s.phase === "done") {
      title.textContent = "All set";
      msg.textContent = "Your chats are on this computer. You can close this page.";
      qr.innerHTML = '<div class="ok">&#10003;</div>';
    } else {
      title.textContent = "Linking stopped";
      msg.textContent = (s.detail || "Something went wrong.") + " Ask Claude to run the pairing again.";
      msg.className = "bad";
      qr.innerHTML = "";
    }
  }
  async function poll() {
    if (stopped) return;
    try {
      const r = await fetch("state", { cache: "no-store" });
      show(await r.json());
    } catch (e) {
      stopped = true;
      if (last === "paired" || last === "done") show({ phase: "done" });
      else show({ phase: "failed", detail: "The pairing program has stopped." });
      return;
    }
    if (last === "done" || last === "failed") { stopped = true; return; }
    setTimeout(poll, 1000);
  }
  poll();
</script>
</body>
</html>
`
