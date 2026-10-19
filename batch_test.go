package main

import (
	"strings"
	"testing"
)

// quizPage is a question: two answers [1] [2] and Submit [3], which leads
// to the next question (another address) with Continue [4].
func quizPage() *fakePage {
	q1 := &fakeDoc{url: "https://learn.example.com/quiz/1", title: "Quiz", text: "Question 1 of 15\nPick one", key: "q1", actions: []action{
		{Kind: "click", Node: node(1), Role: "radio", Label: "Answer A", Checked: "false"},
		{Kind: "click", Node: node(2), Role: "radio", Label: "Answer B", Checked: "false"},
		{Kind: "click", Node: node(3), Role: "button", Label: "Submit"},
	}}
	q2 := &fakeDoc{url: "https://learn.example.com/quiz/2", title: "Quiz", text: "Correct\nQuestion 2 of 15", key: "q2", actions: []action{
		{Kind: "click", Node: node(4), Role: "button", Label: "Continue"},
	}}
	p := &fakePage{doc: q1}
	p.onClick = func(p *fakePage, n int) {
		switch n {
		case 1, 2:
			next := *p.doc
			next.key = p.doc.key + "+" + "picked"
			next.actions = append([]action(nil), p.doc.actions...)
			next.actions[n-1].Checked = "true"
			p.doc = &next
		case 3:
			p.doc = q2
		}
	}
	return p
}

// then does more actions in one call, each on the page read anew, and
// stops once the page is another: the agent reads that page first.
func TestThenStopsWhereThePageChanges(t *testing.T) {
	p := quizPage()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	head, _ := h.call("browser_open", map[string]any{"url": "https://learn.example.com/quiz/1"})
	run := head["run"].(string)
	head, text := h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 2, "then": []any{
		map[string]any{"action": "click", "element": 3},
		map[string]any{"action": "click", "element": 4},
	}})
	if countClicks(p, 2) != 1 || countClicks(p, 3) != 1 || countClicks(p, 4) != 0 {
		t.Fatalf("clicks = %v", p.clicks)
	}
	if note, _ := head["note"].(string); !strings.Contains(note, "did 2 of 3 actions; the page changed to another") {
		t.Fatalf("note = %v", head)
	}
	if !strings.Contains(text, "Question 2 of 15") || !strings.Contains(text, `[4] button "Continue"`) {
		t.Fatalf("the new page is not answered whole:\n%s", text)
	}
}

// An action then refuses ends the call there, saying which.
func TestThenStopsAtARefusal(t *testing.T) {
	p := quizPage()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	head, _ := h.call("browser_open", map[string]any{"url": "https://learn.example.com/quiz/1"})
	head, _ = h.call("browser_act", map[string]any{"run": head["run"], "action": "click", "element": 1, "then": []any{
		map[string]any{"action": "click", "element": 99},
		map[string]any{"action": "click", "element": 3},
	}})
	if note, _ := head["note"].(string); !strings.Contains(note, "did 1 of 3 actions; action 2: the page has no control [99]") || countClicks(p, 3) != 0 {
		t.Fatalf("note = %v (clicks %v)", head, p.clicks)
	}
}

// On the same page the agent is answered what changed, not the page
// again; nothing changed says so.
func TestSamePageAnswersOnlyTheChanges(t *testing.T) {
	p := quizPage()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	head, _ := h.call("browser_open", map[string]any{"url": "https://learn.example.com/quiz/1"})
	run := head["run"].(string)
	_, text := h.call("browser_act", map[string]any{"run": run, "action": "click", "element": 1})
	for _, want := range []string{"only what changed is shown", `[1] radio "Answer A" checked=true`, "(2 other controls as before.)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the changes lack %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `[3] button "Submit"`) || strings.Contains(text, "Pick one") {
		t.Fatalf("unchanged parts were answered again:\n%s", text)
	}
	if _, text = h.call("browser_act", map[string]any{"run": run, "action": "press", "key": "Escape"}); !strings.Contains(text, "Nothing on the page changed.") {
		t.Fatalf("an action that changed nothing:\n%s", text)
	}
	if _, text = h.call("browser_view", map[string]any{"run": run}); !strings.Contains(text, `[3] button "Submit"`) {
		t.Fatalf("browser_view is not the whole page:\n%s", text)
	}
}

// A page that changed mostly is answered whole.
func TestBigChangeIsAnsweredWhole(t *testing.T) {
	before := &page{URL: "https://x.example.com/", Text: "a\nb", PageKey: []byte(`[1]`)}
	now := &page{URL: "https://x.example.com/", Text: "c\nd\ne", PageKey: []byte(`[1]`)}
	if _, small := changesOf(before, now); small {
		t.Fatal("a page whose text is all new was answered as changes")
	}
}
