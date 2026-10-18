package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/luckylhb90/bnbloc/internal/wire"
)

// The run's live view (§2.8, wire/job_live.go): while a call works, the
// tab's picture goes to the room through the relay (POST /v1/live), and the
// reply brings back whether anyone watches, who holds control and their
// input, which runs here as CDP input on the tab.
//
// Frames are screenshots, not Chrome's screencast: the window lives behind
// the member's other windows, where the screencast stops and a capture
// still renders. A frame is posted only when the picture
// changed; the pace follows the viewers: liveWatchedEvery while someone
// watches the large view or holds control, liveIdleEvery otherwise.

const (
	liveWatchedEvery = 250 * time.Millisecond
	liveIdleEvery    = 2 * time.Second
	liveQuality      = 55
	liveCaptureFor   = 2 * time.Second
)

// liveFeed is one call's live view.
type liveFeed struct {
	gw  *relay
	tab tabHandle
	run string

	mu         sync.Mutex
	controller string
	watched    bool
	view       viewport
	seq        int64
	last       [32]byte // the last posted picture's hash (the loop's)

	stop chan struct{}
	done chan struct{}
}

// startLive begins the call's live view on tb; stop ends it. A relay that
// refuses live frames (an older gateway, no running turn) ends it quietly:
// the run works the same without one.
func startLive(gw *relay, tb tabHandle, run string) *liveFeed {
	f := &liveFeed{gw: gw, tab: tb, run: run, stop: make(chan struct{}), done: make(chan struct{})}
	// The first frame goes before the run's first step: the room sees the
	// page at once, and a person already in control holds the run.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	every, ok := f.tick(ctx, &[32]byte{})
	cancel()
	if !ok {
		close(f.done)
		return f
	}
	go f.loop(every)
	return f
}

// end stops the feed and waits for it.
func (f *liveFeed) end() {
	if f == nil {
		return
	}
	close(f.stop)
	<-f.done
}

// holder is who holds control of the run's browser now ("" nobody): while
// someone does, the run takes no step.
func (f *liveFeed) holder() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.controller
}

func (f *liveFeed) loop(every time.Duration) {
	defer close(f.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.stop:
		case <-f.done:
		}
		cancel()
	}()
	last := f.last
	for {
		select {
		case <-f.stop:
			return
		case <-time.After(every):
		}
		next, ok := f.tick(ctx, &last)
		if !ok {
			return
		}
		every = next
	}
}

// tick captures the page, posts it when it changed (or, while someone
// controls, posts anyway: the reply is how their input arrives), and runs
// the input that comes back. It answers the pace for the next tick, and
// false once live views are refused (off here, or the turn ended).
func (f *liveFeed) tick(ctx context.Context, last *[32]byte) (time.Duration, bool) {
	// A window the member minimized never paints, and its capture never
	// answers: bounded, the feed goes on (the last picture stays, input
	// still arrives) until the window shows again.
	cctx, cancel := context.WithTimeout(ctx, liveCaptureFor)
	shot, view, err := capture(cctx, f.tab, liveQuality)
	cancel()
	post := liveFramePost{Run: f.run}
	if err == nil && shot != "" {
		if sum := sha256.Sum256([]byte(shot)); sum != *last {
			*last = sum
			f.mu.Lock()
			f.seq++
			f.view = view
			post.Seq = f.seq
			f.mu.Unlock()
			post.MimeType, post.Data, post.Title, post.URL = "image/jpeg", shot, view.Title, view.URL
			post.Width, post.Height = shotSize(view)
		}
	}
	f.last = *last
	if post.Data == "" && f.holder() == "" {
		return f.pace(), true
	}
	reply, err := f.gw.liveFrame(ctx, post)
	var re *relayError
	switch {
	case errors.As(err, &re) && (re.Status == http.StatusNotFound || re.Status == http.StatusConflict || re.Status == http.StatusBadRequest):
		return 0, false
	case err != nil:
		return f.pace(), true
	}
	f.mu.Lock()
	f.controller, f.watched = reply.Controller, reply.Watching
	f.mu.Unlock()
	f.apply(ctx, reply.Input)
	return f.pace(), true
}

// pace is how often the page is looked at: often while someone watches
// the large view or controls it.
func (f *liveFeed) pace() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watched || f.controller != "" {
		return liveWatchedEvery
	}
	return liveIdleEvery
}

// shotSize is a capture's image size (capture's scale).
func shotSize(v viewport) (int, int) {
	if v.W <= 0 || v.H <= 0 {
		return 0, 0
	}
	w, h := v.W, v.H
	if w > maxShotWidth {
		h = h * maxShotWidth / w
		w = maxShotWidth
	}
	return int(w), int(h)
}

// liveFramePost is wire.LiveFramePost with the image as the capture's
// base64 (what []byte marshals to), so it is never decoded here.
type liveFramePost struct {
	Run      string `json:"run,omitempty"`
	Seq      int64  `json:"seq,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
}

// liveFrame posts one frame (or an empty post) through the relay.
func (g *relay) liveFrame(ctx context.Context, post liveFramePost) (wire.LiveFrameReply, error) {
	var reply wire.LiveFrameReply
	raw, err := json.Marshal(post)
	if err != nil {
		return reply, err
	}
	out, err := g.do(ctx, http.MethodPost, "/v1/live", raw)
	if err != nil {
		return reply, err
	}
	err = json.Unmarshal(out, &reply)
	return reply, err
}

// apply runs a controller's input on the tab. Coordinates are fractions of
// the frame, mapped to the page's CSS pixels.
func (f *liveFeed) apply(ctx context.Context, in []wire.LiveInput) {
	if len(in) == 0 {
		return
	}
	f.mu.Lock()
	view := f.view
	f.mu.Unlock()
	for _, e := range in {
		x, y := e.X*view.W, e.Y*view.H
		switch e.Type {
		case wire.LiveInputMouse:
			kind := map[string]string{"down": "mousePressed", "up": "mouseReleased"}[e.Action]
			if kind == "" {
				kind = "mouseMoved"
			}
			button := e.Button
			if button == "" {
				button = "none"
				if kind != "mouseMoved" {
					button = "left"
				}
			}
			clicks := e.Clicks
			if clicks == 0 && kind != "mouseMoved" {
				clicks = 1
			}
			_ = f.tab.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": kind, "x": x, "y": y, "button": button, "clickCount": clicks, "modifiers": e.Modifiers}, nil)
		case wire.LiveInputWheel:
			_ = f.tab.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": x, "y": y, "deltaX": e.DeltaX, "deltaY": e.DeltaY, "modifiers": e.Modifiers}, nil)
		case wire.LiveInputKey:
			_ = f.tab.call(ctx, "Input.dispatchKeyEvent", keyEvent(e), nil)
		case wire.LiveInputText:
			if e.Text != "" {
				_ = f.tab.call(ctx, "Input.insertText", map[string]any{"text": e.Text}, nil)
			}
		case wire.LiveInputFocus:
			f.bringForward(ctx)
		}
	}
}

// keyEvent is a key's CDP event. A printable key sends its text on press
// (that types it); the named keys carry the virtual key code Chrome acts on.
func keyEvent(e wire.LiveInput) map[string]any {
	ev := map[string]any{"key": e.Key, "code": e.Code, "modifiers": e.Modifiers}
	if vk, ok := virtualKeys[e.Key]; ok {
		ev["windowsVirtualKeyCode"], ev["nativeVirtualKeyCode"] = vk, vk
	}
	switch {
	case e.Action == "up":
		ev["type"] = "keyUp"
	case e.Text != "" && e.Modifiers&(2|4) == 0:
		ev["type"], ev["text"] = "keyDown", e.Text
	default:
		ev["type"] = "rawKeyDown"
	}
	return ev
}

var virtualKeys = map[string]int{
	"Backspace": 8, "Tab": 9, "Enter": 13, "Escape": 27, " ": 32,
	"PageUp": 33, "PageDown": 34, "End": 35, "Home": 36,
	"ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40, "Delete": 46,
}

// bringForward shows the run's window in front of the member's others
// (restored if minimized), with the run's tab selected.
func (f *liveFeed) bringForward(ctx context.Context) {
	h, ok := f.tab.(interface{ bringForward(context.Context) error })
	if ok {
		_ = h.bringForward(ctx)
	}
}

// bringForward restores the tab's window and selects the tab.
func (t *tab) bringForward(ctx context.Context) error {
	var w struct {
		WindowID int `json:"windowId"`
	}
	if err := t.conn.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": t.targetID}, &w); err == nil {
		_ = t.conn.call(ctx, "", "Browser.setWindowBounds", map[string]any{"windowId": w.WindowID, "bounds": map[string]any{"windowState": "normal"}}, nil)
	}
	return t.conn.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": t.targetID}, nil)
}
