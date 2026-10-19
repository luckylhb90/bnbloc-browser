package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startTestChrome runs a headless Chrome on a temporary profile, or skips
// when none is installed.
func startTestChrome(t *testing.T) *cdpConn {
	t.Helper()
	if testing.Short() {
		t.Skip("a real browser: not in -short")
	}
	if _, err := findChrome(os.Getenv("BNBLOC_BROWSER_CHROME")); err != nil {
		t.Skip("no Chrome or Chromium on this machine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd, url, err := startChrome(ctx, hostConfig{Profile: t.TempDir(), Headless: true, Chrome: os.Getenv("BNBLOC_BROWSER_CHROME")}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	c, err := dialCDP(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	return c
}

// The executor on a real Chrome against jev-ultrafast's fixture page
// (testdata/fixture.html): the snapshot reads its controls, a typed
// destination and a submitted search change the page, a select sets its
// option, freshness tracks the page, and a stale observation is refused
// before any input.
func TestExecutorOnChrome(t *testing.T) {
	c := startTestChrome(t)
	fixture, err := os.ReadFile("testdata/fixture.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 1120, "height": 780, "deviceScaleFactor": 1, "mobile": false}, nil); err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	pg, err := observe(ctx, tb)
	if err != nil {
		t.Fatal(err)
	}
	find := func(pg *page, kind, label string) action {
		t.Helper()
		for _, a := range pg.Actions {
			if a.Kind == kind && strings.Contains(a.Label, label) {
				return a
			}
		}
		t.Fatalf("no %s %q among %d actions: %+v", kind, label, len(pg.Actions), pg.Actions)
		return action{}
	}
	dest := find(pg, "fill", "Destination")
	if !strings.Contains(pg.Text, "Somewhere you can exhale") || pg.Fingerprint == "" {
		t.Fatalf("page = %q", pg.Text)
	}
	if ok, err := fresh(ctx, tb, pg, nil); err != nil || !ok {
		t.Fatalf("fresh after observing = %v %v", ok, err)
	}

	if err := act(ctx, tb, pg, dest, "Lisbon"); err != nil {
		t.Fatalf("type: %v", err)
	}
	pg2, _ := observe(ctx, tb)
	if pg2.Fingerprint == pg.Fingerprint || find(pg2, "fill", "Destination").Value != "Lisbon" {
		t.Fatalf("the typed value did not land")
	}
	// The old observation is stale now: a full-marker action is refused
	// before input.
	if err := act(ctx, tb, pg, find(pg, "wait", "Wait"), ""); err != errStale {
		t.Fatalf("a stale observation acted: %v", err)
	}

	if err := act(ctx, tb, pg2, find(pg2, "click", "Find stays"), ""); err != nil {
		t.Fatalf("submit: %v", err)
	}
	pg3, _ := observe(ctx, tb)
	if !strings.Contains(pg3.Text, "places in Lisbon") {
		t.Fatalf("the search did not run: %q", pg3.Text)
	}
	if err := act(ctx, tb, pg3, find(pg3, "select", "Nature"), ""); err != nil {
		t.Fatalf("select: %v", err)
	}
	pg4, _ := observe(ctx, tb)
	if !strings.Contains(pg4.Text, "1 places in Lisbon") || !strings.Contains(pg4.Text, "Serra Lodge") {
		t.Fatalf("the select did not apply: %q", pg4.Text)
	}
	shot, err := screenshot(ctx, tb)
	if err != nil || len(shot) < 100 {
		t.Fatalf("screenshot = %d bytes %v", len(shot), err)
	}
	// One image pixel per CSS pixel, whatever the screen's density.
	if raw, _ := base64.StdEncoding.DecodeString(shot); len(raw) > 0 {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
		var w float64
		_ = evaluate(ctx, tb, "innerWidth", &w)
		if err != nil || float64(cfg.Width) != min(w, maxShotWidth) {
			t.Fatalf("screenshot %dx%d for a %v-wide viewport: %v", cfg.Width, cfg.Height, w, err)
		}
	}

	// A password field is observed by label, never by value.
	if err := evaluate(ctx, tb, `(() => { document.body.insertAdjacentHTML('afterbegin', '<label>Password <input type="password" id="pw" value="hunter2"></label>'); return 1; })()`, nil); err != nil {
		t.Fatal(err)
	}
	pg5, _ := observe(ctx, tb)
	pw := find(pg5, "secret", "Password")
	if pw.Value != "" || !pw.Filled {
		t.Fatalf("password = %+v", pw)
	}
	body, _, _, _ := stepRequest("m", pg5, "g", nil)
	if raw := mustJSON(body); strings.Contains(raw, "hunter2") {
		t.Fatal("a password's value reached the decision request")
	}
}

// A page's dialog on a real Chrome (dialog.go): a call stuck behind it is
// released at once (errDialogOpen) instead of waiting for good, the tab
// reports it, and once answered the page runs on.
func TestDialogOnChrome(t *testing.T) {
	c := startTestChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>q</title><script>setTimeout(() => { document.title = "answered:" + confirm("Leave the sign-in?") }, 200)</script>`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	// An evaluation waiting on a promise that resolves after the dialog.
	start := time.Now()
	var v string
	err = evaluate(ctx, tb, `new Promise(r => setTimeout(() => r(document.title), 1500))`, &v)
	if !errors.Is(err, errDialogOpen) || time.Since(start) > 1200*time.Millisecond {
		t.Fatalf("evaluate behind a dialog = %v after %v, want errDialogOpen at once", err, time.Since(start))
	}
	d := tb.openDialog()
	if d == nil || d.Kind != "confirm" || d.Message != "Leave the sign-in?" {
		t.Fatalf("open dialog = %+v", d)
	}
	if _, err := observe(ctx, tb); !errors.Is(err, errDialogOpen) {
		t.Fatalf("observe with a dialog open = %v", err)
	}
	if err := tb.answerDialog(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tb.openDialog() == nil && evaluate(ctx, tb, "document.title", &v) == nil && v == "answered:true" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("after the answer: dialog %+v, title %q", tb.openDialog(), v)
}

// A browser an older version started (its record names no flags, or other
// ones) is closed and started again with this version's flags when it
// shows no web page; while it shows one it is used as it is.
func TestOutdatedBrowserRestarts(t *testing.T) {
	if testing.Short() {
		t.Skip("a real browser: not in -short")
	}
	if _, err := findChrome(os.Getenv("BNBLOC_BROWSER_CHROME")); err != nil {
		t.Skip("no Chrome or Chromium on this machine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Not t.TempDir: a killed browser may still be writing its profile
	// when the test's own cleanup runs.
	dir, err := os.MkdirTemp("", "bnbloc-browser-restart")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { time.Sleep(time.Second); _ = os.RemoveAll(dir) })
	cfg := hostConfig{Profile: dir + "/profile", Headless: true, EndpointFile: dir + "/endpoint.json", Chrome: os.Getenv("BNBLOC_BROWSER_CHROME")}
	start := func() endpointRecord {
		t.Helper()
		cmd, url, err := startChrome(ctx, cfg, true)
		if err != nil {
			t.Fatal(err)
		}
		rec := endpointRecord{URL: url, PID: cmd.Process.Pid} // an older version's record: no flags
		// Reaped here once it exits (as launchd reaps a real one, whose
		// starter is long gone): an unreaped child would look alive.
		go func() { _ = cmd.Wait() }()
		if err := writeEndpoint(cfg.EndpointFile, rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	kill := func(pid int) {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}

	// Showing a web page: used as it is.
	old := start()
	t.Cleanup(func() { kill(old.PID) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<title>signing in</title>")) }))
	t.Cleanup(srv.Close)
	var c0 *cdpConn
	c0, err = dialCDP(ctx, old.URL)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := c0.call(ctx, "", "Target.createTarget", map[string]any{"url": srv.URL}, &created); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	h := newLocalHost(cfg)
	if _, err := h.connect(ctx); err != nil {
		t.Fatal(err)
	}
	if rec, _ := readEndpoint(cfg.EndpointFile); rec.PID != old.PID || !processAlive(old.PID) {
		t.Fatalf("a browser showing a web page was restarted: %+v", rec)
	}

	// No web page left: closed, and started again with this version's flags.
	_ = c0.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": created.TargetID}, nil)
	c0.close()
	time.Sleep(300 * time.Millisecond)
	h = newLocalHost(cfg)
	if _, err := h.connect(ctx); err != nil {
		t.Fatal(err)
	}
	rec, ok := readEndpoint(cfg.EndpointFile)
	if ok {
		t.Cleanup(func() { kill(rec.PID) })
	}
	if !ok || rec.PID == old.PID || rec.Flags != flagsKey(cfg) {
		t.Fatalf("after: %+v (old pid %d)", rec, old.PID)
	}
	if processAlive(old.PID) {
		out, _ := exec.Command("ps", "-o", "pid,stat,command", "-p", strconv.Itoa(old.PID)).CombinedOutput()
		t.Fatalf("the outdated browser is still running:\n%s", out)
	}
}
