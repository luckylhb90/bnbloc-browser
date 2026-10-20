package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// A step whose page was a sign-in says why it has no picture (QA
// 2026-10-20: "No picture for this step" on two opens that landed on the
// identity provider); one without a picture for no such reason says
// nothing more.
func TestReplaySaysASignInIsNotPictured(t *testing.T) {
	tl := &tools{runner: &runner{store: runStore{dir: t.TempDir()}}, workspace: t.TempDir()}
	r := &runState{ID: "brun_signin", History: []historyRecord{
		{Step: 1, Operation: "OPEN", Action: "https://portal.example.com/task", SignIn: true},
		{Step: 2, Operation: "PERSON", Action: "A person signs in here", Kind: "person", SignIn: true},
		{Step: 3, Operation: "CLICK", Action: "Start"},
	}}
	tl.writeReplay(r)
	steps, page := replayOf(t, tl.workspace, r.ID)
	if steps[0].Note != replaySignIn || steps[1].Note != replaySignIn || steps[2].Note != "" || steps[2].Frame != "" {
		t.Fatalf("steps = %+v", steps)
	}
	if !strings.Contains(page, "s.note||'No picture for this step'") {
		t.Fatal("the replay shows no note for a step without one")
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

// The sweep removes what gone runs left, once old; a live run's files and
// fresh leftovers stay.
func TestSweepRemovesWhatGoneRunsLeft(t *testing.T) {
	s := runStore{dir: t.TempDir()}
	write := func(name, content string) { _ = os.WriteFile(filepath.Join(s.dir, name), []byte(content), 0o600) }
	_ = s.save(&runState{ID: "brun_alive"})
	write("brun_alive.json.lock", "")
	write("brun_gone.json.lock", "")
	write("brun_gone.json.keep.log", "log")
	write("keeper-job_1", "brun_gone")
	write("keeper-job_2", "brun_alive")
	_ = os.MkdirAll(s.framesDir("brun_gone"), 0o700)
	write("brun_fresh.json.lock", "")
	old := time.Now().Add(-2 * time.Hour)
	for _, n := range []string{"brun_alive.json", "brun_alive.json.lock", "brun_gone.json.lock", "brun_gone.json.keep.log", "keeper-job_1", "keeper-job_2", "brun_gone.frames"} {
		_ = os.Chtimes(filepath.Join(s.dir, n), old, old)
	}
	s.sweep(time.Now())
	left, _ := os.ReadDir(s.dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "brun_alive.json,brun_alive.json.lock,brun_fresh.json.lock,keeper-job_2" {
		t.Fatalf("left = %s", got)
	}
}

// Closing a run with a procedure writes its steps as a skill pack draft
// (a repeated step once, the sign-in the person's); a bad name keeps the
// run open.
func TestCloseSavesAProcedureDraft(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	h.tools.workspace = t.TempDir()
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run := head["run"].(string)
	h.call("browser_act", map[string]any{"run": run, "action": "type", "element": 1, "text": "120.50"})
	h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 2})
	h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 2})
	raw, _ := json.Marshal(map[string]any{"name": "browser_close", "arguments": map[string]any{"run": run, "procedure": "File Expense", "purpose": "File an expense"}})
	if _, rerr := h.tools.dispatch(context.Background(), "tools/call", raw); rerr == nil || !strings.Contains(rerr.Message, "lowercase") {
		t.Fatalf("a bad name = %v", rerr)
	}
	if _, err := h.tools.runner.store.load(run); err != nil {
		t.Fatal("a bad name closed the run")
	}
	head, _ = h.call("browser_close", map[string]any{"run": run, "procedure": "file-expense", "purpose": "File an expense: amount and preview"})
	if head["procedure"] != "drafts/file-expense" {
		t.Fatalf("close = %v", head)
	}
	skill, err := os.ReadFile(filepath.Join(h.tools.workspace, "drafts", "file-expense", "file-expense", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: file-expense\ndescription: \"File an expense: amount and preview\"\n---"
	if !strings.HasPrefix(string(skill), want) || !strings.Contains(string(skill), "1. Open https://portal.example.com/expenses/new\n2. Type \"120.50\" into \"Amount\"\n3. Click \"Preview\"\n") || strings.Contains(string(skill), "4. ") {
		t.Fatalf("SKILL.md:\n%s", skill)
	}
	var manifest map[string]any
	raw, _ = os.ReadFile(filepath.Join(h.tools.workspace, "drafts", "file-expense", "component.json"))
	if json.Unmarshal(raw, &manifest) != nil || manifest["kind"] != "bnbloc.skill.package/v1" {
		t.Fatalf("component.json:\n%s", raw)
	}
}
