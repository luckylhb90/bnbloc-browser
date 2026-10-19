package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/component"
)

// keeperHarness runs a keeper over a run paused for a sign-in, with a relay
// whose replies the test sets (reply) and whose posts it reads (posts).
type keeperHarness struct {
	t     *testing.T
	h     *harness
	page  *fakePage
	mu    sync.Mutex
	posts []liveFramePost
	reply func(post liveFramePost) component.LiveFrameReply
	done  chan error
}

func newKeeperHarness(t *testing.T) *keeperHarness {
	t.Helper()
	signIn := &fakeDoc{url: "https://login.idp.example.net/authorize", title: "Sign in", text: "Sign in", key: "idp", actions: []action{
		{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"},
	}}
	k := &keeperHarness{t: t, page: &fakePage{doc: signIn}, done: make(chan error, 1)}
	k.h = newHarness(t, k.page, func(q decReq) (string, string) { return opNeedsHuman, "" })
	keeping := false
	k.h.relay.live = func(post liveFramePost) component.LiveFrameReply {
		k.mu.Lock()
		defer k.mu.Unlock()
		if !keeping {
			return component.LiveFrameReply{}
		}
		k.posts = append(k.posts, post)
		if k.reply != nil {
			return k.reply(post)
		}
		return component.LiveFrameReply{Watching: true, Controller: "maya"}
	}
	k.h.tools.keeper = func(run, job string) error { return nil }
	head, _ := k.h.call("browse", map[string]any{"goal": "sign in", "url": "https://portal.example.com/"})
	if head["checkpoint"] != cpNeedsHuman {
		t.Fatalf("checkpoint = %v", head)
	}
	k.mu.Lock()
	keeping = true
	k.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	run := head["run"].(string)
	go func() { k.done <- keep(ctx, k.h.tools.gw, k.h.host, k.h.tools.runner.store, run, "job-1") }()
	return k
}

// once makes the relay answer input with the next post that ok accepts.
func (k *keeperHarness) once(in component.LiveInput) {
	k.mu.Lock()
	defer k.mu.Unlock()
	sent := false
	k.reply = func(post liveFramePost) component.LiveFrameReply {
		r := component.LiveFrameReply{Watching: true, Controller: "maya"}
		if !sent {
			sent = true
			r.Input = []component.LiveInput{in}
		}
		return r
	}
}

func (k *keeperHarness) waitPost(what string, ok func(liveFramePost) bool) {
	k.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		k.mu.Lock()
		for _, p := range k.posts {
			if ok(p) {
				k.posts = nil
				k.mu.Unlock()
				return
			}
		}
		k.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	k.t.Fatalf("no post %s", what)
}

// "Open on this computer": the window comes forward and the keeper lets go
// of the page (no call to it while the person works there); the address
// says when it looks done; the person's next action in the room's view
// takes the page back.
func TestKeeperGoesHandsOffForTheBrowserWindow(t *testing.T) {
	k := newKeeperHarness(t)
	k.waitPost("waiting", func(p liveFramePost) bool { return p.Waiting == waitSignIn })
	k.once(component.LiveInput{Type: component.LiveInputFocus})
	k.waitPost("hands off", func(p liveFramePost) bool { return p.Waiting == handsOffWaiting })
	k.h.host.mu.Lock()
	forwarded := len(k.h.host.forwarded)
	k.h.host.mu.Unlock()
	if forwarded != 1 {
		t.Fatalf("windows brought forward = %d", forwarded)
	}
	k.page.mu.Lock()
	calls := k.page.calls
	k.page.mu.Unlock()
	time.Sleep(time.Second)
	k.page.mu.Lock()
	if k.page.calls != calls {
		k.page.mu.Unlock()
		t.Fatalf("the page was called %d times while hands off", k.page.calls-calls)
	}
	// Signed in: back on the run's site.
	k.page.doc = &fakeDoc{url: "https://portal.example.com/home", title: "Home", text: "Home", key: "home"}
	k.page.mu.Unlock()
	k.waitPost("ready", func(p liveFramePost) bool { return p.Ready && p.Waiting == handsOffWaiting })
	// An action in the room's view takes the page back.
	k.once(component.LiveInput{Type: component.LiveInputMouse, Action: "down", X: 0.5, Y: 0.5})
	k.waitPost("attached again", func(p liveFramePost) bool { return p.Waiting == waitSignIn && p.Data != "" })
}

// A page that crashed says so to the room; the owner's reload brings it
// back.
func TestKeeperReportsACrashAndReloads(t *testing.T) {
	k := newKeeperHarness(t)
	k.waitPost("waiting", func(p liveFramePost) bool { return p.Waiting == waitSignIn })
	k.page.mu.Lock()
	k.page.crashed = true
	k.page.mu.Unlock()
	k.waitPost("crashed", func(p liveFramePost) bool { return p.Crashed })
	k.once(component.LiveInput{Type: component.LiveInputReload})
	k.waitPost("reloaded", func(p liveFramePost) bool { return !p.Crashed })
	k.page.mu.Lock()
	defer k.page.mu.Unlock()
	if k.page.reloads != 1 {
		t.Fatalf("reloads = %d", k.page.reloads)
	}
}

// A page that crashes while the run works is reloaded once; one that
// crashes again stops the run with that reason.
func TestRunReloadsACrashedPageOnce(t *testing.T) {
	p, _, _ := expensePortal()
	crashes := 1
	h := newHarness(t, p, func(q decReq) (string, string) {
		p.mu.Lock()
		if crashes > 0 {
			crashes--
			p.crashed = true
		}
		p.mu.Unlock()
		return opDone, ""
	})
	head, _ := h.call("browse", map[string]any{"goal": "look", "url": "https://portal.example.com/expenses/new"})
	if head["checkpoint"] != cpDone || p.reloads != 1 {
		t.Fatalf("checkpoint = %v reloads = %d", head, p.reloads)
	}
	crashes = 2
	head, _ = h.call("browse", map[string]any{"goal": "look again", "url": "https://portal.example.com/expenses/new"})
	if head["checkpoint"] != cpBlocked {
		t.Fatalf("a page that keeps crashing = %v", head)
	}
}
