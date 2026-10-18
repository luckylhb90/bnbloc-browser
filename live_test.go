package main

import (
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/component"
)

// While a call works, the room sees the page: a frame is posted when the
// picture changes (with the page's title and the frame's pixel size), and
// a controller's input comes back in the reply and runs on the tab.
func TestLiveViewPostsFramesAndRunsInput(t *testing.T) {
	p, _, _ := expensePortal()
	var mu sync.Mutex
	var posts []liveFramePost
	sentInput := false
	h := newHarness(t, p, func(q decReq) (string, string) {
		// Give the feed time for a frame before the run ends.
		time.Sleep(300 * time.Millisecond)
		return opDone, ""
	})
	h.relay.live = func(post liveFramePost) component.LiveFrameReply {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, post)
		reply := component.LiveFrameReply{Watching: true}
		if !sentInput && post.Data != "" {
			sentInput = true
			// x = 0.002 of a 1000-wide page is node 2 ("Preview") in the fake.
			reply.Input = []component.LiveInput{
				{Type: component.LiveInputMouse, Action: "down", X: 0.002, Y: 0.5},
				{Type: component.LiveInputMouse, Action: "up", X: 0.002, Y: 0.5},
				{Type: component.LiveInputText, Text: "typed live"},
			}
		}
		return reply
	}
	head, _ := h.call("browse", map[string]any{"goal": "look around", "url": "https://portal.example.com/expenses/new"})
	if head["checkpoint"] != cpDone {
		t.Fatalf("checkpoint = %v", head)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) == 0 || posts[0].Data == "" {
		t.Fatalf("no frame was posted: %+v", posts)
	}
	f := posts[0]
	shot, _ := base64.StdEncoding.DecodeString(f.Data)
	if f.MimeType != "image/jpeg" || !strings.HasPrefix(string(shot), "shot of ") || f.Run == "" || f.Seq <= 0 {
		t.Fatalf("frame = %+v (%q)", f, shot)
	}
	if f.Width != 1000 || f.Height != 500 || f.URL != "https://portal.example.com/" {
		t.Fatalf("frame size/url = %dx%d %s", f.Width, f.Height, f.URL)
	}
	for _, later := range posts[1:] {
		if later.Data != "" && later.Data == f.Data {
			t.Fatalf("an unchanged picture was posted again")
		}
	}
	if !containsInt(p.clicks, 2) || !contains(p.typed, "typed live") {
		t.Fatalf("live input did not run: clicks %v typed %v", p.clicks, p.typed)
	}
}

// While a person holds control from the room, the run takes no step: the
// model is not asked until control is handed back.
func TestRunWaitsWhileSomeoneHasControl(t *testing.T) {
	p, _, _ := expensePortal()
	var mu sync.Mutex
	controller := "maya"
	h := newHarness(t, p, func(q decReq) (string, string) { return opDone, "" })
	h.relay.live = func(post liveFramePost) component.LiveFrameReply {
		mu.Lock()
		defer mu.Unlock()
		return component.LiveFrameReply{Controller: controller}
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		mu.Lock()
		controller = ""
		mu.Unlock()
	}()
	start := time.Now()
	head, _ := h.call("browse", map[string]any{"goal": "look around", "url": "https://portal.example.com/expenses/new"})
	if head["checkpoint"] != cpDone {
		t.Fatalf("checkpoint = %v", head)
	}
	if time.Since(start) < time.Second {
		t.Fatalf("the run stepped while maya had control (%v)", time.Since(start))
	}
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
