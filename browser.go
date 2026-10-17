package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"time"
)

// Observation and execution on one tab: a port of jev-ultrafast's
// browser.py (NOTICE). One in-page snapshot (snapshot.js) reads visible
// controls with code-owned node identities, the visible text, a guard per
// node and a semantic marker; the executor re-checks freshness, visibility,
// enabled state and hit-testing immediately before every input, and never
// acts on a target the page has changed under.

//go:embed snapshot.js
var snapshotJS string

// markerJS evaluates only the snapshot's semantic marker (freshness).
var markerJS = "(() => { const state=" + snapshotJS + "; return state?.marker ?? null; })()"

// page is one observation.
type page struct {
	URL            string            `json:"url"`
	Title          string            `json:"title"`
	W              int               `json:"w"`
	H              int               `json:"h"`
	Text           string            `json:"text"`
	Scroll         pageScroll        `json:"scroll"`
	Actions        []action          `json:"actions"`
	Marker         json.RawMessage   `json:"marker"`
	PageKey        json.RawMessage   `json:"page_key"`
	Guards         map[string]any    `json:"guards"`
	OmittedActions int               `json:"omitted_actions"`
	Fingerprint    string            `json:"-"`
	guardsRaw      map[string]string // node → its guard's JSON
}

type pageScroll struct {
	Y      float64 `json:"y"`
	Height float64 `json:"height"`
}

// action is one observed control or page control. Kinds: click, fill,
// select, upload, secret (observed controls, Node set), scroll, wait.
type action struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Node         *int    `json:"node,omitempty"`
	Role         string  `json:"role,omitempty"`
	Label        string  `json:"label"`
	Value        string  `json:"value,omitempty"`
	CurrentValue string  `json:"current_value,omitempty"`
	Checked      string  `json:"checked,omitempty"`
	Selected     string  `json:"selected,omitempty"`
	Expanded     string  `json:"expanded,omitempty"`
	Href         string  `json:"href,omitempty"`
	Delta        float64 `json:"delta,omitempty"`
	Multiple     bool    `json:"multiple,omitempty"`
	Accept       string  `json:"accept,omitempty"`
	Filled       bool    `json:"filled,omitempty"`
}

// errStale: the page changed since the observation a step was decided on.
var errStale = errors.New("the page changed since it was observed")

// errUnconfirmed: an input may have reached the page but could not be
// confirmed (an interrupted native select): never retried blindly.
var errUnconfirmed = errors.New("the dropdown change could not be confirmed")

// browserPage is the executor's view of one tab: the CDP calls it needs.
// The tab implements it; tests substitute a fake.
type browserPage interface {
	call(ctx context.Context, method string, params any, out any) error
}

// evaluate runs an expression in the tab and decodes its value; a thrown
// exception (a navigation mid-read) is errStale.
func evaluate(ctx context.Context, p browserPage, expr string, out any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &res); err != nil {
		return err
	}
	if len(res.ExceptionDetails) > 0 && string(res.ExceptionDetails) != "null" {
		return errStale
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, out)
}

// observe reads the page once (retrying a navigation in progress briefly).
func observe(ctx context.Context, p browserPage) (*page, error) {
	var last error
	for attempt := 0; attempt < 10; attempt++ {
		var pg *page
		err := evaluate(ctx, p, snapshotJS, &pg)
		if err == nil && pg != nil {
			pg.Fingerprint = fingerprint(pg)
			pg.guardsRaw = map[string]string{}
			for node, g := range pg.Guards {
				raw, _ := json.Marshal(g)
				pg.guardsRaw[node] = string(raw)
			}
			return pg, nil
		}
		if err != nil && !errors.Is(err, errStale) {
			return nil, err
		}
		last = errStale
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("the page did not settle: %w", last)
}

// fingerprint is the observation's content identity (url, text, actions,
// scroll): what "the page changed" means to stall detection.
func fingerprint(pg *page) string {
	raw, _ := json.Marshal(map[string]any{"url": pg.URL, "text": pg.Text, "actions": pg.Actions, "scroll": pg.Scroll})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// fresh reports whether the observation still describes the page. A click
// or select compares the page key and its target's guard (unrelated
// content may change); anything else compares the full semantic marker.
func fresh(ctx context.Context, p browserPage, pg *page, a *action) (bool, error) {
	if a != nil && (a.Kind == "click" || a.Kind == "select") && a.Node != nil {
		var cur []json.RawMessage
		expr := "(() => { const c=window.__bnblocBrowse; return c ? [c.pageKey(),c.guard(c.nodes.get(" + strconv.Itoa(*a.Node) + "))] : null; })()"
		if err := evaluate(ctx, p, expr, &cur); err != nil {
			if errors.Is(err, errStale) {
				return false, nil
			}
			return false, err
		}
		if len(cur) != 2 {
			return false, nil
		}
		return jsonEqual(cur[0], pg.PageKey) && jsonEqual(cur[1], json.RawMessage(pg.guardsRaw[strconv.Itoa(*a.Node)])), nil
	}
	var marker json.RawMessage
	if err := evaluate(ctx, p, markerJS, &marker); err != nil {
		if errors.Is(err, errStale) {
			return false, nil
		}
		return false, err
	}
	return jsonEqual(marker, pg.Marker), nil
}

// jsonEqual compares two JSON values by meaning.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ra, _ := json.Marshal(x)
	rb, _ := json.Marshal(y)
	return string(ra) == string(rb)
}

// targetJS resolves an observed node for input: connected, enabled,
// visible, inside the viewport and not covered at its center (hit-tested);
// a select is set here to the observed option. It answers the center, or
// null.
const targetJS = `(action => {
  const e=window.__bnblocBrowse?.nodes.get(action.node);
  if (!e?.isConnected || e.matches(':disabled') || e.closest('[aria-disabled="true"],[inert]') ||
      !e.checkVisibility({checkOpacity:true,checkVisibilityCSS:true})) return null;
  if (action.kind==='fill' && (e.readOnly || e.getAttribute('aria-readonly')==='true')) return null;
  const r=e.getBoundingClientRect(), x=r.x+r.width/2, y=r.y+r.height/2;
  if (!r.width || !r.height || x<0 || y<0 || x>=innerWidth || y>=innerHeight) return null;
  if (!e.contains(document.elementFromPoint(x,y))) return null;
  if (action.kind==='select') {
    if (e.tagName!=='SELECT' || ![...e.options].some(o=>o.value===action.value &&
        !o.disabled && !o.closest('optgroup[disabled]'))) return null;
    e.value=action.value;
    e.dispatchEvent(new Event('input',{bubbles:true}));
    e.dispatchEvent(new Event('change',{bubbles:true}));
  }
  return {x,y};
})`

// settleJS waits after an input: two animation frames, or for an editable
// combobox its visible options (autocomplete), capped.
const settleJS = `(action => new Promise(resolve => {
  const field=window.__bnblocBrowse?.nodes.get(action.node);
  const autocomplete=action.kind==='fill' && field?.getAttribute('role')==='combobox';
  let frames=0, stopped=false;
  const finish=()=>{stopped=true;resolve()};
  setTimeout(finish,autocomplete ? 200 : 50);
  const ready=()=>{
    if (stopped) return;
    const ids=(field?.getAttribute('aria-controls')||field?.getAttribute('aria-owns')||'')
      .split(/\s+/).filter(Boolean);
    const roots=ids.length ? ids.map(id=>document.getElementById(id)).filter(Boolean) : [document];
    const options=roots.flatMap(root=>[...root.querySelectorAll('[role="option"]')]);
    if (++frames>=2 && (!autocomplete || options.some(e=>{
      const r=e.getBoundingClientRect();
      return r.width && r.height && r.bottom>0 && r.top<innerHeight &&
        e.checkVisibility({checkOpacity:true,checkVisibilityCSS:true});
    }))) finish();
    else requestAnimationFrame(ready);
  };
  requestAnimationFrame(ready);
}))`

// act executes one observed action with text (for fill). It re-checks
// freshness first and resolves the target's geometry immediately before
// input; errStale when the page changed, errUnconfirmed when a native
// select's change may have fired without confirmation.
func act(ctx context.Context, p browserPage, pg *page, a action, text string) error {
	ok, err := fresh(ctx, p, pg, &a)
	if err != nil {
		return err
	}
	if !ok {
		return errStale
	}
	switch a.Kind {
	case "wait":
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		return nil
	case "scroll":
		return p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": 10, "y": 10, "deltaX": 0, "deltaY": a.Delta}, nil)
	case "click", "fill", "select":
	default:
		return fmt.Errorf("%s is not an input the executor runs", a.Kind)
	}
	if a.Node == nil {
		return errors.New("the action names no observed node")
	}
	raw, _ := json.Marshal(map[string]any{"node": *a.Node, "kind": a.Kind, "value": a.Value})
	var target *struct{ X, Y float64 }
	if err := evaluate(ctx, p, "("+targetJS+")("+string(raw)+")", &target); err != nil {
		if a.Kind == "select" {
			return errUnconfirmed
		}
		if errors.Is(err, errStale) {
			return errStale
		}
		return err
	}
	if target == nil {
		if a.Kind == "select" {
			return errUnconfirmed
		}
		return errStale
	}
	if a.Kind != "select" {
		for _, ev := range []string{"mousePressed", "mouseReleased"} {
			if err := p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": ev, "x": target.X, "y": target.Y, "button": "left", "clickCount": 1}, nil); err != nil {
				return err
			}
		}
		if a.Kind == "fill" {
			mod := 2 // Ctrl
			if runtime.GOOS == "darwin" {
				mod = 4 // Meta
			}
			_ = p.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "a", "code": "KeyA", "modifiers": mod, "commands": []string{"selectAll"}}, nil)
			_ = p.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "a", "code": "KeyA", "modifiers": mod}, nil)
			if err := p.call(ctx, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
				return err
			}
		}
	}
	// Settle before the next observation (read-only; a navigation ends it).
	_ = evaluate(ctx, p, "("+settleJS+")("+string(raw)+")", nil)
	return nil
}

// navigate opens url in the tab and waits for the document to load.
func navigate(ctx context.Context, p browserPage, url string) error {
	if err := p.call(ctx, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		return fmt.Errorf("open %s: %w", url, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := evaluate(ctx, p, "document.readyState", &state); err == nil && state == "complete" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil // a slow page is observed as it is; the loop waits on it
}

// screenshot captures the tab as JPEG (base64).
func screenshot(ctx context.Context, p browserPage) (string, error) {
	var shot struct {
		Data string `json:"data"`
	}
	if err := p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 72}, &shot); err != nil {
		return "", err
	}
	return shot.Data, nil
}
