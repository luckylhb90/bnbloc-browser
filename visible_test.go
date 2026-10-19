package main

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/component"
)

// The live view on a visible Chrome (BNBLOC_BROWSER_VISIBLE_TEST=1; it opens
// a window): a run's tab opens in its own window behind the member's (not
// minimized, where Chrome stops painting), the capture renders the page
// there, input from the room lands in it, and "Open in the browser window"
// brings it forward.
func TestLiveViewOnABackgroundWindow(t *testing.T) {
	if os.Getenv("BNBLOC_BROWSER_VISIBLE_TEST") != "1" {
		t.Skip("opens a browser window: set BNBLOC_BROWSER_VISIBLE_TEST=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, url, err := startChrome(ctx, hostConfig{Profile: t.TempDir(), Chrome: os.Getenv("BNBLOC_BROWSER_CHROME")}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	c, err := dialCDP(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	tb, err := openTab(ctx, c, true)
	if err != nil {
		t.Fatal(err)
	}
	var win struct {
		WindowID int `json:"windowId"`
		Bounds   struct {
			WindowState string `json:"windowState"`
		} `json:"bounds"`
	}
	if err := c.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": tb.targetID}, &win); err != nil || win.Bounds.WindowState != "normal" {
		t.Fatalf("the run's window = %+v %v, want a normal window", win, err)
	}
	if err := navigate(ctx, tb, `data:text/html,<title>Form</title><input id=f autofocus style="width:300px;height:40px"><p>hello live</p>`); err != nil {
		t.Fatal(err)
	}
	shot, view, err := capture(ctx, tb, liveQuality)
	raw, _ := base64.StdEncoding.DecodeString(shot)
	if err != nil || len(raw) < 1000 || view.Title != "Form" || view.W == 0 {
		t.Fatalf("capture in a background window = %d bytes %+v %v", len(raw), view, err)
	}
	f := &liveFeed{tab: tb, view: view}
	_ = evaluate(ctx, tb, "document.getElementById('f').focus()", nil)
	f.apply(ctx, []component.LiveInput{{Type: component.LiveInputText, Text: "typed from the room"}, {Type: component.LiveInputKey, Action: "down", Key: "!", Code: "Digit1", Text: "!"}, {Type: component.LiveInputKey, Action: "up", Key: "!", Code: "Digit1"}}, nil)
	var value string
	if err := evaluate(ctx, tb, "document.getElementById('f').value", &value); err != nil || value != "typed from the room!" {
		t.Fatalf("input in a background window = %q %v", value, err)
	}
	// A window someone minimized comes back with "Open in the browser
	// window".
	_ = c.call(ctx, "", "Browser.setWindowBounds", map[string]any{"windowId": win.WindowID, "bounds": map[string]any{"windowState": "minimized"}}, nil)
	f.apply(ctx, []component.LiveInput{{Type: component.LiveInputFocus}}, nil)
	_ = c.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": tb.targetID}, &win)
	if win.Bounds.WindowState != "normal" {
		t.Fatalf("the window after bringing it forward = %q", win.Bounds.WindowState)
	}
	if _, _, err := capture(ctx, tb, liveQuality); err != nil {
		t.Fatalf("capture after restoring: %v", err)
	}
}
