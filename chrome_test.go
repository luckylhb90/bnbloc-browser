package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
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
