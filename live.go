package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/luckylhb90/bnbloc/component"
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
	// liveAfterInput is how soon the page is looked at again after the
	// controller's input ran: what they did shows at once.
	liveAfterInput = 60 * time.Millisecond
	liveQuality    = 50
	liveCaptureFor = 2 * time.Second
	// liveInputPostFor bounds one post of the input loop (realtime holds
	// it at most half a second).
	liveInputPostFor = 10 * time.Second
	// liveAskEvery is how often the input loop asks, nobody in control.
	liveAskEvery = time.Second
)

// liveFeed is one call's live view, or a paused run's that a keeper holds
// after the call (keep.go: job, waiting and ready set).
type liveFeed struct {
	gw  *relay
	tab tabHandle
	run string
	// job names the job the view is posted under ("" the bound turn's).
	job string
	// waiting is what a person does in the view (keep.go); ready reports
	// whether it looks done, asked when the picture changes.
	waiting string
	ready   func(ctx context.Context) bool

	mu         sync.Mutex
	controller string
	watched    bool
	view       viewport
	seq        int64
	last       [32]byte // the last posted picture's hash (the loop's)
	posted     time.Time
	isReady    bool
	// closed: the owner closed the view from the room (LiveInputClose).
	closed bool
	// crashed: the page crashed, as the last post said.
	crashed bool
	// dialog is the page's open dialog the last post carried.
	dialog *component.LiveDialog
	// handsOff marks a feed let go of its page while the person works in
	// the browser window itself (keep.go); onFocus, when set, is what the
	// owner's "open on this computer" does; reattach asks for the page
	// again (input from the room while hands off).
	handsOff     bool
	onFocus      func(ctx context.Context)
	reattach     bool
	wantHandsOff bool
	// refused is the last refusal of a post (the keeper logs it).
	refusedBy string

	// poke asks the frame loop to look at the page now (input just ran).
	poke     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// startLive begins the call's live view on tb; stop ends it. A relay that
// refuses live frames (an older gateway, no running turn) ends it quietly:
// the run works the same without one.
func startLive(gw *relay, tb tabHandle, run string) *liveFeed {
	f := newFeed(gw, tb, run)
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
	go f.inputs()
	return f
}

// newFeed is a feed not yet started. Its frames count on from the clock,
// so a later feed of the run (a keeper, the next call) never posts a lower
// sequence than an earlier one.
func newFeed(gw *relay, tb tabHandle, run string) *liveFeed {
	return &liveFeed{gw: gw, tab: tb, run: run, seq: time.Now().UnixMilli(), poke: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

// page is the feed's tab (nil while hands off).
func (f *liveFeed) page() tabHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tab
}

// setPage sets the feed's tab (nil: hands off).
func (f *liveFeed) setPage(t tabHandle) {
	f.mu.Lock()
	f.tab, f.handsOff = t, t == nil
	f.mu.Unlock()
}

// requests answers, and clears, what the owner asked of a keeper's feed:
// to let go of the page (they work in the browser window) or to take it
// back (they act in the room's view).
func (f *liveFeed) requests() (handsOff, reattach bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	handsOff, reattach = f.wantHandsOff, f.reattach
	f.wantHandsOff, f.reattach = false, false
	return handsOff, reattach
}

// end stops the feed and waits for it.
func (f *liveFeed) end() {
	if f == nil {
		return
	}
	f.stopOnce.Do(func() { close(f.stop) })
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
		case <-f.poke:
			if sleepCtx(ctx, liveAfterInput) != nil {
				return
			}
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
	// A page that waits on its dialog draws nothing new: the view keeps
	// its last picture and carries the dialog, which the controller
	// answers (dialog.go).
	// A crashed page, or one let go of (hands off), draws nothing new
	// either: the last picture stays, and the post says why.
	t := f.page()
	var dialog *component.LiveDialog
	crashed := false
	if t != nil {
		dialog, crashed = dialogOf(t), crashedOf(t)
	}
	var shot string
	var view viewport
	err := errDialogOpen
	if t != nil && dialog == nil && !crashed {
		cctx, cancel := context.WithTimeout(ctx, liveCaptureFor)
		shot, view, err = capture(cctx, t, liveQuality)
		cancel()
	}
	f.mu.Lock()
	post := liveFramePost{Run: f.run, Waiting: f.waiting, Dialog: dialog, Crashed: crashed}
	crashChanged := crashed != f.crashed
	f.crashed = crashed
	dialogChanged := !sameDialog(dialog, f.dialog)
	f.dialog = dialog
	f.mu.Unlock()
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
	// A waiting view says whether what it waits for looks done, asked
	// when the picture changes (the page moved on).
	readyChanged := false
	if f.ready != nil && post.Data != "" {
		now := f.ready(ctx)
		f.mu.Lock()
		readyChanged, f.isReady = now != f.isReady, now
		f.mu.Unlock()
	}
	f.mu.Lock()
	post.Ready = f.isReady
	// A waiting view posts at least every LiveKeepEverySec: the room keeps
	// it only while posts go on.
	keep := f.waiting != "" && time.Since(f.posted) >= component.LiveKeepEverySec*time.Second/2
	f.mu.Unlock()
	if post.Data == "" && !keep && !readyChanged && !dialogChanged && !crashChanged {
		return f.pace(), true
	}
	// While someone drives, their input is the input loop's (inputs): a
	// frame leaves it queued, so a keystroke never waits behind a capture
	// and its upload.
	post.LeaveInput = f.holder() != ""
	reply, err := f.gw.liveFrame(ctx, f.job, post)
	var re *relayError
	switch {
	case errors.As(err, &re) && refused(re.Status):
		f.mu.Lock()
		f.refusedBy = re.Error()
		f.mu.Unlock()
		return 0, false
	case err != nil:
		return f.pace(), true
	}
	f.mu.Lock()
	f.controller, f.watched, f.posted = reply.Controller, reply.Watching, time.Now()
	f.mu.Unlock()
	// Input in a frame's reply: the owner's focus or close (no control
	// needed), or anyone's from a realtime that does not leave it.
	f.apply(ctx, reply.Input, post.Dialog)
	if len(reply.Input) > 0 {
		return liveAfterInput, true
	}
	return f.pace(), true
}

// inputs runs the controller's input as it arrives: while someone holds
// control, an empty post that waits for it (WaitInput; realtime answers as
// it is queued), its input run on the tab, the frame loop poked to show
// the result, and again. Nobody in control, an empty post every
// liveAskEvery learns when someone takes it (and the owner's focus or
// close), however still the page stands.
func (f *liveFeed) inputs() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.stop:
		case <-f.done:
		}
		cancel()
	}()
	for ctx.Err() == nil {
		controlled := f.holder() != ""
		f.mu.Lock()
		post := liveFramePost{Run: f.run, Waiting: f.waiting, Ready: f.isReady, WaitInput: controlled, Dialog: f.dialog, Crashed: f.crashed}
		f.mu.Unlock()
		pctx, pcancel := context.WithTimeout(ctx, liveInputPostFor)
		reply, err := f.gw.liveFrame(pctx, f.job, post)
		pcancel()
		if err != nil {
			// Refused or unreachable: the frame loop decides whether the
			// view goes on; this one tries again shortly.
			if sleepCtx(ctx, liveWatchedEvery) != nil {
				return
			}
			continue
		}
		f.mu.Lock()
		f.controller, f.watched, f.posted = reply.Controller, reply.Watching, time.Now()
		f.mu.Unlock()
		if len(reply.Input) > 0 {
			f.apply(ctx, reply.Input, post.Dialog)
			select {
			case f.poke <- struct{}{}:
			default:
			}
			continue
		}
		if !controlled && sleepCtx(ctx, liveAskEvery) != nil {
			return
		}
	}
}

// announce posts the view's picture now, saying what it waits for (the
// call's feed, ended, at a needs_human pause): the room keeps the view
// before the turn ends, with no gap until the keeper's first post.
func (f *liveFeed) announce(ctx context.Context, waiting string) {
	f.mu.Lock()
	f.waiting, f.posted, f.last = waiting, time.Time{}, [32]byte{}
	f.mu.Unlock()
	last := [32]byte{}
	_, _ = f.tick(ctx, &last)
}

// refusal is why the last post was refused ("" none).
func (f *liveFeed) refusal() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refusedBy
}

// refused: the relay or the gateway will not take this view's posts (live
// views off, the turn ended and the view does not wait, the session gone).
func refused(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return true
	}
	return false
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

// liveFramePost is component.LiveFramePost with the image as the capture's
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
	Waiting  string `json:"waiting,omitempty"`
	Ready    bool   `json:"ready,omitempty"`
	End      bool   `json:"end,omitempty"`
	// WaitInput: an empty post that waits a moment for the controller's
	// input (component.LiveFramePost.WaitInput).
	WaitInput bool `json:"waitInput,omitempty"`
	// LeaveInput: the post takes none of the queued input (the input
	// loop's; component.LiveFramePost.LeaveInput).
	LeaveInput bool `json:"leaveInput,omitempty"`
	// Crashed: the page crashed (component.LiveFramePost.Crashed).
	Crashed bool `json:"crashed,omitempty"`
	// Dialog is the page's open dialog (component.LiveFramePost.Dialog).
	Dialog *component.LiveDialog `json:"dialog,omitempty"`
}

// sameDialog reports whether a and b are the same dialog (or both none).
func sameDialog(a, b *component.LiveDialog) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// liveFrame posts one frame (or an empty post) through the relay, under
// job (a job the session ran; "" the bound turn's).
func (g *relay) liveFrame(ctx context.Context, job string, post liveFramePost) (component.LiveFrameReply, error) {
	var reply component.LiveFrameReply
	raw, err := json.Marshal(post)
	if err != nil {
		return reply, err
	}
	path := "/v1/live"
	if job != "" {
		path += "?job=" + url.QueryEscape(job)
	}
	out, err := g.do(ctx, http.MethodPost, path, raw)
	if err != nil {
		return reply, err
	}
	err = json.Unmarshal(out, &reply)
	return reply, err
}

// apply runs a controller's input on the tab. Coordinates are fractions of
// the frame, mapped to the page's CSS pixels.
//
// A dialog's answer answers the dialog the post it came back to carried,
// only while that one is still open: an answer that crossed the dialog's
// close (a second click, a slow view) never answers the page's next one.
func (f *liveFeed) apply(ctx context.Context, in []component.LiveInput, carried *component.LiveDialog) {
	if len(in) == 0 {
		return
	}
	f.mu.Lock()
	view, t, handsOff := f.view, f.tab, f.handsOff
	f.mu.Unlock()
	for _, e := range in {
		// Hands off, the page is the person's in the browser window: what
		// they do in the room's view asks for it back (the next look
		// attaches again) and runs from then on.
		if handsOff {
			switch e.Type {
			case component.LiveInputMouse, component.LiveInputWheel, component.LiveInputKey, component.LiveInputText, component.LiveInputReload:
				f.mu.Lock()
				f.reattach = true
				f.mu.Unlock()
				continue
			}
		}
		if t == nil && e.Type != component.LiveInputFocus && e.Type != component.LiveInputClose {
			continue
		}
		x, y := e.X*view.W, e.Y*view.H
		switch e.Type {
		case component.LiveInputMouse:
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
			_ = t.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": kind, "x": x, "y": y, "button": button, "clickCount": clicks, "modifiers": e.Modifiers}, nil)
		case component.LiveInputWheel:
			_ = t.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": x, "y": y, "deltaX": e.DeltaX, "deltaY": e.DeltaY, "modifiers": e.Modifiers}, nil)
		case component.LiveInputKey:
			_ = t.call(ctx, "Input.dispatchKeyEvent", keyEvent(e), nil)
		case component.LiveInputText:
			if e.Text != "" {
				_ = t.call(ctx, "Input.insertText", map[string]any{"text": e.Text}, nil)
			}
		case component.LiveInputFocus:
			if f.onFocus != nil {
				f.onFocus(ctx)
			} else {
				f.bringForward(ctx)
			}
		case component.LiveInputReload:
			if cp, ok := t.(crashPage); ok {
				_ = cp.reload(ctx)
			}
		case component.LiveInputClose:
			f.mu.Lock()
			f.closed = true
			f.mu.Unlock()
		case component.LiveInputDialog:
			if dp, ok := t.(dialogPage); ok && carried != nil && sameDialog(dp.openDialog(), carried) {
				_ = dp.answerDialog(ctx, e.Action == "accept", e.Text)
				carried = nil // one answer per dialog
			}
		}
	}
}

// keyEvent is a key's CDP event. A printable key sends its text on press
// (that types it); the named keys carry the virtual key code Chrome acts on.
// An editing shortcut (Cmd or Ctrl with C, X, A, Z) carries its editing
// command: a synthesized key alone does not run it on macOS, so copy would
// do nothing. Copy writes the clipboard of the machine the browser runs on:
// the owner's, who is the only one who controls the view, so a copy in the
// room lands where they paste.
func keyEvent(e component.LiveInput) map[string]any {
	ev := map[string]any{"key": e.Key, "code": e.Code, "modifiers": e.Modifiers}
	if vk, ok := virtualKeys[e.Key]; ok {
		ev["windowsVirtualKeyCode"], ev["nativeVirtualKeyCode"] = vk, vk
	}
	if cmd, ok := editCommands[strings.ToLower(e.Key)]; ok && e.Modifiers&(2|4) != 0 && e.Action != "up" {
		if e.Modifiers&8 != 0 && cmd == "undo" {
			cmd = "redo"
		}
		ev["commands"] = []string{cmd}
		if vk := int(strings.ToUpper(e.Key)[0]); vk >= 'A' && vk <= 'Z' {
			ev["windowsVirtualKeyCode"], ev["nativeVirtualKeyCode"] = vk, vk
		}
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

// editCommands are the editing shortcuts' commands (Chrome's names).
var editCommands = map[string]string{"c": "copy", "x": "cut", "a": "selectAll", "z": "undo"}

var virtualKeys = map[string]int{
	"Backspace": 8, "Tab": 9, "Enter": 13, "Escape": 27, " ": 32,
	"PageUp": 33, "PageDown": 34, "End": 35, "Home": 36,
	"ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40, "Delete": 46,
}

// bringForward shows the run's window in front of the member's others
// (restored if minimized), with the run's tab selected.
func (f *liveFeed) bringForward(ctx context.Context) {
	h, ok := f.page().(interface{ bringForward(context.Context) error })
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
