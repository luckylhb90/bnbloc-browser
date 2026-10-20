package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A device is desktop, phone or tablet ("" keeps the run's); anything else
// is refused.
func TestParseDevice(t *testing.T) {
	for in, want := range map[string]string{"": "", "desktop": "desktop", " Phone ": "phone", "tablet": "tablet"} {
		if got, err := parseDevice(in); err != nil || got != want {
			t.Fatalf("parseDevice(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := parseDevice("watch"); err == nil {
		t.Fatal("a watch was taken for a device")
	}
}

// The device is the run's: every call shows its tab as it (open, view,
// act), a later open without one keeps it, one that names another changes
// it, and an unknown device is refused.
func TestDeviceIsTheRunsOnEveryCall(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new", "device": "phone"})
	run := head["run"].(string)
	if head["device"] != "phone" {
		t.Fatalf("open = %v", head)
	}
	h.call("browser_view", map[string]any{"run": run})
	h.call("browser_act", map[string]any{"run": run, "action": "type", "element": 1, "text": "12"})
	if head, _ = h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"}); head["run"] != run || head["device"] != "phone" {
		t.Fatalf("a later open = %v", head)
	}
	if head, _ = h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new", "device": "desktop"}); head["device"] != nil {
		t.Fatalf("an open as a desktop = %v", head)
	}
	h.call("browser_view", map[string]any{"run": run})
	want := "tab-1 phone|tab-1 phone|tab-1 phone|tab-1 phone|tab-1 desktop|tab-1 desktop"
	if got := strings.Join(h.host.shownAs, "|"); got != want {
		t.Fatalf("the tab was shown as %s; want %s", got, want)
	}
	if r, err := h.tools.runner.store.load(run); err != nil || r.Device != "desktop" {
		t.Fatalf("the run's device = %+v, %v", r, err)
	}
	if msg := h.callErr("browser_open", map[string]any{"url": "https://portal.example.com/", "device": "watch"}); !strings.Contains(msg, "desktop, phone or tablet") {
		t.Fatalf("an unknown device = %s", msg)
	}
}

// phonePage reports what it sees of the device, and counts clicks and
// touches on its button; the server keeps the user agent its requests
// came with.
const phonePage = `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><title>phone</title>
<body style="margin:0"><p>Order status</p><button id="go" style="margin:200px 40px;width:120px;height:48px">Track order</button>
<script>window.hits=0;window.touches=0;
document.getElementById('go').addEventListener('click',()=>window.hits++);
document.getElementById('go').addEventListener('touchstart',()=>window.touches++);</script>`

// On a real Chrome, a phone run's tab is a phone to the page: its width
// and density, a mobile user agent (to the server too), touch; a click
// still reaches the page's button, a tap does too on a page that is shown;
// its picture is phone-shaped; and the page is never sized to its window,
// though shrunkViewport would call it shrunk.
func TestPhoneOnChrome(t *testing.T) {
	c := startTestChrome(t)
	var mu sync.Mutex
	var agents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		agents = append(agents, r.UserAgent())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(phonePage))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	var seen struct {
		W, H, DPR     float64
		UA            string
		Mobile        bool
		Touch, Coarse bool
		Points        int
	}
	if err := evaluate(ctx, tb, `({W: innerWidth, H: innerHeight, DPR: devicePixelRatio, UA: navigator.userAgent, Mobile: navigator.userAgentData?.mobile ?? false,
	  Touch: 'ontouchstart' in window, Coarse: matchMedia('(pointer: coarse)').matches, Points: navigator.maxTouchPoints})`, &seen); err != nil {
		t.Fatal(err)
	}
	phone := devices["phone"]
	if seen.W != float64(phone.Width) || seen.H != float64(phone.Height) || seen.DPR != phone.DPR || !seen.Touch || !seen.Coarse || seen.Points == 0 ||
		!strings.Contains(seen.UA, "Android") || !strings.Contains(seen.UA, "Mobile") || strings.Contains(seen.UA, "Headless") || !seen.Mobile {
		t.Fatalf("the page sees %+v", seen)
	}
	mu.Lock()
	if len(agents) == 0 || !strings.Contains(agents[len(agents)-1], "Mobile") {
		t.Fatalf("the server saw %q", agents)
	}
	mu.Unlock()

	// A click (the mouse, as the executor clicks) reaches the button.
	pg, err := observe(ctx, tb)
	if err != nil {
		t.Fatal(err)
	}
	var button action
	for _, a := range pg.Actions {
		if a.Kind == "click" && strings.Contains(a.Label, "Track order") {
			button = a
		}
	}
	if button.Node == nil {
		t.Fatalf("no button among %+v", pg.Actions)
	}
	if err := act(ctx, tb, pg, button, ""); err != nil {
		t.Fatal(err)
	}
	var count [2]int
	if err := evaluate(ctx, tb, "[window.hits, window.touches]", &count); err != nil || count != [2]int{1, 0} {
		t.Fatalf("after a click: hits, touches = %v, %v", count, err)
	}
	// A tap reaches it as a touch and a click, once the page is shown (on
	// one that is not, a background tab, Chrome never answers a tap: the
	// executor keeps to the mouse).
	_ = tb.call(ctx, "Page.bringToFront", nil, nil)
	var at [2]float64
	_ = evaluate(ctx, tb, "(r => [r.x+r.width/2, r.y+r.height/2])(document.getElementById('go').getBoundingClientRect())", &at)
	tctx, tcancel := context.WithTimeout(ctx, 5*time.Second)
	for _, ev := range []map[string]any{
		{"type": "touchStart", "touchPoints": []any{map[string]any{"x": at[0], "y": at[1]}}},
		{"type": "touchEnd", "touchPoints": []any{}},
	} {
		if err := tb.call(tctx, "Input.dispatchTouchEvent", ev, nil); err != nil {
			t.Fatalf("tap: %v", err)
		}
	}
	tcancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && count != [2]int{2, 1} {
		time.Sleep(50 * time.Millisecond)
		_ = evaluate(ctx, tb, "[window.hits, window.touches]", &count)
	}
	if count != [2]int{2, 1} {
		t.Fatalf("after a tap: hits, touches = %v", count)
	}

	// The picture is the phone's screen, one image pixel per CSS pixel.
	shot, _, err := capture(ctx, tb, 50)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(shot)
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != phone.Width || b.Dy() != phone.Height {
		t.Fatalf("the picture is %v", b)
	}

	// A phone's page is far smaller than its window, on purpose: a second
	// session showing it as the phone leaves it so.
	var win struct {
		Bounds struct{ Width, Height float64 } `json:"bounds"`
	}
	if err := c.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": tb.targetID}, &win); err != nil {
		t.Fatal(err)
	}
	if !shrunkViewport(seen.W, seen.H, win.Bounds.Width, win.Bounds.Height) {
		t.Fatalf("a %vx%v page in a %vx%v window is not shrunk to shrunkViewport", seen.W, seen.H, win.Bounds.Width, win.Bounds.Height)
	}
	tb.detach(ctx)
	again, err := attachTab(ctx, c, tb.targetID, "phone")
	if err != nil {
		t.Fatal(err)
	}
	var after [3]float64
	_ = evaluate(ctx, again, "[innerWidth, innerHeight, devicePixelRatio]", &after)
	if after != [3]float64{seen.W, seen.H, seen.DPR} {
		t.Fatalf("attached again, the page is %v", after)
	}
}

// On a real Chrome, a tablet's user agent says Android without Mobile, so
// sites serve their tablet layout.
func TestTabletOnChrome(t *testing.T) {
	c := startTestChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "tablet")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, `data:text/html,<meta name="viewport" content="width=device-width"><title>t</title>tablet`); err != nil {
		t.Fatal(err)
	}
	var seen struct {
		W  float64
		UA string
	}
	_ = evaluate(ctx, tb, "({W: innerWidth, UA: navigator.userAgent})", &seen)
	if seen.W != float64(devices["tablet"].Width) || !strings.Contains(seen.UA, "Android") || strings.Contains(seen.UA, "Mobile") {
		t.Fatalf("the page sees %+v", seen)
	}
}
