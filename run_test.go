package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/core/pkgmeta"
	"github.com/luckylhb90/bnbloc/internal/wire"
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
	fr.bind(wire.RoomRelaySession{JobID: "job-1", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 10, ByRequester: true})
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

// The expense run end to end: the amount the agent gave is typed, the
// irreversible submit pauses at needs_confirmation, an agent "confirming"
// in the same turn changes nothing, the requester's own newer message
// lets it through, and DONE comes back with the page fenced.
func TestRunConfirmsByANewerTurn(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) {
		page, _ := q.State["page"].(map[string]any)
		switch {
		case page["title"] == "Submitted":
			return opDone, ""
		case strings.Contains(string(q.Questions["type_text_target"].Criteria), `"current_value":""`):
			return opTypeText, "Amount"
		default:
			return opClick, "Submit expense"
		}
	})
	h.model.predicate = 0.9
	head, text := h.call("browse", map[string]any{"goal": "file the expense", "url": "https://portal.example.com/expenses/new", "values": map[string]string{"amount": "120.50"}})
	if head["checkpoint"] != cpNeedsConfirmation || head["irreversibleProbability"] != 0.9 {
		t.Fatalf("first stop = %v", head)
	}
	if len(p.typed) != 1 || p.typed[0] != "120.50" || len(p.clicks) != 1 || p.clicks[0] != 1 {
		t.Fatalf("typed %v clicks %v; want the amount typed, nothing submitted", p.typed, p.clicks)
	}
	if !strings.Contains(text, "untrusted-content") || !strings.Contains(text, "Submit expense") {
		t.Fatalf("the page is not fenced: %s", text)
	}
	run := head["run"].(string)

	// The same turn: an agent persuaded the person "already agreed".
	head, _ = h.call("browse", map[string]any{"run": run, "confirmed": true})
	if head["checkpoint"] != cpNeedsConfirmation || !strings.Contains(head["note"].(string), "has not answered") {
		t.Fatalf("same-turn confirmation = %v", head)
	}
	// A newer turn, but a schedule's post, not the person.
	h.relay.bind(wire.RoomRelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 11, ByRequester: false})
	head, _ = h.call("browse", map[string]any{"run": run, "confirmed": true})
	if head["checkpoint"] != cpNeedsConfirmation {
		t.Fatalf("a scheduled turn confirmed it: %v", head)
	}
	if got := countClicks(p, 3); got != 0 {
		t.Fatalf("submitted before the person answered (%d)", got)
	}
	// maya answers in the room: a newer turn she opened.
	h.relay.bind(wire.RoomRelaySession{JobID: "job-3", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 12, ByRequester: true})
	head, text = h.call("browse", map[string]any{"run": run, "confirmed": true})
	if head["checkpoint"] != cpDone || countClicks(p, 3) != 1 {
		t.Fatalf("after maya's answer = %v (submit clicks %d)", head, countClicks(p, 3))
	}
	if !strings.Contains(text, "Expense submitted") {
		t.Fatalf("done page = %s", text)
	}
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

// A field the agent gave no value for stops at needs_input; the value
// resumes the held step; a decline of a confirmation is told to the model.
func TestNeedsInputAndDecline(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) {
		if strings.Contains(string(q.Questions["type_text_target"].Criteria), `"current_value":""`) {
			return opTypeText, "Amount"
		}
		return opClick, "Submit expense"
	})
	h.model.predicate = 0.95
	head, _ := h.call("browse", map[string]any{"goal": "file the expense", "url": "https://portal.example.com/expenses/new"})
	if head["checkpoint"] != cpNeedsInput || len(p.typed) != 0 {
		t.Fatalf("no value = %v typed %v", head, p.typed)
	}
	run := head["run"].(string)
	if head, _ = h.call("browse", map[string]any{"run": run}); head["checkpoint"] != cpNeedsInput {
		t.Fatalf("resumed without the value = %v", head)
	}
	head, _ = h.call("browse", map[string]any{"run": run, "value": "88"})
	if head["checkpoint"] != cpNeedsConfirmation || len(p.typed) != 1 || p.typed[0] != "88" {
		t.Fatalf("with the value = %v typed %v", head, p.typed)
	}
	h.relay.bind(wire.RoomRelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 20, ByRequester: true})
	h.model.script = func(q decReq) (string, string) { return opBlocked, "" }
	head, _ = h.call("browse", map[string]any{"run": run, "confirmed": false})
	if head["checkpoint"] != cpBlocked || countClicks(p, 3) != 0 {
		t.Fatalf("declined = %v", head)
	}
	r, _ := h.tools.runner.store.load(run)
	if !strings.Contains(r.Goal, `do not click "Submit expense"`) {
		t.Fatalf("the decline is not in the goal: %q", r.Goal)
	}
}

// Progress, not time, stops a run: three executed steps that change
// nothing block it (a WAIT does not count).
func TestStallBlocksByProgress(t *testing.T) {
	doc := &fakeDoc{url: "https://example.com/", title: "Stuck", key: "k", actions: []action{{Kind: "click", Node: node(1), Role: "button", Label: "Nothing"}}}
	p := &fakePage{doc: doc}
	waits := 0
	h := newHarness(t, p, func(q decReq) (string, string) {
		if waits < 2 {
			waits++
			return "WAIT", ""
		}
		return opClick, "Nothing"
	})
	head, _ := h.call("browse", map[string]any{"goal": "g", "url": "https://example.com/"})
	if head["checkpoint"] != cpBlocked || !strings.Contains(head["reason"].(string), "changed nothing") {
		t.Fatalf("stall = %v", head)
	}
	if h.model.steps != 2+stallSteps {
		t.Fatalf("steps = %d, want %d (waits never count)", h.model.steps, 2+stallSteps)
	}
}

// A page that changes between the decision and the input is never acted
// on: the target is refused and the run decides again on the new page.
func TestStaleTargetIsReDecided(t *testing.T) {
	a := &fakeDoc{url: "https://example.com/a", title: "A", key: "a", actions: []action{{Kind: "click", Node: node(1), Role: "link", Label: "Row 1"}}}
	b := &fakeDoc{url: "https://example.com/b", title: "B", key: "b", actions: []action{{Kind: "click", Node: node(2), Role: "link", Label: "Row 2"}}}
	p := &fakePage{doc: a}
	p.beforeTarget = func(p *fakePage) { p.doc = b }
	h := newHarness(t, p, func(q decReq) (string, string) {
		page, _ := q.State["page"].(map[string]any)
		if page["title"] == "B" {
			return opDone, ""
		}
		return opClick, "Row 1"
	})
	head, _ := h.call("browse", map[string]any{"goal": "g", "url": "https://example.com/a"})
	if head["checkpoint"] != cpDone || len(p.clicks) != 0 {
		t.Fatalf("stale = %v clicks %v", head, p.clicks)
	}
}

// A link off the run's site pauses at leaving_site; the requester's yes
// in a newer turn adds the site and follows it.
func TestLeavingSite(t *testing.T) {
	a := &fakeDoc{url: "https://shop.example.com/", title: "Shop", key: "a", actions: []action{{Kind: "click", Node: node(1), Role: "link", Label: "Pay", Href: "https://pay.other.test/checkout"}}}
	b := &fakeDoc{url: "https://pay.other.test/checkout", title: "Checkout", key: "b"}
	p := &fakePage{doc: a, onClick: func(p *fakePage, n int) { p.doc = b }}
	h := newHarness(t, p, func(q decReq) (string, string) {
		page, _ := q.State["page"].(map[string]any)
		if page["title"] == "Checkout" {
			return opDone, ""
		}
		return opClick, "Pay"
	})
	head, _ := h.call("browse", map[string]any{"goal": "g", "url": "https://shop.example.com/"})
	if head["checkpoint"] != cpLeavingSite || head["site"] != "other.test" || len(p.clicks) != 0 {
		t.Fatalf("leaving = %v", head)
	}
	h.relay.bind(wire.RoomRelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 11, ByRequester: true})
	head, _ = h.call("browse", map[string]any{"run": head["run"], "confirmed": true})
	if head["checkpoint"] != cpDone || len(p.clicks) != 1 {
		t.Fatalf("after yes = %v clicks %v", head, p.clicks)
	}
	if sites := head["sites"].([]any); len(sites) != 2 {
		t.Fatalf("sites = %v", sites)
	}
}

// A secret field is a person's (needs_human); an upload pauses for the
// requester's consent and takes a file inside the workspace only.
func TestSecretsAndUploads(t *testing.T) {
	login := &fakeDoc{url: "https://example.com/login", title: "Login", key: "l", actions: []action{{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"}}}
	p := &fakePage{doc: login}
	h := newHarness(t, p, func(q decReq) (string, string) { return opSecret, "Password" })
	head, _ := h.call("browse", map[string]any{"goal": "sign in", "url": "https://example.com/login"})
	if head["checkpoint"] != cpNeedsHuman || len(p.typed) != 0 {
		t.Fatalf("secret = %v", head)
	}

	form := &fakeDoc{url: "https://example.com/claim", title: "Claim", key: "c", actions: []action{{Kind: "upload", Node: node(5), Role: "file", Label: "Receipt"}}}
	p2 := &fakePage{doc: form}
	h2 := newHarness(t, p2, func(q decReq) (string, string) {
		if len(p2.files) > 0 {
			return opDone, ""
		}
		return opUpload, "Receipt"
	})
	head, _ = h2.call("browse", map[string]any{"goal": "attach the receipt", "url": "https://example.com/claim"})
	if head["checkpoint"] != cpUpload {
		t.Fatalf("upload = %v", head)
	}
	run := head["run"]
	wd, _ := os.Getwd()
	work := t.TempDir()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	_ = os.WriteFile(filepath.Join(work, "receipt.pdf"), []byte("%PDF"), 0o600)
	h2.relay.bind(wire.RoomRelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 11, ByRequester: true})
	head, _ = h2.call("browse", map[string]any{"run": run, "confirmed": true, "file": "../../etc/passwd"})
	if head["checkpoint"] != cpUpload || !strings.Contains(head["note"].(string), "workspace") || len(p2.files) != 0 {
		t.Fatalf("a file outside the workspace = %v", head)
	}
	head, _ = h2.call("browse", map[string]any{"run": run, "confirmed": true, "file": "receipt.pdf"})
	if head["checkpoint"] != cpDone || len(p2.files) != 1 || filepath.Base(p2.files[0]) != "receipt.pdf" {
		t.Fatalf("upload done = %v files %v", head, p2.files)
	}
}

// The owner's browser is theirs: another member's turn gets needs_human
// naming the owner (grants are the owner's to give), and a run continues
// only in its requester's turns.
func TestOwnerAndRequester(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return opBlocked, "" })
	h.relay.bind(wire.RoomRelaySession{JobID: "job-1", RoomID: "room-1", Requester: "jack", Owner: "maya", MessageSeq: 5, ByRequester: true})
	head, _ := h.call("browse", map[string]any{"goal": "g", "url": "https://portal.example.com/expenses/new"})
	if head["checkpoint"] != cpNeedsHuman || !strings.Contains(head["reason"].(string), "ask maya") || h.host.opened != 0 {
		t.Fatalf("a non-owner's run = %v (tabs opened %d)", head, h.host.opened)
	}
	h.relay.bind(wire.RoomRelaySession{JobID: "job-2", RoomID: "room-1", Requester: "maya", Owner: "maya", MessageSeq: 6, ByRequester: true})
	head, _ = h.call("browse", map[string]any{"goal": "g", "url": "https://portal.example.com/expenses/new"})
	h.relay.bind(wire.RoomRelaySession{JobID: "job-3", RoomID: "room-1", Requester: "jack", Owner: "maya", MessageSeq: 7, ByRequester: true})
	if msg := h.callErr("browse", map[string]any{"run": head["run"]}); !strings.Contains(msg, "acts for someone else") {
		t.Fatalf("another requester resumed it: %s", msg)
	}
}

// A run outlives its call: its state is saved by id and found again; a
// closed tab ends it; decisions off is a blocked checkpoint, not a failure.
func TestRunStateAndDecisionsOff(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return opBlocked, "" })
	head, _ := h.call("browse", map[string]any{"goal": "g", "url": "https://portal.example.com/expenses/new"})
	run := head["run"].(string)
	r, err := h.tools.runner.store.load(run)
	if err != nil || r.Tab != "tab-1" || r.Requester != "maya" || r.Checkpoint == nil || r.Checkpoint.Kind != cpBlocked || r.PausedAtSeq != 10 {
		t.Fatalf("saved = %+v %v", r, err)
	}
	if _, err := h.tools.runner.store.load("../x"); err == nil {
		t.Fatal("a path loaded as a run")
	}
	h.model.off = true
	head, _ = h.call("browse", map[string]any{"run": run, "reply": "try again"})
	if head["checkpoint"] != cpBlocked || !strings.Contains(head["reason"].(string), "decisions are off") {
		t.Fatalf("decisions off = %v", head)
	}
	delete(h.host.pages, "tab-1")
	if msg := h.callErr("browse", map[string]any{"run": run}); !strings.Contains(msg, "tab is closed") {
		t.Fatalf("closed tab = %s", msg)
	}
	if _, err := h.tools.runner.store.load(run); err == nil {
		t.Fatal("a run whose tab closed was kept")
	}
}

// A run still progressing when the call's budget runs out answers
// `running`; resuming keeps it going.
func TestRunningCheckpoint(t *testing.T) {
	steps := 0
	doc := &fakeDoc{url: "https://example.com/", title: "Long", key: "0", actions: []action{{Kind: "click", Node: node(1), Role: "button", Label: "Next"}}}
	p := &fakePage{doc: doc}
	p.onClick = func(p *fakePage, n int) {
		steps++
		next := *p.doc
		next.key = strings.Repeat("x", steps)
		p.doc = &next
	}
	h := newHarness(t, p, func(q decReq) (string, string) {
		if steps >= 3 {
			return opDone, ""
		}
		return opClick, "Next"
	})
	clock := time.Unix(0, 0)
	h.tools.runner.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	h.tools.runner.budget = 2 * time.Second
	head, _ := h.call("browse", map[string]any{"goal": "g", "url": "https://example.com/"})
	if head["checkpoint"] != cpRunning {
		t.Fatalf("budget = %v", head)
	}
	h.tools.runner.budget = time.Hour
	head, _ = h.call("browse", map[string]any{"run": head["run"]})
	if head["checkpoint"] != cpDone || steps != 3 {
		t.Fatalf("continued = %v steps %d", head, steps)
	}
}

// A finished download of the run's tab is a checkpoint naming the file.
func TestDownloadCheckpoint(t *testing.T) {
	doc := &fakeDoc{url: "https://example.com/report", title: "Report", key: "r", actions: []action{{Kind: "click", Node: node(1), Role: "button", Label: "Export"}}}
	p := &fakePage{doc: doc}
	h := newHarness(t, p, func(q decReq) (string, string) { return opBlocked, "" })
	h.host.dl = map[string][]string{"tab-1": {"downloads/report.csv"}}
	head, _ := h.call("browse", map[string]any{"goal": "export it", "url": "https://example.com/report"})
	if head["checkpoint"] != cpDownload || head["files"].([]any)[0] != "downloads/report.csv" {
		t.Fatalf("download = %v", head)
	}
}

// The MCP surface: initialize, three tools, and an unknown tool refused.
func TestMCPSurface(t *testing.T) {
	h := newHarness(t, &fakePage{doc: &fakeDoc{url: "https://example.com", key: "k"}}, nil)
	res, rerr := h.tools.dispatch(context.Background(), "initialize", nil)
	if rerr != nil || res.(map[string]any)["serverInfo"].(map[string]any)["name"] != "bnbloc-browser" {
		t.Fatalf("initialize = %v %v", res, rerr)
	}
	res, _ = h.tools.dispatch(context.Background(), "tools/list", nil)
	tools := res.(map[string]any)["tools"].([]any)
	// The published package names exactly the tools the binary serves, and
	// is a valid MCP package apart from its binaries.
	raw, err := os.ReadFile("../../packages/browser/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var draft pkgmeta.Package
	if err := json.Unmarshal(raw, &draft); err != nil {
		t.Fatal(err)
	}
	if _, err := pkgmeta.PrepareMCP(draft, []string{"darwin-arm64"}); err != nil {
		t.Fatalf("package: %v", err)
	}
	if len(tools) != len(draft.MCP.Tools) {
		t.Fatalf("tools = %v; package names %v", tools, draft.MCP.Tools)
	}
	for i, tl := range tools {
		if name := tl.(map[string]any)["name"]; name != draft.MCP.Tools[i].Name {
			t.Fatalf("tool %d = %v; package names %s", i, name, draft.MCP.Tools[i].Name)
		}
	}
	if _, rerr := h.tools.dispatch(context.Background(), "tools/call", json.RawMessage(`{"name":"click"}`)); rerr == nil {
		t.Fatal("an unknown tool ran")
	}
	if msg := h.callErr("browse", map[string]any{"goal": "g", "url": "file:///etc/passwd"}); !strings.Contains(msg, "http(s) url") {
		t.Fatalf("a file url = %s", msg)
	}
}
