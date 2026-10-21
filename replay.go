package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A run's replay (PRD §2.8a): after each call the run's steps, each with
// the page after it, are written to browser-runs/<run>.html in the agent's
// workspace, which the bridge publishes as a room file like any other the
// agent made. A sign-in is never pictured. A step that acted on a control
// has it marked on its picture; one that looks as the picture before it
// does keeps none of its own and plays that one; and when the pictures do
// not all fit the file, the plain view steps' go first (chooseFrames). A
// run's GIF guide (gif.go) is made of the same pictures and is linked from
// the replay: the room runs the page sandboxed, with no downloads, and
// opens the room file a relative link names in its own tab, where the
// member downloads it.

const (
	// replayDir is the workspace directory replays are written to.
	replayDir = "browser-runs"
	// replayMaxBytes bounds a replay file. The room plays a page with a
	// script in its live preview (the web's core.app viewer) up to 5 MiB
	// and shows a larger one as source. Up to 2 MiB the bridge publishes
	// it inline; past that as a blob version (services/bridge/artifacts.go,
	// deliverableFor) under the room's blob cap (100 MiB by default), which
	// takes a core that reads workspace files byte-exact, as the GIF guide
	// does. So the live preview's limit is the one that binds.
	replayMaxBytes = 5 << 20
	// replayHeadroom is left under replayMaxBytes for what the pictures'
	// share is not measured on: the run's sites and the steps' notes.
	replayHeadroom = 256 << 10
	// replayFrameWidth bounds a frame's width (one pixel per CSS pixel
	// below it, so a phone run's are its 390), replayQuality is its JPEG
	// quality: a 1280-wide page measured 50 KB, one dense with text and
	// tables 125 KB, so forty pictures of the one fit and about thirty of
	// the other (TestFrameJPEGIsSmall, TestReplayRunOnChrome).
	replayFrameWidth = 1280
	replayQuality    = 70
	// sameLookBits is how many of two pictures' 256 look bits may differ,
	// and sameLookGray how far their brightness, for them to be the same
	// picture.
	sameLookBits = 6
	sameLookGray = 6
	// lookMargin is how much darker (of 255) a cell must be than the next
	// for its look bit: cells of one colour stay 0 through any encoding.
	lookMargin = 3
	// replaySignIn is what the replay shows for a step whose page was a
	// sign-in, never pictured.
	replaySignIn = "Sign-in page — not pictured"
)

// markColor is the mark drawn on an acted control.
var markColor = color.RGBA{255, 59, 48, 255}

// framesDir is where a run's frames are kept, beside its state.
func (s runStore) framesDir(id string) string { return filepath.Join(s.dir, id+".frames") }

// frameMark is where a step acted on its picture, in the picture's pixels:
// the control's box, or the point pressed (W and H 0).
type frameMark struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w,omitempty"`
	H int `json:"h,omitempty"`
}

// frameLook is a picture's perceptual hash: each bit says whether a cell
// of a 17×16 grid of its brightness is darker than the cell to its right
// (a difference hash, past lookMargin), and Gray is its mean brightness.
type frameLook struct {
	Bits [4]uint64 `json:"bits"`
	Gray uint8     `json:"gray"`
}

// keepFrame pictures the page after the run's last step as its frame,
// unless the page is a sign-in (marked so) or looks as the run's picture
// before it does (Same: no picture of its own).
func (t *tools) keepFrame(ctx context.Context, r *runState, tb tabHandle) {
	pg, err := observe(ctx, tb)
	if err != nil || len(r.History) == 0 {
		return
	}
	last := &r.History[len(r.History)-1]
	if signInPage(pg) {
		last.SignIn = true
		return
	}
	img, view, err := captureImage(ctx, tb, replayFrameWidth)
	if err != nil {
		return
	}
	cur := historyRecord{Page: pg.Fingerprint, Look: lookOf(img), Mark: actedMark(ctx, tb, last.Node, view, img.Bounds().Dx())}
	last.Frame, last.Same, last.Page, last.Look, last.Mark = false, 0, "", nil, nil
	if prev := lastFrame(r.History[:len(r.History)-1]); prev != nil && sameFrame(*prev, cur) {
		last.Same = prev.Step
		return
	}
	drawMark(img, cur.Mark)
	var out bytes.Buffer
	if jpeg.Encode(&out, img, &jpeg.Options{Quality: replayQuality}) != nil {
		return
	}
	dir := t.runner.store.framesDir(r.ID)
	if os.MkdirAll(dir, 0o700) != nil || os.WriteFile(filepath.Join(dir, strconv.Itoa(last.Step)+".jpg"), []byte(base64.StdEncoding.EncodeToString(out.Bytes())), 0o600) != nil {
		return
	}
	last.Frame, last.Page, last.Look, last.Mark = true, cur.Page, cur.Look, cur.Mark
}

// lastFrame is the last of hist's steps with a picture of its own.
func lastFrame(hist []historyRecord) *historyRecord {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Frame {
			return &hist[i]
		}
	}
	return nil
}

// sameFrame reports whether cur's picture would be prev's: marked alike,
// and of the same page (its fingerprint) or near-identical to look at.
func sameFrame(prev, cur historyRecord) bool {
	if (prev.Mark == nil) != (cur.Mark == nil) || prev.Mark != nil && *prev.Mark != *cur.Mark {
		return false
	}
	if prev.Page != "" && prev.Page == cur.Page {
		return true
	}
	return prev.Look != nil && cur.Look != nil && sameLook(*prev.Look, *cur.Look)
}

// sameLook reports two looks of the same picture.
func sameLook(a, b frameLook) bool {
	n := 0
	for i := range a.Bits {
		n += bits.OnesCount64(a.Bits[i] ^ b.Bits[i])
	}
	return n <= sameLookBits && max(a.Gray, b.Gray)-min(a.Gray, b.Gray) <= sameLookGray
}

// lookOf is img's look.
func lookOf(img *image.RGBA) *frameLook {
	small := scaleImage(img, 17, 16)
	var l frameLook
	sum := 0
	for y := 0; y < 16; y++ {
		for x := 0; x < 17; x++ {
			c := small.RGBAAt(x, y)
			g := (299*int(c.R) + 587*int(c.G) + 114*int(c.B)) / 1000
			sum += g
			if x == 16 {
				continue
			}
			n := small.RGBAAt(x+1, y)
			if g+lookMargin < (299*int(n.R)+587*int(n.G)+114*int(n.B))/1000 {
				bit := y*16 + x
				l.Bits[bit/64] |= 1 << (bit % 64)
			}
		}
	}
	l.Gray = uint8(sum / (17 * 16))
	return &l
}

// actedJS answers where control n is on the page as it is pictured: its
// box (CSS pixels) while it is there and in view, else the point the
// action pressed on this page (targetJS keeps it), else null (another
// page, or a step that pressed nothing).
const actedJS = `(n => {
  const c=window.__bnblocBrowse, e=c?.nodes.get(n);
  if (e?.isConnected && e.checkVisibility()) {
    const r=e.getBoundingClientRect();
    if (r.width && r.height && r.right>0 && r.bottom>0 && r.left<innerWidth && r.top<innerHeight) return {x:r.x,y:r.y,w:r.width,h:r.height};
  }
  return c?.acted?.node===n ? {x:c.acted.x,y:c.acted.y} : null;
})`

// actedMark is where control node is on a picture width wide of view, nil
// when it is not on it (no control, or the page is another).
func actedMark(ctx context.Context, p browserPage, node *int, view viewport, width int) *frameMark {
	if node == nil || view.W <= 0 {
		return nil
	}
	var at *struct{ X, Y, W, H float64 }
	if evaluate(ctx, p, "("+actedJS+")("+strconv.Itoa(*node)+")", &at) != nil || at == nil {
		return nil
	}
	s := float64(width) / view.W
	m := frameMark{X: int(math.Round(at.X * s)), Y: int(math.Round(at.Y * s))}
	if at.W > 0 && at.H > 0 {
		m.W, m.H = max(1, int(math.Round(at.W*s))), max(1, int(math.Round(at.H*s)))
	}
	return &m
}

// drawMark draws m on img: a frame around the control's box, a little
// outside it, or a dot ringed in white at the point pressed.
func drawMark(img *image.RGBA, m *frameMark) {
	if m == nil {
		return
	}
	paint := func(r image.Rectangle, c color.Color) {
		draw.Draw(img, r.Intersect(img.Bounds()), image.NewUniform(c), image.Point{}, draw.Src)
	}
	w := img.Bounds().Dx()
	if m.W > 0 && m.H > 0 {
		t := max(2, w/320)
		out := image.Rect(m.X, m.Y, m.X+m.W, m.Y+m.H).Inset(-(t + 2))
		paint(image.Rect(out.Min.X, out.Min.Y, out.Max.X, out.Min.Y+t), markColor)
		paint(image.Rect(out.Min.X, out.Max.Y-t, out.Max.X, out.Max.Y), markColor)
		paint(image.Rect(out.Min.X, out.Min.Y, out.Min.X+t, out.Max.Y), markColor)
		paint(image.Rect(out.Max.X-t, out.Min.Y, out.Max.X, out.Max.Y), markColor)
		return
	}
	rad := max(6, w/128)
	for y := -rad - 2; y <= rad+2; y++ {
		for x := -rad - 2; x <= rad+2; x++ {
			switch d := x*x + y*y; {
			case d <= rad*rad:
				paint(image.Rect(m.X+x, m.Y+y, m.X+x+1, m.Y+y+1), markColor)
			case d <= (rad+2)*(rad+2):
				paint(image.Rect(m.X+x, m.Y+y, m.X+x+1, m.Y+y+1), color.White)
			}
		}
	}
}

// frameChoice is what a step shows: the picture of step Show (its own, or
// the earlier one it looks the same as; 0: none) and a note saying why it
// is not its own, or why there is none. Rank orders the pictures: the
// lower, the sooner it goes when they do not all fit (dropOrder).
type frameChoice struct {
	Show int
	Note string
	Rank int
}

// chooseFrames is the one rule for which pictures a run's replay, and so
// its GIF guide, carries within budget bytes (size is a step's picture's,
// 0 none): all of them when they fit, else they go in dropOrder's order
// until the rest fit.
func chooseFrames(hist []historyRecord, size func(step int) int, budget int) []frameChoice {
	at := map[int]int{} // a step with a picture of its own → its index
	var pics []int
	total := 0
	for i, h := range hist {
		if h.Frame && !h.SignIn && size(h.Step) > 0 {
			at[h.Step] = i
			pics = append(pics, i)
			total += size(h.Step)
		}
	}
	// picOf is the index of the picture step i shows, -1 none.
	picOf := func(i int) int {
		if j, ok := at[hist[i].Step]; ok {
			return j
		}
		if j, ok := at[hist[i].Same]; ok && hist[i].Same != 0 {
			return j
		}
		return -1
	}
	prio := map[int]int{} // a picture → the highest priority of a step showing it
	for i := range hist {
		if j := picOf(i); j >= 0 {
			prio[j] = max(prio[j], framePriority(hist, i))
		}
	}
	order := dropOrder(len(hist), pics, prio)
	rank, dropped := map[int]int{}, map[int]bool{}
	for k, j := range order {
		rank[j] = k + 1
		if total > budget {
			dropped[j] = true
			total -= size(hist[j].Step)
		}
	}
	out := make([]frameChoice, len(hist))
	for i, h := range hist {
		j := picOf(i)
		switch {
		case h.SignIn:
			out[i].Note = replaySignIn
		case j < 0:
		case dropped[j] && j != i:
			out[i].Note = fmt.Sprintf("left out: same as step %d, over the replay budget", hist[j].Step)
		case dropped[j] && viewStep(h):
			out[i].Note = "left out: over the replay budget, a view step"
		case dropped[j]:
			out[i].Note = "left out: over the replay budget"
		default:
			out[i].Show, out[i].Rank = hist[j].Step, rank[j]
			if j != i {
				out[i].Note = fmt.Sprintf("same as step %d", hist[j].Step)
			}
		}
	}
	return out
}

// framePriority is how long step i's picture stays when pictures go: the
// first and last steps', a pause's and a page's message last (2), an
// acting step's (1) before a plain view step's (0).
func framePriority(hist []historyRecord, i int) int {
	switch h := hist[i]; {
	case i == 0 || i == len(hist)-1 || h.Checkpoint != "" || h.Operation == "DIALOG":
		return 2
	case viewStep(h):
		return 0
	}
	return 1
}

// viewStep reports a step that only looked (a scroll, a wait): nothing on
// the page acted on, nowhere gone.
func viewStep(h historyRecord) bool { return h.Kind == "scroll" || h.Kind == "wait" }

// dropOrder is the order n steps' pictures (pics, step indices ascending)
// go when they do not all fit: the lowest priority first, and within one,
// always the picture in the densest stretch of those left (the least room
// between its neighbours), so the ones that stay are spread evenly.
func dropOrder(n int, pics []int, prio map[int]int) []int {
	left := append([]int(nil), pics...)
	var order []int
	for len(left) > 0 {
		low := 3
		for _, j := range left {
			low = min(low, prio[j])
		}
		at, room := -1, 0
		for k, j := range left {
			if prio[j] != low {
				continue
			}
			before, after := -1, n
			if k > 0 {
				before = left[k-1]
			}
			if k < len(left)-1 {
				after = left[k+1]
			}
			if at < 0 || after-before < room {
				at, room = k, after-before
			}
		}
		order = append(order, left[at])
		left = append(left[:at], left[at+1:]...)
	}
	return order
}

// replayStep is one step as the replay page shows it: its own picture
// (Frame) or the earlier step's it looks as (Same).
type replayStep struct {
	Step  int    `json:"step"`
	What  string `json:"what"`
	Text  string `json:"text,omitempty"`
	URL   string `json:"url,omitempty"`
	At    string `json:"at"`
	Frame string `json:"frame,omitempty"`
	Same  int    `json:"same,omitempty"`
	Note  string `json:"note,omitempty"`
	rank  int
}

// replaySteps are the run's steps as its replay shows them, with the
// pictures chosen (chooseFrames) within what the file leaves for them.
func (t *tools) replaySteps(r *runState) []replayStep {
	steps := make([]replayStep, len(r.History))
	frames := map[int]string{} // a step → its picture (base64 JPEG)
	for i, h := range r.History {
		steps[i] = replayStep{Step: h.Step, What: describeStep(h), Text: h.Text, URL: h.URL, At: time.UnixMilli(h.AtMs).Format("15:04:05")}
		if h.Frame {
			if raw, err := os.ReadFile(filepath.Join(t.runner.store.framesDir(r.ID), strconv.Itoa(h.Step)+".jpg")); err == nil {
				frames[h.Step] = string(raw)
			}
		}
	}
	bare, _ := json.Marshal(steps)
	budget := replayMaxBytes - replayHeadroom - len(replayPage) - len(bare)
	for i, c := range chooseFrames(r.History, func(step int) int { return len(frames[step]) }, budget) {
		steps[i].Note, steps[i].rank = c.Note, c.Rank
		switch {
		case c.Show == r.History[i].Step:
			steps[i].Frame = frames[c.Show]
		case c.Show != 0:
			steps[i].Same = c.Show
		}
	}
	return steps
}

// writeReplay writes the run's replay into the workspace ("" writes none).
// It never fails the call: a replay is a record, not the work.
func (t *tools) writeReplay(r *runState) {
	if t.workspace == "" || len(r.History) == 0 {
		return
	}
	d := map[string]any{"run": r.ID, "sites": r.Sites, "started": time.UnixMilli(r.StartedAtMs).Format("2006-01-02 15:04"), "steps": t.replaySteps(r)}
	if _, ok := devices[r.Device]; ok {
		d["device"] = r.Device
	}
	if gif := t.gifOf(r.ID); gif != "" {
		d["gif"] = gif
	}
	data, err := json.Marshal(d)
	if err != nil {
		return
	}
	page := strings.Replace(replayPage, "{{DATA}}", string(data), 1)
	dir := filepath.Join(t.workspace, replayDir)
	tmp := filepath.Join(dir, "."+r.ID+".tmp")
	if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(tmp, []byte(page), 0o644) != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, r.ID+".html")); err != nil {
		_ = os.Remove(tmp)
	}
}

// describeStep is a step's one line.
func describeStep(h historyRecord) string {
	switch h.Operation {
	case "OPEN":
		return "Opened " + h.Action
	case "PERSON":
		return h.Action
	case "TYPE":
		return fmt.Sprintf("Typed into %q", h.Action)
	case "SELECT":
		return fmt.Sprintf("Chose in %q", h.Action)
	case "PRESS":
		return "Pressed " + h.Action
	case "SCROLL", "BACK":
		return h.Action
	case "UPLOAD":
		return fmt.Sprintf("Uploaded to %q", h.Action)
	}
	return fmt.Sprintf("Clicked %q", h.Action)
}

// replayPage is the replay: the steps are JSON data (json.Marshal escapes
// <, > and &, so page text cannot close the script), shown with textContent
// only, never as markup.
const replayPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Browser run replay</title>
<style>
body{margin:0;font:14px system-ui,sans-serif;background:#111;color:#eee;display:flex;flex-direction:column;height:100vh}
header{padding:6px 10px;border-bottom:1px solid #333;display:flex;gap:6px;align-items:center;min-width:0}
header strong{white-space:nowrap}
header .gif{color:#8ab4f8;white-space:nowrap}
header .meta{color:#999;flex:1;min-width:0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
button{background:#2a2a2a;color:#eee;border:1px solid #444;border-radius:6px;padding:4px 10px;cursor:pointer;flex-shrink:0}
#steps{display:none}
main{flex:1;display:flex;min-height:0}
#stage{flex:1;display:flex;flex-direction:column;min-width:0}
#shot{flex:1;min-height:0;display:flex;align-items:center;justify-content:center;background:#000}
#shot img{max-width:100%;max-height:100%;object-fit:contain}
#shot .none{color:#888;padding:24px;text-align:center}
#caption{padding:6px 10px;border-top:1px solid #333}
#what{display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
#caption .url,#caption .note{color:#888;font-size:12px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
ol{margin:0;padding:0;list-style:none;width:280px;overflow:auto;border-left:1px solid #333}
li{padding:6px 10px;border-bottom:1px solid #222;cursor:pointer;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
li.on{background:#1d3a5a}
li .at{color:#888;font-size:12px;margin-right:6px}
@media (max-width:720px){
  header strong{display:none}
  #steps{display:inline-block}
  main{flex-direction:column}
  ol{display:none;width:auto;max-height:40vh;border-left:0;border-top:1px solid #333}
  body.list ol{display:block}
}
</style></head><body>
<header><strong>Browser run replay</strong><span class="meta" id="meta"></span><a class="gif" id="gif" hidden>GIF guide</a>
<button id="steps">Steps</button><button id="prev">&#9664;</button><button id="play">Play</button><button id="next">&#9654;</button></header>
<main><div id="stage"><div id="shot"></div><div id="caption"><div id="what"></div><div class="note" id="note"></div><div class="url" id="url"></div></div></div><ol id="list"></ol></main>
<script id="data" type="application/json">{{DATA}}</script>
<script>
const d=JSON.parse(document.getElementById('data').textContent), steps=d.steps;
const $=id=>document.getElementById(id);
$('meta').textContent=d.run+' · '+(d.sites||[]).join(', ')+(d.device?' · as a '+d.device:'')+' · '+d.started+' · '+steps.length+' steps';
if(d.gif){$('gif').setAttribute('href',d.gif);$('gif').hidden=false}
const frames={};steps.forEach(s=>{if(s.frame)frames[s.step]=s.frame});
let at=steps.length-1, timer=null;
steps.forEach((s,i)=>{const li=document.createElement('li');const t=document.createElement('span');t.className='at';t.textContent=s.at;
  li.append(t,document.createTextNode(s.step+'. '+s.what));li.onclick=()=>{stop();show(i)};$('list').append(li)});
function show(i){if(i<0||i>=steps.length)return;at=i;const s=steps[i],shot=$('shot'),f=s.frame||frames[s.same];shot.replaceChildren();
  if(f){const img=new Image();img.alt='The page after step '+s.step;img.src='data:image/jpeg;base64,'+f;shot.append(img)}
  else{const p=document.createElement('div');p.className='none';p.textContent=s.note||'No picture for this step';shot.append(p)}
  $('what').textContent=s.step+'. '+s.what+(s.text?' — '+s.text:'');$('note').textContent=f&&s.note||'';$('url').textContent=s.url||'';
  [...$('list').children].forEach((li,j)=>li.classList.toggle('on',j===i));$('list').children[i]?.scrollIntoView({block:'nearest'})}
function stop(){clearInterval(timer);timer=null;$('play').textContent='Play'}
$('prev').onclick=()=>{stop();show(at-1)};$('next').onclick=()=>{stop();show(at+1)};
$('play').onclick=()=>{if(timer){stop();return}if(at>=steps.length-1)show(0);$('play').textContent='Pause';
  timer=setInterval(()=>{if(at>=steps.length-1){stop();return}show(at+1)},1500)};
$('steps').onclick=()=>document.body.classList.toggle('list');
addEventListener('keydown',e=>{if(e.key==='ArrowLeft')$('prev').click();if(e.key==='ArrowRight')$('next').click()});
show(at);
</script></body></html>
`
