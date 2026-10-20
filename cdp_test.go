package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeBrowser is a DevTools endpoint: /json/version names its socket, and
// the socket answers the target calls a run makes, sessions flattened. It
// refuses what a browser refuses: a session call on a session it never
// attached, an attach to a target that does not exist.
type fakeBrowser struct {
	mu       sync.Mutex
	targets  map[string]bool
	sessions map[string]string // session → target
	calls    []string
}

func (b *fakeBrowser) start(t *testing.T) string {
	t.Helper()
	b.targets, b.sessions = map[string]bool{}, map[string]string{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": "ws" + strings.TrimPrefix(srv.URL, "http") + "/devtools/browser/x"})
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(-1)
		ctx := r.Context()
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m struct {
				ID        int64          `json:"id"`
				Method    string         `json:"method"`
				SessionID string         `json:"sessionId"`
				Params    map[string]any `json:"params"`
			}
			_ = json.Unmarshal(raw, &m)
			res, cerr := b.answer(m.Method, m.SessionID, m.Params)
			out := map[string]any{"id": m.ID}
			if cerr != nil {
				out["error"] = cerr
			} else {
				out["result"] = res
			}
			msg, _ := json.Marshal(out)
			_ = c.Write(ctx, websocket.MessageText, msg)
			if m.Method == "Page.navigate" {
				ev, _ := json.Marshal(map[string]any{"method": "Page.frameNavigated", "sessionId": m.SessionID, "params": map[string]any{"url": m.Params["url"]}})
				_ = c.Write(ctx, websocket.MessageText, ev)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (b *fakeBrowser) answer(method, session string, p map[string]any) (any, *cdpError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, method)
	if session != "" {
		if _, ok := b.sessions[session]; !ok {
			return nil, &cdpError{Code: -32001, Message: "Session with given id not found."}
		}
	}
	switch method {
	case "Target.createTarget":
		id := "T" + string(rune('0'+len(b.targets)))
		b.targets[id] = true
		return map[string]any{"targetId": id}, nil
	case "Target.getTargets":
		var infos []any
		for id := range b.targets {
			infos = append(infos, map[string]any{"targetId": id, "type": "page"})
		}
		return map[string]any{"targetInfos": infos}, nil
	case "Target.attachToTarget":
		id, _ := p["targetId"].(string)
		if !b.targets[id] {
			return nil, &cdpError{Code: -32602, Message: "No target with given id found"}
		}
		s := "S-" + id
		b.sessions[s] = id
		return map[string]any{"sessionId": s}, nil
	case "Target.detachFromTarget":
		delete(b.sessions, p["sessionId"].(string))
		return map[string]any{}, nil
	case "Runtime.evaluate":
		if p["expression"] == "document.readyState" {
			return map[string]any{"result": map[string]any{"value": "complete"}}, nil
		}
		return map[string]any{"result": map[string]any{"value": nil}, "exceptionDetails": map[string]any{"text": "Uncaught"}}, nil
	}
	return map[string]any{}, nil
}

// The transport: the socket from /json/version, a new tab attached with
// its session flattened onto every call, an exception as errStale, a
// protocol error as an error, events queued per session, a closed tab
// errTabGone.
func TestCDPTransport(t *testing.T) {
	fb := &fakeBrowser{}
	base := fb.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialCDP(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if tb.target() != "T0" || tb.session != "S-T0" {
		t.Fatalf("tab = %+v", tb)
	}
	if err := navigate(ctx, tb, "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if evs := c.drain(tb.session, "Page.frameNavigated"); len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	if err := evaluate(ctx, tb, "boom()", nil); !errors.Is(err, errStale) {
		t.Fatalf("an exception = %v; want errStale", err)
	}
	var ce *cdpError
	if err := c.call(ctx, "S-nope", "Runtime.evaluate", nil, nil); !errors.As(err, &ce) || ce.Code != -32001 {
		t.Fatalf("a bad session = %v", err)
	}
	if _, err := attachTab(ctx, c, "T9", ""); !errors.Is(err, errTabGone) {
		t.Fatalf("a closed tab = %v", err)
	}
	tb.detach(ctx)
	if err := tb.call(ctx, "Runtime.evaluate", map[string]any{"expression": "1"}, nil); err == nil {
		t.Fatal("a detached session still answered")
	}
}

// The DevTools port file: port, then the browser path.
func TestDevToolsURL(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/DevToolsActivePort"
	if _, ok := devToolsURL(p); ok {
		t.Fatal("a missing file named an endpoint")
	}
	writeFile(t, p, "53211\n/devtools/browser/abc-123\n")
	if u, ok := devToolsURL(p); !ok || u != "ws://127.0.0.1:53211/devtools/browser/abc-123" {
		t.Fatalf("url = %q %v", u, ok)
	}
	writeFile(t, p, "53211\n/not/devtools\n")
	if _, ok := devToolsURL(p); ok {
		t.Fatal("a foreign path accepted")
	}
	ep := dir + "/ep/endpoint.json"
	if err := writeEndpoint(ep, endpointRecord{URL: "ws://127.0.0.1:1/devtools/browser/x", PID: 7}); err != nil {
		t.Fatal(err)
	}
	if rec, ok := readEndpoint(ep); !ok || rec.PID != 7 {
		t.Fatalf("endpoint = %+v %v", rec, ok)
	}
	args := strings.Join(chromeArgs(hostConfig{Profile: "/p"}), " ")
	if !strings.Contains(args, "--user-data-dir=/p") || !strings.Contains(args, "--remote-debugging-port=0") || strings.Contains(args, "headless") {
		t.Fatalf("args = %s", args)
	}
	// A visible browser starts without a window (a run's tab opens its own,
	// minimized) and keeps rendering hidden pages for the live view.
	if !strings.Contains(args, "--no-startup-window") || !strings.Contains(args, "--disable-backgrounding-occluded-windows") || strings.Contains(args, "about:blank") {
		t.Fatalf("args = %s", args)
	}
}

// Downloads land in the workspace under a safe, unused name.
func TestMoveDownload(t *testing.T) {
	dir := t.TempDir()
	src := dir + "/guid-1"
	writeFile(t, src, "a,b\n")
	name, err := moveDownload(src, dir+"/downloads", "../../etc/report.csv")
	if err != nil || name != "downloads/report.csv" {
		t.Fatalf("name = %q %v", name, err)
	}
	writeFile(t, src, "c,d\n")
	if name, _ = moveDownload(src, dir+"/downloads", "report.csv"); name != "downloads/report-2.csv" {
		t.Fatalf("second = %q", name)
	}
	if got := safeFileName(".."); got != "download" {
		t.Fatalf("safe(..) = %q", got)
	}
}

// An upload names a file inside the workspace only: not outside it, not
// through a link out of it, not a folder.
func TestWorkspaceFile(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	t.Chdir(root)
	writeFile(t, root+"/a.pdf", "%PDF")
	writeFile(t, outside+"/secret.txt", "x")
	if err := os.Symlink(outside+"/secret.txt", root+"/link.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root+"/dir", 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := workspaceFile("a.pdf"); err != nil || !strings.HasSuffix(p, "a.pdf") {
		t.Fatalf("a.pdf = %q %v", p, err)
	}
	for _, bad := range []string{"", "../" + filepath.Base(outside) + "/secret.txt", outside + "/secret.txt", "link.txt", "dir", "missing.pdf"} {
		if p, err := workspaceFile(bad); err == nil {
			t.Errorf("%q resolved to %s", bad, p)
		}
	}
}
