package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"runtime"
	"strconv"
	"strings"
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
	// LoadedMs is how long ago the page finished loading, LeftMs how long
	// ago it began a navigation to another document (nil: not yet, or not
	// known). Neither is part of what it shows.
	LoadedMs *float64 `json:"loaded_ms"`
	LeftMs   *float64 `json:"left_ms"`
}

// justMoved is how long a page that finished loading may still send itself
// on by script (a session check sending it to the identity provider; QA
// 2026-10-20), and how long one that began leaving may still show while
// the next is on its way (Chrome holds a read once the request is out; a
// navigation that ended in a download or nothing leaves the page as it is).
const justMoved = time.Second

// onItsWay reports a page that may be about to become another, which a
// step waits out before taking it for where it came to: one that just
// loaded, or just began leaving.
func onItsWay(pg *page) bool {
	recent := func(ms *float64) bool { return ms != nil && *ms < float64(justMoved.Milliseconds()) }
	return recent(pg.LoadedMs) || recent(pg.LeftMs)
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
	// Offscreen: the control is outside the viewport (an action scrolls it
	// into view first).
	Offscreen bool `json:"offscreen,omitempty"`
}

// errStale: the page changed since the observation a step was decided on.
var errStale = errors.New("the page changed since it was observed")

// errUnconfirmed: an input may have reached the page but could not be
// confirmed (an interrupted native select): never retried blindly.
var errUnconfirmed = errors.New("the dropdown change could not be confirmed")

// blockedError: the control is there but cannot take input now (covered,
// disabled, hidden); nothing was done.
type blockedError struct{ why string }

func (e blockedError) Error() string { return e.why }

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
// visible, and a point of it (or of its label) inside the viewport that is
// not covered (hit-tested). A control under a bar fixed to the window's
// edge is scrolled to the middle first, then any uncovered point of it is
// used (QA 2026-10-19: a quiz answer in a small window sat under the
// page's bottom bar, and every click was refused). A select is set here to
// the observed option. It answers the point, {why} when the control is
// there but cannot take input now, or null when it is gone. The point is
// kept on the page for the step's picture (actedJS).
const targetJS = `(action => {
  const e=window.__bnblocBrowse?.nodes.get(action.node);
  if (!e?.isConnected) return null;
  if (e.matches(':disabled') || e.closest('[aria-disabled="true"],[inert]')) return {why:'it is disabled'};
  if (!e.checkVisibility({checkOpacity:true,checkVisibilityCSS:true})) return {why:'it is hidden'};
  if (action.kind==='fill' && (e.readOnly || e.getAttribute('aria-readonly')==='true')) return {why:'it is read-only'};
  const targets=[e,...[...(e.labels||[])].filter(l=>l.checkVisibility())];
  const inside=(x,y)=>x>=0 && y>=0 && x<innerWidth && y<innerHeight;
  const point=()=>{
    for (const t of targets) {
      const r=t.getBoundingClientRect();
      if (!r.width || !r.height) continue;
      for (const [fx,fy] of [[.5,.5],[.2,.5],[.8,.5],[.5,.2],[.5,.8],[.2,.2],[.8,.2],[.2,.8],[.8,.8]]) {
        const x=r.x+r.width*fx, y=r.y+r.height*fy, hit=inside(x,y) && document.elementFromPoint(x,y);
        if (hit && t.contains(hit)) return {x,y};
      }
    }
    return null;
  };
  let r=e.getBoundingClientRect(), at=inside(r.x+r.width/2,r.y+r.height/2) && point();
  if (!at) {
    e.scrollIntoView({block:'center',inline:'center',behavior:'instant'});
    at=point();
  }
  if (!at) {
    r=e.getBoundingClientRect();
    const x=r.x+r.width/2, y=r.y+r.height/2, hit=inside(x,y) && document.elementFromPoint(x,y);
    if (!hit) return {why:'it does not fit in the window'};
    const what=(hit.getAttribute('aria-label')||hit.innerText||hit.getAttribute('title')||'').trim().slice(0,60);
    return {why:'it is covered by '+(what ? JSON.stringify(what)+' ' : '')+'('+hit.tagName.toLowerCase()+')'};
  }
  if (action.kind==='select') {
    if (e.tagName!=='SELECT' || ![...e.options].some(o=>o.value===action.value &&
        !o.disabled && !o.closest('optgroup[disabled]'))) return null;
    e.value=action.value;
    e.dispatchEvent(new Event('input',{bubbles:true}));
    e.dispatchEvent(new Event('change',{bubbles:true}));
  }
  window.__bnblocBrowse.acted={node:action.node,x:at.x,y:at.y};
  return at;
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
	var target *struct {
		X, Y float64
		Why  string
	}
	if err := evaluate(ctx, p, "("+targetJS+")("+string(raw)+")", &target); err != nil {
		if a.Kind == "select" {
			return errUnconfirmed
		}
		if errors.Is(err, errStale) {
			return errStale
		}
		return err
	}
	if target != nil && target.Why != "" {
		return blockedError{target.Why}
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

// autoPostJS submits a hand-off page's form: a page that shows nothing and
// holds one form of hidden fields only (a SAML or OIDC POST binding, whose
// onload submits it). It answers whether it did.
const autoPostJS = `(() => {
  const b=document.body;
  if (!b || b.innerText.trim() || document.readyState!=='complete') return false;
  const forms=[...document.forms].filter(f=>f.elements.length && [...f.elements].every(e=>e.type==='hidden'));
  if (forms.length!==1) return false;
  HTMLFormElement.prototype.submit.call(forms[0]);
  return true;
})()`

// autoPostWait is how long a hand-off page gets to submit itself first.
const autoPostWait = 1500 * time.Millisecond

// autoPost finishes a hand-off page the browser left standing (seen on a
// background window: the onload submit never ran, and the page stays
// blank): when pg shows nothing and is still there after autoPostWait, its
// form is submitted, which is all the page itself does, and the next page
// loads. It answers whether it submitted.
func autoPost(ctx context.Context, p browserPage, pg *page) (bool, error) {
	if strings.TrimSpace(pg.Text) != "" || len(pg.Actions) > 0 && !onlyControls(pg) {
		return false, nil
	}
	if err := sleepCtx(ctx, autoPostWait); err != nil {
		return false, err
	}
	var now string
	if evaluate(ctx, p, "location.href", &now) != nil || now != pg.URL {
		return false, nil // it went on by itself
	}
	var posted bool
	if evaluate(ctx, p, autoPostJS, &posted) != nil || !posted {
		return false, nil
	}
	waitLoaded(ctx, p, pg.URL)
	return true, nil
}

// onlyControls: the page offers nothing but its scroll and wait controls.
func onlyControls(pg *page) bool {
	for _, a := range pg.Actions {
		if a.Node != nil {
			return false
		}
	}
	return true
}

// waitLoaded waits until the tab has left from and the next document
// loaded (bounded: a slow page is observed as it is).
func waitLoaded(ctx context.Context, p browserPage, from string) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var st struct {
			URL   string `json:"url"`
			Ready string `json:"ready"`
		}
		if evaluate(ctx, p, "({url: location.href, ready: document.readyState})", &st) == nil && st.URL != from && st.Ready == "complete" {
			return
		}
		if sleepCtx(ctx, 50*time.Millisecond) != nil {
			return
		}
	}
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

// screenshot captures the tab's viewport as JPEG (base64) at one image
// pixel per CSS pixel, at most maxShotWidth wide: a high-density screen
// would otherwise double each side, and every pixel costs the agent tokens.
func screenshot(ctx context.Context, p browserPage) (string, error) {
	shot, _, err := capture(ctx, p, 60)
	return shot, err
}

// viewport is the tab's page size in CSS pixels, its title and address.
type viewport struct {
	W, H, DPR  float64
	Title, URL string
}

// capture is screenshot at a JPEG quality, with the viewport it shows.
// It works in a window behind others, where Chrome's screencast sends
// nothing. Chrome captures at the screen's pixels and the picture is
// scaled here: a capture Chrome scales (a clip with a scale) resizes the
// page for its duration, and two at once (the room's view and a step's
// picture) left a page at the other's scaled size, halving it each time
// (QA 2026-10-19: a 1200-wide window's page at 512, then 128; reproduced
// with two sessions capturing at once).
func capture(ctx context.Context, p browserPage, quality int) (string, viewport, error) {
	shot, view, err := grab(ctx, p, quality)
	if err != nil {
		return "", view, err
	}
	w, h := shotSize(view, maxShotWidth)
	if w <= 0 || h <= 0 {
		return shot, view, nil
	}
	scaled, err := scaleJPEG(shot, w, h, quality)
	if err != nil {
		return shot, view, nil
	}
	return scaled, view, nil
}

// grab is Chrome's capture of the tab at the screen's pixels (base64 JPEG
// at quality) and the viewport it shows.
func grab(ctx context.Context, p browserPage, quality int) (string, viewport, error) {
	var view viewport
	_ = evaluate(ctx, p, "({W: innerWidth, H: innerHeight, DPR: devicePixelRatio, Title: document.title, URL: location.href})", &view)
	var shot struct {
		Data string `json:"data"`
	}
	if err := p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": quality}, &shot); err != nil {
		return "", view, err
	}
	return shot.Data, view, nil
}

// captureImage is capture as a picture to draw on, at most widest wide (a
// replay's frame, encoded once it is drawn on): taken at a high quality,
// so the one encoding that counts is the caller's.
func captureImage(ctx context.Context, p browserPage, widest int) (*image.RGBA, viewport, error) {
	shot, view, err := grab(ctx, p, 90)
	if err != nil {
		return nil, view, err
	}
	raw, err := base64.StdEncoding.DecodeString(shot)
	if err != nil {
		return nil, view, err
	}
	src, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, view, err
	}
	sb := src.Bounds()
	w, h := shotSize(view, widest)
	if w <= 0 || h <= 0 {
		w, h = sb.Dx(), sb.Dy()
	}
	if sb.Dx() == w && sb.Dy() == h {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(img, img.Bounds(), src, sb.Min, draw.Src)
		return img, view, nil
	}
	return scaleImage(src, w, h), view, nil
}

// boxAverage is the average colour of src's pixels in [x0,x1)×[y0,y1).
// A JPEG decodes to YCbCr, averaged in its own planes (an interface call
// a pixel made a full-screen picture take 150 ms).
func boxAverage(src image.Image, x0, y0, x1, y1 int) color.RGBA {
	if yc, ok := src.(*image.YCbCr); ok {
		var ys, cbs, crs, n int
		for sy := y0; sy < y1; sy++ {
			for sx := x0; sx < x1; sx++ {
				ys += int(yc.Y[yc.YOffset(sx, sy)])
				ci := yc.COffset(sx, sy)
				cbs, crs, n = cbs+int(yc.Cb[ci]), crs+int(yc.Cr[ci]), n+1
			}
		}
		r, g, b := color.YCbCrToRGB(uint8(ys/n), uint8(cbs/n), uint8(crs/n))
		return color.RGBA{r, g, b, 255}
	}
	var r, g, b, n uint32
	for sy := y0; sy < y1; sy++ {
		for sx := x0; sx < x1; sx++ {
			cr, cg, cb, _ := src.At(sx, sy).RGBA()
			r, g, b, n = r+cr, g+cg, b+cb, n+1
		}
	}
	return color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(b / n >> 8), 255}
}

// scaleJPEG scales a base64 JPEG to w×h by averaging the pixels each
// target pixel covers; one already that size is answered as it is.
func scaleJPEG(b64 string, w, h, quality int) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	src, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	if sb := src.Bounds(); sb.Dx() == w && sb.Dy() == h {
		return b64, nil
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, scaleImage(src, w, h), &jpeg.Options{Quality: quality}); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// scaleImage is src scaled to w×h, each pixel the average of the pixels it
// covers.
func scaleImage(src image.Image, w, h int) *image.RGBA {
	sb := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := sb.Min.Y+y*sb.Dy()/h, sb.Min.Y+(y+1)*sb.Dy()/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0, x1 := sb.Min.X+x*sb.Dx()/w, sb.Min.X+(x+1)*sb.Dx()/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			dst.SetRGBA(x, y, boxAverage(src, x0, y0, x1, y1))
		}
	}
	return dst
}

// maxShotWidth bounds a screenshot's width in image pixels.
const maxShotWidth = 1024
