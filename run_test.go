package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/component"
)

// expensePortal is a two-page fake site: a form (amount field, a "Preview"
// button, a "Submit expense" button) and a "Submitted" page.
func expensePortal() (*fakePage, *fakeDoc, *fakeDoc) {
	form := &fakeDoc{url: "https://portal.example.com/expenses/new", title: "New expense", text: "New expense", key: "form", actions: []action{
		{Kind: "fill", Node: node(1), Role: "textbox", Label: "Amount"},
		{Kind: "click", Node: node(2), Role: "button", Label: "Preview"},
		{Kind: "click", Node: node(3), Role: "button", Label: "Submit expense"},
	}}
	done := &fakeDoc{url: "https://portal.example.com/expenses/42", title: "Submitted", text: "Expense submitted", key: "done"}
	p := &fakePage{doc: form}
	p.onType = func(p *fakePage, n int, text string) {
		next := *p.doc
		next.key = p.doc.key + "+" + text
		next.actions = append([]action(nil), p.doc.actions...)
		next.actions[0].Value = text
		p.doc = &next
	}
	p.onClick = func(p *fakePage, n int) {
		if n == 3 {
			p.doc = done
		}
	}
	return p, form, done
}

// harness is the server with fakes: one relay, one host, the model.
type harness struct {
	t     *testing.T
	tools *tools
	relay *fakeRelay
	model *fakeModel
	host  *fakeHost
}

func newHarness(t *testing.T, p *fakePage, script func(q decReq) (string, string)) *harness {
	t.Helper()
	model := &fakeModel{script: script}
	fr := &fakeRelay{model: model}
	fr.bind(component.RelaySession{JobID: "job-1", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 10, ByRequester: true})
	host := &fakeHost{next: p}
	gw := fr.start(t)
	tl := &tools{gw: gw, runner: &runner{dec: gw, host: host, store: runStore{dir: t.TempDir()}, downloads: filepath.Join(t.TempDir(), "downloads"), budget: time.Minute, now: time.Now}}
	return &harness{t: t, tools: tl, relay: fr, model: model, host: host}
}

// call runs one tool and answers its head (the checkpoint JSON) and text.
func (h *harness) call(name string, args map[string]any) (map[string]any, string) {
	h.t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	res, rerr := h.tools.dispatch(context.Background(), "tools/call", raw)
	if rerr != nil {
		h.t.Fatalf("%s: %s", name, rerr.Message)
	}
	content := res.(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	head := text
	if i := strings.Index(text, "\n\n"); i >= 0 {
		head = text[:i]
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(head), &out); err != nil {
		h.t.Fatalf("%s head: %v\n%s", name, err, text)
	}
	return out, text
}

func (h *harness) callErr(name string, args map[string]any) string {
	h.t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	_, rerr := h.tools.dispatch(context.Background(), "tools/call", raw)
	if rerr == nil {
		h.t.Fatalf("%s succeeded", name)
	}
	return rerr.Message
}

func countClicks(p *fakePage, n int) int {
	c := 0
	for _, x := range p.clicks {
		if x == n {
			c++
		}
	}
	return c
}

// The keeper holds a paused run's view after the call: it posts under the
// pausing turn's job saying what it waits for, marks the view ready once
// the page moved back on the run's site with no password left, and posts
// the view's end when the run is resumed (stopKeeper) or the owner closes
// it from the room.
func TestKeeperHoldsTheViewUntilResumed(t *testing.T) {
	signIn := &fakeDoc{url: "https://login.idp.example.net/authorize", title: "Sign in", text: "Sign in", key: "idp", actions: []action{
		{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"},
	}}
	page := &fakePage{doc: signIn}
	h := newHarness(t, page, func(q decReq) (string, string) { return "", "" })
	var mu sync.Mutex
	var posts []liveFramePost
	closeIt := false
	h.relay.live = func(post liveFramePost) component.LiveFrameReply {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, post)
		if closeIt {
			return component.LiveFrameReply{Input: []component.LiveInput{{Type: component.LiveInputClose}}}
		}
		return component.LiveFrameReply{}
	}
	h.tools.keeper = func(run, job string) error { return nil }
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run := head["run"].(string)
	store := h.tools.runner.store

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- keep(ctx, h.tools.gw, h.host, store, run, "job-1") }()
	waitFor := func(what string, ok func(liveFramePost) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			for _, p := range posts {
				if ok(p) {
					mu.Unlock()
					return
				}
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no post %s: %+v", what, posts)
	}
	mu.Lock()
	posts = nil
	mu.Unlock()
	waitFor("waiting", func(p liveFramePost) bool { return p.Waiting == waitSignIn && !p.Ready })
	// A second keeper of the run exits at once.
	if err := keep(ctx, h.tools.gw, h.host, store, run, "job-1"); err != nil {
		t.Fatalf("second keeper = %v", err)
	}
	// Signed in: back on the portal, no password field.
	page.mu.Lock()
	page.doc = &fakeDoc{url: "https://portal.example.com/expenses/new", title: "New expense", text: "New expense", key: "form"}
	page.mu.Unlock()
	waitFor("ready", func(p liveFramePost) bool { return p.Ready && p.Waiting != "" })

	store.stopKeeper(run)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("keeper = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper did not stop")
	}
	waitFor("end", func(p liveFramePost) bool { return p.End })

	// Closed by the owner from the room.
	mu.Lock()
	closeIt, posts = true, nil
	mu.Unlock()
	page.mu.Lock()
	page.doc = signIn
	page.mu.Unlock()
	go func() { done <- keep(ctx, h.tools.gw, h.host, store, run, "job-1") }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper did not stop on close")
	}
	waitFor("end after close", func(p liveFramePost) bool { return p.End })
}

// A SAML hand-off page the browser left standing (blank, one form of
// hidden fields, its onload submit never ran) is submitted, and the
// identity provider's first sign-in step (a login name, no password yet)
// pauses at needs_human before any decision there.
func TestStuckSignInHandOffGoesOn(t *testing.T) {
	handOff := &fakeDoc{url: "https://identity.portal.example.com/oidc/authorize", title: "", text: "", key: "hand-off"}
	login := &fakeDoc{url: "https://sso.idp.example.net/ui/login/login", title: "Example SSO", text: "Enter your login data. Login Name", key: "idp", actions: []action{
		{Kind: "fill", Node: node(1), Role: "textbox", Label: "Login Name"},
		{Kind: "click", Node: node(2), Role: "button", Label: "Next"},
	}}
	fp := &fakePage{doc: handOff}
	posted := 0
	fp.onAutoPost = func(p *fakePage) { posted++; p.doc = login }
	h := newHarness(t, fp, func(q decReq) (string, string) {
		t.Errorf("a decision was asked on %v", q.State["page"])
		return "", ""
	})
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/home"})
	if head["checkpoint"] != cpNeedsHuman || posted != 1 {
		t.Fatalf("checkpoint = %v (hand-off posted %d)", head, posted)
	}
	if signInPage(&page{URL: "https://portal.example.com/reports", Title: "Reports"}) {
		t.Fatal("an ordinary page counted as a sign-in")
	}
}

// The room shows one view per job: a newer run's keeper takes the job and
// the older keeper leaves without posting an end (that would close the
// newer view).
func TestNewestKeeperHoldsTheJob(t *testing.T) {
	signIn := &fakeDoc{url: "https://login.idp.example.net/authorize", title: "Sign in", text: "Sign in", key: "idp", actions: []action{
		{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"},
	}}
	h := newHarness(t, &fakePage{doc: signIn}, func(q decReq) (string, string) { return "", "" })
	var mu sync.Mutex
	ends := 0
	h.relay.live = func(post liveFramePost) component.LiveFrameReply {
		mu.Lock()
		defer mu.Unlock()
		if post.End {
			ends++
		}
		return component.LiveFrameReply{}
	}
	h.tools.keeper = func(run, job string) error { return nil }
	first, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/"})
	second, _ := h.call("browser_open", map[string]any{"url": "https://travel.example.org/"})
	store := h.tools.runner.store
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- keep(ctx, h.tools.gw, h.host, store, first["run"].(string), "job-1") }()
	time.Sleep(300 * time.Millisecond)
	go func() { _ = keep(ctx, h.tools.gw, h.host, store, second["run"].(string), "job-1") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("older keeper = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the older keeper kept the job")
	}
	mu.Lock()
	defer mu.Unlock()
	if ends != 0 {
		t.Fatalf("the older keeper ended the job's view (%d ends)", ends)
	}
}

// A page that stops answering (a dialog the picture cannot show, a hung
// page) keeps its view: the keeper ends only once the tab is gone.
func TestKeeperHoldsAnUnresponsivePage(t *testing.T) {
	signIn := &fakeDoc{url: "https://login.idp.example.net/authorize", title: "Sign in", text: "Sign in", key: "idp", actions: []action{
		{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"},
	}}
	page := &fakePage{doc: signIn}
	h := newHarness(t, page, func(q decReq) (string, string) { return "", "" })
	var mu sync.Mutex
	ends := 0
	h.relay.live = func(post liveFramePost) component.LiveFrameReply {
		mu.Lock()
		defer mu.Unlock()
		if post.End {
			ends++
		}
		return component.LiveFrameReply{Watching: true}
	}
	h.tools.keeper = func(run, job string) error { return nil }
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/"})
	run := head["run"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- keep(ctx, h.tools.gw, h.host, h.tools.runner.store, run, "job-1") }()
	time.Sleep(300 * time.Millisecond)
	page.mu.Lock()
	page.hung = true
	page.mu.Unlock()
	select {
	case err := <-done:
		t.Fatalf("the keeper ended on a page that only stopped answering: %v", err)
	case <-time.After(2 * time.Second):
	}
	mu.Lock()
	if ends != 0 {
		mu.Unlock()
		t.Fatalf("the view was ended (%d) while its tab is open", ends)
	}
	mu.Unlock()
	h.host.mu.Lock()
	delete(h.host.pages, "tab-1")
	h.host.mu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, errTabGone) {
			t.Fatalf("keeper = %v, want errTabGone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper outlived its tab")
	}
}

// The keeper carries the page's dialog to the room and runs the answer
// that comes back; one still open when it ends is dismissed (nobody could
// answer it after).
func TestKeeperCarriesThePagesDialog(t *testing.T) {
	signIn := &fakeDoc{url: "https://login.idp.example.net/authorize", title: "Sign in", text: "Sign in", key: "idp", actions: []action{
		{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"},
	}}
	page := &fakePage{doc: signIn}
	h := newHarness(t, page, func(q decReq) (string, string) { return "", "" })
	var mu sync.Mutex
	var carried *component.LiveDialog
	answer, keeping := false, false
	h.relay.live = func(post liveFramePost) component.LiveFrameReply {
		mu.Lock()
		defer mu.Unlock()
		reply := component.LiveFrameReply{Watching: true}
		if keeping {
			reply.Controller = "maya" // the owner drives the view the keeper holds
		}
		if post.Dialog != nil {
			carried = post.Dialog
			if answer {
				reply.Input = []component.LiveInput{{Type: component.LiveInputDialog, Action: "accept"}}
			}
		}
		return reply
	}
	h.tools.keeper = func(run, job string) error { return nil }
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/"})
	run := head["run"].(string)
	if head["checkpoint"] != cpNeedsHuman {
		t.Fatalf("checkpoint = %v", head)
	}
	store := h.tools.runner.store
	mu.Lock()
	keeping = true
	mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- keep(ctx, h.tools.gw, h.host, store, run, "job-1") }()
	time.Sleep(200 * time.Millisecond)
	page.mu.Lock()
	page.dialog = &component.LiveDialog{Kind: component.LiveDialogConfirm, Message: "Trust this device?"}
	page.mu.Unlock()
	waitUntil := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal(what)
	}
	waitUntil("the dialog never reached the room", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return carried != nil && carried.Message == "Trust this device?"
	})
	mu.Lock()
	answer = true
	mu.Unlock()
	waitUntil("the room's answer never ran", func() bool {
		page.mu.Lock()
		defer page.mu.Unlock()
		return len(page.answers) == 1 && page.answers[0]
	})
	// A dialog still open at the end is dismissed.
	mu.Lock()
	answer = false
	mu.Unlock()
	page.mu.Lock()
	page.dialog = &component.LiveDialog{Kind: component.LiveDialogConfirm, Message: "Stay signed in?"}
	page.mu.Unlock()
	store.stopKeeper(run)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper did not stop")
	}
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.dialog != nil || len(page.answers) != 2 || page.answers[1] {
		t.Fatalf("the open dialog was left behind: %+v answers %v", page.dialog, page.answers)
	}
}

// signInDoc is an identity provider's password page.
func signInDoc() *fakeDoc {
	return &fakeDoc{url: "https://login.idp.example.net/authorize", title: "Sign in", text: "Sign in", key: "idp", actions: []action{
		{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"},
	}}
}

// A browser signs in to a site once at a time: opening a site where a run
// waits for the person to sign in answers that run (shown again under this
// turn) instead of a second tab, whose sign-in would undo the first. Once
// its tab is gone, the room's run opens anew.
func TestOneSignInPerSite(t *testing.T) {
	h := newHarness(t, &fakePage{doc: signInDoc()}, func(q decReq) (string, string) { return "", "" })
	var kept [][2]string
	h.tools.keeper = func(run, job string) error { kept = append(kept, [2]string{run, job}); return nil }
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run, _ := head["run"].(string)
	if head["checkpoint"] != cpNeedsHuman || run == "" {
		t.Fatalf("first open = %v", head)
	}
	h.relay.bind(component.RelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 11, ByRequester: true})
	again, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses"})
	if again["run"] != run || again["checkpoint"] != cpNeedsHuman || h.host.opened != 1 {
		t.Fatalf("a second sign-in started: %v (tabs opened %d)", again, h.host.opened)
	}
	if last := kept[len(kept)-1]; last != [2]string{run, "job-2"} {
		t.Fatalf("the waiting view was not shown under this turn: %v", kept)
	}
	// The tab is gone: the run is dropped and a new one starts.
	h.host.mu.Lock()
	delete(h.host.pages, "tab-1")
	h.host.mu.Unlock()
	fresh, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses"})
	if fresh["run"] == run || h.host.opened != 2 {
		t.Fatalf("after the tab closed = %v (tabs opened %d)", fresh, h.host.opened)
	}
	if _, err := h.tools.runner.store.load(run); err == nil {
		t.Fatal("the run of the closed tab was kept")
	}
}

// A room has one run per requester (a later open navigates it); close ends
// the room's runs, or one by id, each tab closed and the run forgotten;
// another room's run is not touched.
func TestCloseEndsRuns(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	a, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/"})
	b, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	if a["run"] != b["run"] || h.host.opened != 1 {
		t.Fatalf("a second open in the room = %v (tabs opened %d)", b, h.host.opened)
	}
	h.relay.bind(component.RelaySession{JobID: "job-9", RoomID: "room-2", Requester: "maya", Owner: "maya", MessageSeq: 3, ByRequester: true})
	other, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/"})
	if other["run"] == a["run"] {
		t.Fatalf("another room used the first room's run")
	}
	h.relay.bind(component.RelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 11, ByRequester: true})
	_, text := h.call("browser_close", map[string]any{})
	if !strings.Contains(text, a["run"].(string)) || strings.Contains(text, other["run"].(string)) {
		t.Fatalf("close = %s", text)
	}
	store := h.tools.runner.store
	if _, err := store.load(a["run"].(string)); err == nil {
		t.Fatal("the closed run was kept")
	}
	h.relay.bind(component.RelaySession{JobID: "job-10", RoomID: "room-2", Requester: "maya", Owner: "maya", MessageSeq: 4, ByRequester: true})
	if _, text := h.call("browser_close", map[string]any{"run": other["run"]}); !strings.Contains(text, other["run"].(string)) {
		t.Fatalf("close by id = %s", text)
	}
	h.host.mu.Lock()
	tabs := len(h.host.pages)
	h.host.mu.Unlock()
	if tabs != 0 {
		t.Fatalf("%d tabs left open", tabs)
	}
}
