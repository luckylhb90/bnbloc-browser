package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/luckylhb90/bnbloc/component"
)

// fakeDoc is one state of a fake page.
type fakeDoc struct {
	url, title, text string
	actions          []action
	// key is the page's semantic state: the marker, page key and guards
	// derive from it, so changing it is "the page changed".
	key string
}

func (d *fakeDoc) snapshot() map[string]any {
	acts := make([]action, 0, len(d.actions)+1)
	guards := map[string]any{}
	for i, a := range d.actions {
		a.ID = "e" + strconv.Itoa(i+1)
		acts = append(acts, a)
		if a.Node != nil {
			guards[strconv.Itoa(*a.Node)] = []any{*a.Node, a.Label, d.key}
		}
	}
	acts = append(acts, action{ID: "wait", Kind: "wait", Label: "Wait for the page to update"})
	return map[string]any{
		"url": d.url, "title": d.title, "text": d.text, "actions": acts,
		"marker": []any{d.url, d.text, d.key}, "page_key": []any{d.url, d.key}, "guards": guards,
		"scroll": map[string]any{"y": 0, "height": 700},
	}
}

// fakePage answers the executor's CDP calls like a page would: the
// snapshot, freshness reads, hit-tested targets and inputs, which move it
// between documents (onClick, onType). It checks what the real page
// checks: a target the page no longer shows is refused (null).
type fakePage struct {
	mu      sync.Mutex
	doc     *fakeDoc
	onClick func(p *fakePage, node int)
	onType  func(p *fakePage, node int, text string)
	// beforeTarget runs before the target resolves (a page that moves
	// between the observation and the input).
	beforeTarget func(p *fakePage)
	clicks       []int
	typed        []string
	files        []string
	lastNode     int
}

func (p *fakePage) set(d *fakeDoc) { p.doc = d }

func (p *fakePage) call(_ context.Context, method string, params any, out any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, _ := json.Marshal(params)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	reply := func(v any) error {
		if out == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
	value := func(v any) error { return reply(map[string]any{"result": map[string]any{"value": v}}) }
	switch method {
	case "Runtime.evaluate":
		expr, _ := m["expression"].(string)
		switch {
		case expr == snapshotJS:
			return value(p.doc.snapshot())
		case expr == markerJS:
			return value(p.doc.snapshot()["marker"])
		case expr == "document.readyState":
			return value("complete")
		case strings.HasPrefix(expr, "({W: innerWidth"):
			return value(map[string]any{"W": 1000, "H": 500, "DPR": 2, "Title": p.doc.title, "URL": "https://portal.example.com/"})
		case strings.Contains(expr, "c.pageKey(),c.guard("):
			n := nodeIn(expr)
			s := p.doc.snapshot()
			return value([]any{s["page_key"], s["guards"].(map[string]any)[strconv.Itoa(n)]})
		case strings.Contains(expr, "return c ? c.guard(c.nodes.get("):
			n := nodeIn(expr)
			return value([]any{n, "scope", "form text of " + p.doc.title})
		case strings.HasPrefix(expr, "("+targetJS+")("):
			if p.beforeTarget != nil {
				f := p.beforeTarget
				p.beforeTarget = nil
				f(p)
			}
			var a struct {
				Node  int    `json:"node"`
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			arg := strings.TrimSuffix(strings.TrimPrefix(expr, "("+targetJS+")("), ")")
			_ = json.Unmarshal([]byte(arg), &a)
			if !p.shows(a.Node) {
				return value(nil)
			}
			if a.Kind == "select" {
				p.clicks = append(p.clicks, a.Node)
			}
			return value(map[string]any{"x": a.Node, "y": 1})
		case strings.HasPrefix(expr, "("+settleJS+")("):
			return value(nil)
		case strings.HasPrefix(expr, "window.__bnblocBrowse?.nodes.get("):
			return reply(map[string]any{"result": map[string]any{"objectId": "obj-" + strconv.Itoa(nodeIn(expr))}})
		}
		return fmt.Errorf("fake page: unexpected expression %.60q", expr)
	case "Input.dispatchMouseEvent":
		switch m["type"] {
		case "mousePressed":
			p.lastNode = int(m["x"].(float64))
		case "mouseReleased":
			n := int(m["x"].(float64))
			p.clicks = append(p.clicks, n)
			if p.onClick != nil {
				p.onClick(p, n)
			}
		}
		return nil
	case "Input.dispatchKeyEvent":
		return nil
	case "Input.insertText":
		text, _ := m["text"].(string)
		p.typed = append(p.typed, text)
		if p.onType != nil {
			p.onType(p, p.lastNode, text)
		}
		return nil
	case "DOM.setFileInputFiles":
		for _, f := range m["files"].([]any) {
			p.files = append(p.files, f.(string))
		}
		return nil
	case "Page.navigate":
		return nil
	case "Page.captureScreenshot":
		// The picture follows the page: a new document, a new frame.
		return reply(map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("shot of " + p.doc.title))})
	}
	return fmt.Errorf("fake page: unexpected method %s", method)
}

// shows reports whether the current document offers node.
func (p *fakePage) shows(n int) bool {
	for _, a := range p.doc.actions {
		if a.Node != nil && *a.Node == n {
			return true
		}
	}
	return false
}

// nodeIn reads the node number from "…nodes.get(N)…".
func nodeIn(expr string) int {
	i := strings.Index(expr, "nodes.get(")
	if i < 0 {
		return 0
	}
	rest := expr[i+len("nodes.get("):]
	j := strings.IndexByte(rest, ')')
	n, _ := strconv.Atoi(rest[:j])
	return n
}

// fakeTab is a run's tab on a fake page.
type fakeTab struct {
	*fakePage
	id string
}

func (t *fakeTab) target() string             { return t.id }
func (t *fakeTab) detach(ctx context.Context) {}

// fakeHost holds tabs by id (closing one makes attach answer errTabGone).
type fakeHost struct {
	mu     sync.Mutex
	pages  map[string]*fakePage
	next   *fakePage // the page the next opened tab shows
	opened int
	dl     map[string][]string
}

func (h *fakeHost) open(context.Context) (tabHandle, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opened++
	id := "tab-" + strconv.Itoa(h.opened)
	if h.pages == nil {
		h.pages = map[string]*fakePage{}
	}
	h.pages[id] = h.next
	return &fakeTab{fakePage: h.next, id: id}, nil
}

func (h *fakeHost) attach(_ context.Context, id string) (tabHandle, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pages[id]
	if !ok {
		return nil, errTabGone
	}
	return &fakeTab{fakePage: p, id: id}, nil
}

func (h *fakeHost) finishedDownloads(tabID, dir string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.dl[tabID]
	delete(h.dl, tabID)
	return out
}

// decReq is one decision request as the fake model reads it.
type decReq struct {
	Questions map[string]struct {
		Type     string          `json:"type"`
		Criteria json.RawMessage `json:"criteria"`
	} `json:"questions"`
	State map[string]any `json:"state"`
}

func (q decReq) labels(head string) []string {
	var m map[string]any
	_ = json.Unmarshal(q.Questions[head].Criteria, &m)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// targetFor finds the index of the head's candidate whose element label
// contains label.
func (q decReq) targetFor(head, label string) string {
	var m map[string]map[string]any
	_ = json.Unmarshal(q.Questions[head].Criteria, &m)
	for idx, c := range m {
		if el, _ := c["element"].(string); strings.Contains(el, label) {
			return idx
		}
	}
	return ""
}

// fakeModel is the decision model: script answers each step request with
// an operation and, for a target operation, a label to find; predicate
// answers the irreversibility question.
type fakeModel struct {
	mu        sync.Mutex
	script    func(q decReq) (op, label string)
	predicate float64
	steps     int
	asked     int
	off       bool
}

func (m *fakeModel) systemOne(_ context.Context, body any) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.off {
		return nil, errDecisionsOff
	}
	raw, _ := json.Marshal(body)
	var q decReq
	_ = json.Unmarshal(raw, &q)
	if _, ok := q.Questions["irreversible"]; ok {
		m.asked++
		return json.Marshal(map[string]any{"answers": map[string]any{"irreversible": map[string]any{"type": "noul", "noul": m.predicate}}})
	}
	m.steps++
	op, label := m.script(q)
	answers := map[string]any{"operation": choice(q.labels("operation"), op)}
	head := strings.ToLower(op) + "_target"
	if _, ok := q.Questions[head]; ok {
		answers[head] = choice(q.labels(head), q.targetFor(head, label))
	}
	return json.Marshal(map[string]any{"answers": answers, "usage": map[string]any{"input_tokens": 10}})
}

// fakeRelay is the bridge's room relay: it refuses a call without the
// session's bearer (as the real relay does), answers the bound turn, and
// sends systemone to the fake model.
type fakeRelay struct {
	mu    sync.Mutex
	sess  component.RelaySession
	model *fakeModel
	// live answers POST /v1/live (nil: 404, live views off).
	live func(liveFramePost) component.LiveFrameReply
}

func (f *fakeRelay) bind(s component.RelaySession) { f.mu.Lock(); f.sess = s; f.mu.Unlock() }

func (f *fakeRelay) start(t *testing.T) *relay {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer relay-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/relay"+component.RelaySessionPath:
			f.mu.Lock()
			s := f.sess
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPost && r.URL.Path == "/relay/v1/live" && f.live != nil:
			var post liveFramePost
			_ = json.NewDecoder(r.Body).Decode(&post)
			_ = json.NewEncoder(w).Encode(f.live(post))
		case r.Method == http.MethodPost && r.URL.Path == "/relay/v1/systemone":
			var body any
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, err := f.model.systemOne(r.Context(), body)
			if err != nil {
				http.Error(w, `{"error":"decisions are not available here"}`, http.StatusConflict)
				return
			}
			_, _ = w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &relay{base: srv.URL + "/relay", token: "relay-token"}
}
