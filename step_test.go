package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func node(n int) *int { return &n }

// testPage is a search form: one editable field (fill + its click), a
// submit button, a select with two options, then the page controls.
func testPage() *page {
	return &page{
		URL: "https://shop.example.com/search", Title: "Search", Text: "Find a book",
		Actions: []action{
			{ID: "e1", Kind: "fill", Node: node(10), Role: "searchbox", Label: "Search books", Value: ""},
			{ID: "e2", Kind: "click", Node: node(10), Role: "searchbox", Label: "Open Search books"},
			{ID: "e3", Kind: "click", Node: node(11), Role: "button", Label: "Search"},
			{ID: "e4", Kind: "select", Node: node(12), Role: "combobox", Label: "Sort → Price", Value: "price", CurrentValue: "Relevance"},
			{ID: "e5", Kind: "select", Node: node(12), Role: "combobox", Label: "Sort → Date", Value: "date", CurrentValue: "Relevance"},
			{ID: "scroll_down", Kind: "scroll", Label: "Scroll down", Delta: 560},
			{ID: "wait", Kind: "wait", Label: "Wait for the page to update"},
		},
		Fingerprint: "fp1",
	}
}

// choice builds a valid SystemOne choice answer for labels picking pick.
func choice(labels []string, pick string) map[string]any {
	probs := map[string]float64{}
	rest := 0.0
	if len(labels) > 1 {
		rest = 0.1 / float64(len(labels)-1)
	}
	for _, l := range labels {
		probs[l] = rest
	}
	probs[pick] = 0.9
	if len(labels) == 1 {
		probs[pick] = 1
	}
	return map[string]any{"type": "choice", "choice": pick, "confidence": 0.9, "probabilities": probs}
}

// The predicate reads a probability; a refusal pauses like a yes.
func TestReadIrreversible(t *testing.T) {
	if p, err := readIrreversible([]byte(`{"answers":{"irreversible":{"type":"noul","noul":0.42}}}`)); err != nil || p != 0.42 {
		t.Fatalf("p = %v %v", p, err)
	}
	if p, err := readIrreversible([]byte(`{"answers":{"irreversible":{"type":"refusal"}}}`)); err != nil || p != 1 {
		t.Fatalf("refusal = %v %v", p, err)
	}
	if _, err := readIrreversible([]byte(`{"answers":{}}`)); !errors.Is(err, errInvalidAnswer) {
		t.Fatalf("missing = %v", err)
	}
	body, _ := json.Marshal(irreversibleRequest("m", "pay the bill", action{Label: "Pay now", Role: "button"}, "Total €120 Pay now"))
	if !strings.Contains(string(body), `"type":"noul"`) || !strings.Contains(string(body), "Total €120") {
		t.Fatalf("predicate request = %s", body)
	}
}

// A run's site is the registrable domain; subdomains share it, other
// sites and look-alikes do not.
func TestSites(t *testing.T) {
	r := &runState{Sites: []string{siteOf("https://portal.example.com/expenses")}}
	if r.Sites[0] != "example.com" {
		t.Fatalf("site = %q", r.Sites[0])
	}
	for raw, want := range map[string]bool{
		"https://login.example.com/sso":   true,
		"https://example.com.evil.test/x": false,
		"https://other.example.org":       false,
		"about:blank":                     true,
		"http://127.0.0.1:8080/x":         false,
	} {
		if got := r.onSite(raw); got != want {
			t.Errorf("onSite(%s) = %v, want %v", raw, got, want)
		}
	}
}
