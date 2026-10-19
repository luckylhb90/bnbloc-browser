package main

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"

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
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
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
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	if head["checkpoint"] != nil {
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

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// An editing shortcut carries its command (a synthesized Cmd+C alone copies
// nothing on macOS); plain keys and releases carry none.
func TestKeyEventEditCommands(t *testing.T) {
	for _, c := range []struct {
		in   component.LiveInput
		want string
	}{
		{component.LiveInput{Type: component.LiveInputKey, Action: "down", Key: "c", Code: "KeyC", Modifiers: 4}, "copy"},
		{component.LiveInput{Type: component.LiveInputKey, Action: "down", Key: "x", Code: "KeyX", Modifiers: 2}, "cut"},
		{component.LiveInput{Type: component.LiveInputKey, Action: "down", Key: "z", Code: "KeyZ", Modifiers: 4 | 8}, "redo"},
		{component.LiveInput{Type: component.LiveInputKey, Action: "down", Key: "c", Code: "KeyC"}, ""},
		{component.LiveInput{Type: component.LiveInputKey, Action: "up", Key: "c", Code: "KeyC", Modifiers: 4}, ""},
	} {
		ev := keyEvent(c.in)
		got, _ := ev["commands"].([]string)
		if (c.want == "" && got != nil) || (c.want != "" && (len(got) != 1 || got[0] != c.want)) {
			t.Fatalf("%+v: commands = %v, want %q", c.in, got, c.want)
		}
	}
}

// A dialog's answer answers only the dialog its post carried, once: one
// that crossed that dialog's close (a second click, a slow view) never
// answers the page's next dialog.
func TestDialogAnswerNeverCrossesToTheNextDialog(t *testing.T) {
	a := &component.LiveDialog{Kind: component.LiveDialogConfirm, Message: "Trust this device?"}
	b := &component.LiveDialog{Kind: component.LiveDialogConfirm, Message: "Delete the draft?"}
	p := &fakePage{doc: &fakeDoc{url: "https://portal.example.com/", key: "x"}, dialog: b}
	f := &liveFeed{tab: &fakeTab{fakePage: p, id: "tab-1"}}
	accept := []component.LiveInput{{Type: component.LiveInputDialog, Action: "accept"}}
	f.apply(context.Background(), accept, a)
	if p.dialog == nil || len(p.answers) != 0 {
		t.Fatalf("an answer to %q answered %q", a.Message, b.Message)
	}
	f.apply(context.Background(), append(accept, accept...), b)
	if p.dialog != nil || len(p.answers) != 1 || !p.answers[0] {
		t.Fatalf("answers = %v, want the carried dialog answered once", p.answers)
	}
}
