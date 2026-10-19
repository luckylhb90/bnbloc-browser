package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// replayOf reads a run's replay page from the workspace: its steps, and
// the page itself.
func replayOf(t *testing.T, workspace, run string) ([]replayStep, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workspace, replayDir, run+".html"))
	if err != nil {
		t.Fatalf("no replay: %v", err)
	}
	page := string(raw)
	const open = `<script id="data" type="application/json">`
	i := strings.Index(page, open)
	j := strings.Index(page[i:], "</script>")
	var d struct{ Steps []replayStep }
	if i < 0 || j < 0 || json.Unmarshal([]byte(page[i+len(open):i+j]), &d) != nil {
		t.Fatalf("the replay carries no steps:\n%s", page)
	}
	return d.Steps, page
}

// Each action that ran is a step of the replay with the page after it; a
// refused one is no step, and page text stays data.
func TestReplayRecordsWhatRan(t *testing.T) {
	p, form, _ := expensePortal()
	form.actions[1].Label = `Preview</script><img src=x onerror=alert(1)>`
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	h.tools.workspace = t.TempDir()
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run := head["run"].(string)
	h.call("browser_act", map[string]any{"run": run, "action": "type", "element": 1, "text": "120.50"})
	h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 99})
	h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 2})
	h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 3})
	steps, page := replayOf(t, h.tools.workspace, run)
	var whats []string
	for _, s := range steps {
		whats = append(whats, s.What)
		if s.Frame == "" {
			t.Fatalf("step %d has no picture: %+v", s.Step, s)
		}
	}
	want := []string{"Opened https://portal.example.com/expenses/new", `Typed into "Amount"`, `Clicked "Preview</script><img src=x onerror=alert(1)>"`, `Clicked "Submit expense"`}
	if strings.Join(whats, "|") != strings.Join(want, "|") || steps[1].Text != "120.50" {
		t.Fatalf("steps = %q (%+v)", whats, steps[1])
	}
	if strings.Contains(page, "</script><img") {
		t.Fatal("page text ended the replay's data")
	}
}

// A sign-in is never pictured: the replay says a person signed in there.
// Closing the run drops its frames, not its replay.
func TestReplayNeverPicturesASignIn(t *testing.T) {
	login := &fakeDoc{url: "https://portal.example.com/login", title: "Sign in", text: "Sign in", key: "login", actions: []action{
		{Kind: "secret", Node: node(1), Role: "textbox", Label: "Password"},
	}}
	h := newHarness(t, &fakePage{doc: login}, func(q decReq) (string, string) { return "", "" })
	h.tools.keeper = func(run, job string) error { return nil }
	h.tools.workspace = t.TempDir()
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/login"})
	run := head["run"].(string)
	if head, _ = h.call("browser_act", map[string]any{"run": run, "action": "type", "element": 1, "text": "hunter2"}); head["checkpoint"] != cpNeedsHuman {
		t.Fatalf("type into the password = %v", head)
	}
	steps, _ := replayOf(t, h.tools.workspace, run)
	if len(steps) != 2 || steps[0].Frame != "" || steps[1].What != "A person signs in here" || steps[1].Frame != "" {
		t.Fatalf("steps = %+v", steps)
	}
	if _, err := os.Stat(h.tools.runner.store.framesDir(run)); err == nil {
		t.Fatal("a sign-in page was pictured")
	}
	h.tools.runner.store.remove(run)
	if _, err := os.Stat(filepath.Join(h.tools.workspace, replayDir, run+".html")); err != nil {
		t.Fatalf("the replay went with the run: %v", err)
	}
}

// Frames are kept newest first within the budget; an older step past it
// keeps its text.
func TestReplayKeepsNewestFramesWithinBudget(t *testing.T) {
	store := runStore{dir: t.TempDir()}
	tl := &tools{runner: &runner{store: store}, workspace: t.TempDir()}
	r := &runState{ID: "brun_budget"}
	big := strings.Repeat("A", replayFrameBudget/2-10)
	_ = os.MkdirAll(store.framesDir(r.ID), 0o700)
	for i := 1; i <= 3; i++ {
		r.History = append(r.History, historyRecord{Step: i, Operation: "CLICK", Action: "Next", Frame: true})
		_ = os.WriteFile(filepath.Join(store.framesDir(r.ID), strings.TrimSpace(string(rune('0'+i)))+".jpg"), []byte(big), 0o600)
	}
	tl.writeReplay(r)
	steps, _ := replayOf(t, tl.workspace, r.ID)
	if steps[0].Frame != "" || steps[0].Note == "" || steps[1].Frame == "" || steps[2].Frame == "" {
		t.Fatalf("frames kept: %v %v %v (%q)", steps[0].Frame != "", steps[1].Frame != "", steps[2].Frame != "", steps[0].Note)
	}
}
