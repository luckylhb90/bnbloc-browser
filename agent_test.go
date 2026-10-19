package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/component"
	"github.com/luckylhb90/bnbloc/core/pkgmeta"
)

// The agent drives: open answers the page with its numbered controls,
// find narrows to a control, act types and clicks by number.
func TestAgentOpensFindsAndActs(t *testing.T) {
	p, _, done := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "Submit expense" })
	head, text := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run, _ := head["run"].(string)
	if run == "" || head["checkpoint"] != nil {
		t.Fatalf("open = %v", head)
	}
	for _, want := range []string{"Controls (act on one by its number)", `[1] textbox "Amount" = (empty)`, `[3] button "Submit expense"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("the view lacks %q:\n%s", want, text)
		}
	}
	found, ftext := h.call("browser_find", map[string]any{"run": run, "what": "the submit button"})
	list, _ := found["found"].([]any)
	if len(list) == 0 || list[0].(map[string]any)["element"] != float64(3) || !strings.Contains(ftext, "untrusted-content") {
		t.Fatalf("find = %v\n%s", found, ftext)
	}
	if head, _ := h.call("browser_act", map[string]any{"run": run, "action": "type", "element": 1, "text": "120.50"}); head["checkpoint"] != nil || !contains(p.typed, "120.50") {
		t.Fatalf("type = %v typed %v", head, p.typed)
	}
	if head, _ := h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 3}); head["checkpoint"] != nil || p.doc != done {
		t.Fatalf("click = %v", head)
	}
	if _, text := h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 99}); !strings.Contains(text, "no control [99]") {
		t.Fatalf("a number not on the page:\n%s", text)
	}
	if _, text := h.call("browser_act", map[string]any{"run": run, "action": "press", "key": "Enter"}); !contains(p.keys, "Enter") {
		t.Fatalf("press = %v\n%s", p.keys, text)
	}
}

// A click the predicate judges irreversible pauses; confirmed in a turn the
// requester opened with their own message goes through, even with the
// control renumbered or no pause before it ("submit all fifteen, don't ask
// me"), while a turn a schedule or another agent opened cannot confirm.
func TestIrreversibleClickWaitsForTheRequester(t *testing.T) {
	p, _, done := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	h.model.predicate = 0.9
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run := head["run"].(string)
	head, _ = h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 3})
	if head["checkpoint"] != cpNeedsConfirmation || countClicks(p, 3) != 0 {
		t.Fatalf("click = %v (clicks %v)", head, p.clicks)
	}
	h.relay.bind(component.RelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 11})
	head, _ = h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 3, "confirmed": true})
	if !strings.Contains(head["note"].(string), "own message") || countClicks(p, 3) != 0 {
		t.Fatalf("confirmed in a turn maya did not open = %v", head)
	}
	h.relay.bind(component.RelaySession{JobID: "job-3", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 12, ByRequester: true})
	head, _ = h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 3, "confirmed": true})
	if head["checkpoint"] != nil || p.doc != done {
		t.Fatalf("confirmed in maya's turn = %v", head)
	}
}

// confirmed needs no pause first: the person's message already asked.
func TestConfirmedClickNeedsNoPauseFirst(t *testing.T) {
	p, _, done := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	h.model.predicate = 0.9
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	head, _ = h.call("browser_act", map[string]any{"run": head["run"].(string), "action": "click", "element": 3, "confirmed": true})
	if head["checkpoint"] != nil || p.doc != done {
		t.Fatalf("confirmed click = %v", head)
	}
}

// A secret field is never typed into: the person fills it.
func TestSecretIsNeverTyped(t *testing.T) {
	login := &fakeDoc{url: "https://portal.example.com/login", title: "Sign in", text: "Sign in", key: "login", actions: []action{
		{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"},
	}}
	p := &fakePage{doc: login}
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	h.tools.keeper = func(run, job string) error { return nil }
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/login"})
	head, _ = h.call("browser_act", map[string]any{"run": head["run"], "action": "type", "element": 1, "text": "hunter2"})
	if head["checkpoint"] != cpNeedsHuman || len(p.typed) != 0 {
		t.Fatalf("type into a secret = %v typed %v", head, p.typed)
	}
}

// A single sign-on the person already has passes through the identity
// provider by itself: no pause (QA 2026-10-19: the person was asked to sign
// in while signed in). One that stays on its page is the person's.
func TestSingleSignOnPassingThroughIsNoPause(t *testing.T) {
	p, form, _ := expensePortal()
	idp := &fakeDoc{url: "https://sso.idp.example.net/ui/login/login", title: "Example SSO", text: "Signing you in", key: "idp"}
	var mu sync.Mutex
	back := true
	p.onNavigate = func(p *fakePage, _ string) {
		p.doc = idp
		mu.Lock()
		comesBack := back
		mu.Unlock()
		if comesBack {
			go func() {
				time.Sleep(800 * time.Millisecond)
				p.mu.Lock()
				p.doc = form
				p.mu.Unlock()
			}()
		}
	}
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	h.tools.keeper = func(run, job string) error { return nil }
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	if head["checkpoint"] != nil {
		t.Fatalf("a sign-on that passed through paused: %v", head)
	}
	mu.Lock()
	back = false
	mu.Unlock()
	head, _ = h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses"})
	if head["checkpoint"] != cpNeedsHuman {
		t.Fatalf("a sign-in page that stayed = %v", head)
	}
}

// A click on a link to another site waits for the requester's word; then
// the site is the run's.
func TestLeavingTheSitesWaitsForTheRequester(t *testing.T) {
	doc := &fakeDoc{url: "https://portal.example.com/", title: "Portal", text: "Portal", key: "p", actions: []action{
		{Kind: "click", Node: node(1), Role: "link", Label: "Travel desk", Href: "https://travel.example.org/desk"},
	}}
	p := &fakePage{doc: doc}
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/"})
	run := head["run"].(string)
	head, _ = h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 1})
	if head["checkpoint"] != cpLeavingSite || head["site"] != "example.org" || len(p.clicks) != 0 {
		t.Fatalf("click off the sites = %v", head)
	}
	h.relay.bind(component.RelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 11, ByRequester: true})
	head, _ = h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 1, "confirmed": true})
	if countClicks(p, 1) != 1 || !strings.Contains(strings.Join(toStrings(head["sites"]), ","), "example.org") {
		t.Fatalf("confirmed = %v", head)
	}
}

// While a person drives from the room, the agent's action is refused with
// who drives.
func TestActWaitsForThePersonDriving(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	h.relay.live = func(liveFramePost) component.LiveFrameReply { return component.LiveFrameReply{Controller: "maya"} }
	head, _ = h.call("browser_act", map[string]any{"run": head["run"], "action": "click", "element": 2})
	if !strings.Contains(head["note"].(string), "maya is driving") || len(p.clicks) != 0 {
		t.Fatalf("act while maya drives = %v (clicks %v)", head, p.clicks)
	}
}

// A page's question after a click waits for the requester; their word lets
// the repeated click's question be accepted. An alert is accepted.
func TestPagesQuestionWaitsForTheRequester(t *testing.T) {
	p, _, _ := expensePortal()
	previewed := &fakeDoc{url: "https://portal.example.com/expenses/new", title: "Previewed", text: "Preview", key: "previewed"}
	p.onClick = func(p *fakePage, n int) {
		if n == 2 {
			p.dialog = &component.LiveDialog{Kind: component.LiveDialogConfirm, Message: "Discard your draft?"}
		}
	}
	p.onAnswer = func(p *fakePage, accept bool, _ string) {
		if accept {
			p.doc = previewed
		}
	}
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run := head["run"].(string)
	head, text := h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 2})
	if head["checkpoint"] != cpNeedsConfirmation || len(p.answers) != 1 || p.answers[0] {
		t.Fatalf("question = %v (answers %v)", head, p.answers)
	}
	if i, j := strings.Index(text, "untrusted-content"), strings.Index(text, "Discard your draft?"); i < 0 || j < i {
		t.Fatalf("the question is not fenced:\n%s", text)
	}
	h.relay.bind(component.RelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 11, ByRequester: true})
	head, _ = h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 2, "confirmed": true})
	if head["checkpoint"] != nil || len(p.answers) != 2 || !p.answers[1] || p.doc != previewed {
		t.Fatalf("confirmed = %v (answers %v)", head, p.answers)
	}
}

// A page that opens a new tab is followed there.
func TestRunFollowsANewTab(t *testing.T) {
	p, _, _ := expensePortal()
	lesson := &fakeDoc{url: "https://portal.example.com/lesson/1", title: "Lesson 1", text: "Lesson", key: "lesson"}
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	p.onClick = func(_ *fakePage, n int) {
		if n == 2 {
			h.host.mu.Lock()
			h.host.pages["tab-9"] = &fakePage{doc: lesson}
			if h.host.openers == nil {
				h.host.openers = map[string]string{}
			}
			h.host.openers["tab-9"] = "tab-1"
			h.host.mu.Unlock()
		}
	}
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run := head["run"].(string)
	head, text := h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 2})
	r, _ := h.tools.runner.store.load(run)
	if r.Tab != "tab-9" || !strings.Contains(text, "Lesson 1") || !strings.Contains(head["note"].(string), "new tab") {
		t.Fatalf("the run did not follow the new tab: tab %s\n%s", r.Tab, text)
	}
}

// A page that crashed after an action is reloaded once.
func TestCrashedPageIsReloaded(t *testing.T) {
	p, _, _ := expensePortal()
	p.onClick = func(p *fakePage, n int) {
		if n == 2 {
			p.crashed = true
		}
	}
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	h.call("browser_act", map[string]any{"run": head["run"], "action": "click", "element": 2})
	if p.reloads != 1 || p.crashed {
		t.Fatalf("reloads = %d crashed = %v", p.reloads, p.crashed)
	}
}

// The tools an agent sees.
func TestToolList(t *testing.T) {
	h := newHarness(t, &fakePage{doc: &fakeDoc{url: "https://portal.example.com/"}}, func(q decReq) (string, string) { return "", "" })
	res, rerr := h.tools.dispatch(t.Context(), "tools/list", nil)
	if rerr != nil {
		t.Fatal(rerr.Message)
	}
	var names []string
	for _, tool := range res.(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "browser_open,browser_view,browser_find,browser_act,browser_close" {
		t.Fatalf("tools = %v", names)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// The package declares the tools the server lists.
func TestPackageDeclaresTheTools(t *testing.T) {
	raw, err := os.ReadFile("package.json")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := parsePublished(raw)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, tl := range pkg.MCP.Tools {
		declared = append(declared, tl.Name)
	}
	if strings.Join(declared, ",") != "browser_open,browser_view,browser_find,browser_act,browser_close" {
		t.Fatalf("package.json declares %v", declared)
	}
	// The parse is the platform's: a setting it would refuse fails here.
	bad := strings.Replace(string(raw), `"BNBLOC_BROWSER_CHROME"`, `"PATH"`, 1)
	if _, err := parsePublished([]byte(bad)); err == nil {
		t.Fatal("a setting delivered as PATH was accepted")
	}
}

// parsePublished parses package.json with the platform's own parser, which
// validates it as the gateway does, with a stand-in for the artifacts a
// publish adds.
func parsePublished(raw []byte) (pkgmeta.Package, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return pkgmeta.Package{}, err
	}
	sum := strings.Repeat("0", 64)
	doc["artifacts"] = map[string]any{"darwin-arm64": map[string]any{"url": "https://gateway.example.com/v1/artifacts/sha256/" + sum, "sha256": sum}}
	full, _ := json.Marshal(doc)
	return pkgmeta.ParsePackage(full)
}
