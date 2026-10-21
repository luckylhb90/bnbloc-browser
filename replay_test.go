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
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// Within the budget every picture stays; past it the plain view steps'
// go first, each saying why, then acting ones evenly, while the first and
// last steps and a pause's stay; a step that looks as an earlier one shows
// that one's picture, and goes with it.
func TestChooseFramesKeepsWhatMatters(t *testing.T) {
	hist := []historyRecord{
		{Step: 1, Operation: "OPEN", Kind: "nav", Frame: true},
		{Step: 2, Operation: "TYPE", Kind: "fill", Frame: true},
		{Step: 3, Operation: "SCROLL", Kind: "scroll", Frame: true},
		{Step: 4, Operation: "CLICK", Kind: "click", Frame: true},
		{Step: 5, Operation: "SCROLL", Kind: "scroll", Frame: true},
		{Step: 6, Operation: "CLICK", Kind: "click", Same: 5},
		{Step: 7, Operation: "SCROLL", Kind: "scroll", Same: 4},
		{Step: 8, Operation: "OPEN", Kind: "nav", SignIn: true},
		{Step: 9, Operation: "SELECT", Kind: "select", Frame: true},
		{Step: 10, Operation: "CLICK", Kind: "click", Frame: true, Checkpoint: cpNeedsConfirmation},
		{Step: 11, Operation: "SCROLL", Kind: "scroll", Frame: true},
		{Step: 12, Operation: "PRESS", Kind: "key", Frame: true},
	}
	size := func(int) int { return 100 }
	shown := func(cs []frameChoice) string {
		var out []string
		for _, c := range cs {
			out = append(out, strconv.Itoa(c.Show))
		}
		return strings.Join(out, ",")
	}
	all := chooseFrames(hist, size, 1000)
	if got := shown(all); got != "1,2,3,4,5,5,4,0,9,10,11,12" {
		t.Fatalf("within the budget: %s", got)
	}
	if all[5].Note != "same as step 5" || all[6].Note != "same as step 4" || all[7].Note != replaySignIn || all[0].Note != "" {
		t.Fatalf("notes = %+v", all)
	}
	// Two pictures too many: the view steps 3 and 11 go; 5 stays, as
	// acting step 6 shows it.
	some := chooseFrames(hist, size, 700)
	if got := shown(some); got != "1,2,0,4,5,5,4,0,9,10,0,12" {
		t.Fatalf("two over: %s", got)
	}
	if some[2].Note != "left out: over the replay budget, a view step" || some[10].Note != some[2].Note {
		t.Fatalf("a dropped view step says %q", some[2].Note)
	}
	// Room for four: the first, the last and the pause, and of the acting
	// ones the one in the middle.
	few := chooseFrames(hist, size, 400)
	if got := shown(few); got != "1,0,0,0,5,5,0,0,0,10,0,12" {
		t.Fatalf("room for four: %s", got)
	}
	if few[1].Note != "left out: over the replay budget" || few[6].Note != "left out: same as step 4, over the replay budget" {
		t.Fatalf("notes = %q, %q", few[1].Note, few[6].Note)
	}
	// What stays last ranks highest.
	if few[0].Rank <= few[4].Rank || all[2].Rank >= all[1].Rank {
		t.Fatalf("ranks = %+v / %+v", few, all)
	}
}

// Pictures of acting steps go evenly: of nine between the first and the
// last, every other.
func TestDropOrderGoesEvenly(t *testing.T) {
	pics, prio := []int{}, map[int]int{}
	for i := 0; i <= 10; i++ {
		pics = append(pics, i)
		prio[i] = 1
	}
	prio[0], prio[10] = 2, 2
	if got := fmt.Sprint(dropOrder(11, pics, prio)[:5]); got != "[1 3 5 7 9]" {
		t.Fatalf("first to go: %s", got)
	}
}

// pagePicture is a synthetic page w wide: a header bar, lines of
// "words", a coloured card and a button, at the given offset down.
func pagePicture(w, h, down int, seed int64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{252, 252, 248, 255}), image.Point{}, draw.Src)
	fill := func(r image.Rectangle, c color.RGBA) { draw.Draw(img, r, image.NewUniform(c), image.Point{}, draw.Src) }
	fill(image.Rect(0, 0, w, 64), color.RGBA{46, 83, 68, 255})
	rnd := rand.New(rand.NewSource(seed))
	for y := 100 + down; y < h-40; y += 26 {
		for x := 40; x < w*2/3; {
			word := 20 + rnd.Intn(60)
			fill(image.Rect(x, y, x+word, y+12), color.RGBA{52, 62, 56, 255})
			x += word + 8
		}
	}
	fill(image.Rect(w*2/3+30, 120+down, w-40, 360+down), color.RGBA{220, 232, 214, 255})
	fill(image.Rect(w*2/3+60, 300+down, w*2/3+200, 340+down), color.RGBA{56, 80, 62, 255})
	return img
}

// reencoded is img through a JPEG at quality q, as a later capture of the
// same page would be.
func reencoded(t *testing.T, img image.Image, q int) *image.RGBA {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	back, err := jpeg.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	rgba := image.NewRGBA(back.Bounds())
	draw.Draw(rgba, rgba.Bounds(), back, image.Point{}, draw.Src)
	return rgba
}

// A picture of the same page looks the same through another encoding;
// a page scrolled, another page or a darker one does not.
func TestLookFindsTheSamePicture(t *testing.T) {
	base := pagePicture(1280, 800, 0, 1)
	look := *lookOf(base)
	if !sameLook(look, *lookOf(reencoded(t, base, 60))) {
		t.Fatal("the same page through a JPEG looks different")
	}
	dark := image.NewRGBA(base.Bounds())
	for i := range base.Pix {
		dark.Pix[i] = base.Pix[i]
		if i%4 != 3 {
			dark.Pix[i] = base.Pix[i] / 2
		}
	}
	for name, other := range map[string]*image.RGBA{"scrolled": pagePicture(1280, 800, 26, 1), "another": pagePicture(1280, 800, 0, 2), "dark": dark} {
		if sameLook(look, *lookOf(other)) {
			t.Fatalf("a %s page looks the same", name)
		}
	}
	// keepFrame's rule: alike marks, and the same fingerprint or look.
	prev := historyRecord{Page: "p1", Look: &look, Mark: &frameMark{X: 1, Y: 2, W: 3, H: 4}}
	for _, c := range []struct {
		cur  historyRecord
		same bool
	}{
		{historyRecord{Page: "p1", Look: lookOf(pagePicture(1280, 800, 0, 2)), Mark: &frameMark{X: 1, Y: 2, W: 3, H: 4}}, true},
		{historyRecord{Page: "p2", Look: &look, Mark: &frameMark{X: 1, Y: 2, W: 3, H: 4}}, true},
		{historyRecord{Page: "p1", Look: &look}, false},
		{historyRecord{Page: "p1", Look: &look, Mark: &frameMark{X: 9, Y: 2, W: 3, H: 4}}, false},
		{historyRecord{Page: "p2", Look: lookOf(pagePicture(1280, 800, 0, 2)), Mark: &frameMark{X: 1, Y: 2, W: 3, H: 4}}, false},
	} {
		if got := sameFrame(prev, c.cur); got != c.same {
			t.Fatalf("sameFrame(%+v) = %v", c.cur, got)
		}
	}
}

// An acted control's box is framed in red a little outside it; a point
// pressed is a red dot ringed in white. Nothing else changes.
func TestDrawMarkAtTheControl(t *testing.T) {
	grey := color.RGBA{128, 128, 128, 255}
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	draw.Draw(img, img.Bounds(), image.NewUniform(grey), image.Point{}, draw.Src)
	drawMark(img, &frameMark{X: 100, Y: 60, W: 80, H: 30})
	// Two pixels thick (400 wide: one a 320), two pixels out: x 96..97 and 182..183,
	// y 56..57 and 92..93.
	for _, c := range []struct {
		x, y int
		want color.RGBA
	}{
		{96, 75, markColor}, {97, 75, markColor}, {98, 75, grey}, {140, 75, grey},
		{183, 75, markColor}, {184, 75, grey}, {140, 56, markColor}, {140, 93, markColor},
		{140, 55, grey}, {140, 94, grey}, {95, 75, grey}, {300, 150, grey},
	} {
		if got := img.RGBAAt(c.x, c.y); got != c.want {
			t.Fatalf("(%d,%d) = %v; want %v", c.x, c.y, got, c.want)
		}
	}
	draw.Draw(img, img.Bounds(), image.NewUniform(grey), image.Point{}, draw.Src)
	drawMark(img, &frameMark{X: 300, Y: 100})
	if img.RGBAAt(300, 100) != markColor || img.RGBAAt(305, 100) != markColor || img.RGBAAt(307, 100) != (color.RGBA{255, 255, 255, 255}) || img.RGBAAt(310, 100) != grey {
		t.Fatalf("the dot: %v %v %v %v", img.RGBAAt(300, 100), img.RGBAAt(305, 100), img.RGBAAt(307, 100), img.RGBAAt(310, 100))
	}
	// A box at the picture's edge is clipped, not a panic.
	drawMark(img, &frameMark{X: 390, Y: 190, W: 40, H: 40})
}

// A 1280-wide picture of a page dense with text, at the replay's quality,
// stays within 128 KB (a page's real pictures measure 50 to 125 KB:
// TestReplayRunOnChrome), so thirty or more fit the replay's file.
func TestFrameJPEGIsSmall(t *testing.T) {
	var out bytes.Buffer
	if err := jpeg.Encode(&out, pagePicture(replayFrameWidth, 800, 0, 1), &jpeg.Options{Quality: replayQuality}); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodedLen(out.Len())
	t.Logf("a 1280x800 page: %d bytes, %d as base64", out.Len(), b64)
	if out.Len() > 128<<10 || 30*b64 > replayMaxBytes-replayHeadroom {
		t.Fatalf("a frame is %d bytes (%d of base64)", out.Len(), b64)
	}
}

// A step that leaves the page as it was keeps no picture of its own: the
// replay plays the earlier one and says so. An acted control is marked.
func TestReplayPlaysTheSamePictureOnce(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	h.tools.workspace = t.TempDir()
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	run := head["run"].(string)
	h.call("browser_act", map[string]any{"run": run, "action": "scroll"})
	h.call("browser_act", map[string]any{"run": run, "action": "type", "element": 1, "text": "120.50"})
	r, err := h.tools.runner.store.load(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.History) != 3 || r.History[1].Frame || r.History[1].Same != 1 || !r.History[2].Frame || r.History[2].Mark == nil {
		t.Fatalf("history = %+v", r.History)
	}
	if _, err := os.Stat(filepath.Join(h.tools.runner.store.framesDir(run), "2.jpg")); err == nil {
		t.Fatal("the same picture was kept twice")
	}
	steps, page := replayOf(t, h.tools.workspace, run)
	if steps[1].Frame != "" || steps[1].Same != 1 || steps[1].Note != "same as step 1" || steps[0].Frame == "" || steps[2].Frame == "" {
		t.Fatalf("steps = %+v", steps)
	}
	if !strings.Contains(page, "s.frame||frames[s.same]") {
		t.Fatal("the replay does not play an earlier picture")
	}
	// The fake's control 1 is at (100,40) 80×24 in its 1000-wide page.
	img := decodeFrame(steps[2].Frame)
	if img == nil || !near(img.At(96, 50), markColor) || near(img.At(140, 50), markColor) {
		t.Fatalf("the typed-into control is not marked")
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

// replayAppPage is a small order app on two pages: a form with fields,
// dropdowns, checkboxes and buttons that change it, tall enough to
// scroll, and a review page.
const replayAppPage = `<!doctype html><title>Order supplies</title>
<style>body{font:15px system-ui,sans-serif;margin:0;color:#333}header{background:#2e5344;color:#fff;padding:16px 32px;font-size:22px}
main{padding:24px 32px;max-width:900px}label{display:block;margin:10px 0}input,select,textarea{font:inherit;margin-left:8px}
button{font:inherit;padding:8px 14px;margin:6px 6px 6px 0}#lines li{margin:4px 0}#msg{color:#a33;min-height:20px}.tall{height:900px;background:linear-gradient(#fff,#e8efe4)}</style>
<header>Supplies · Orders</header><main><h1>New order</h1><p>Fill in the order and review it before it is sent.</p>
<label>Name <input id="name"></label><label>Email <input id="email" type="email"></label><label>Quantity <input id="qty" type="number"></label>
<label>Department <select id="dept"><option>Sales</option><option>Support</option><option>Operations</option></select></label>
<label>Priority <select id="prio"><option>Low</option><option>High</option></select></label>
<label><input type="checkbox" id="gift">Gift wrap</label><label><input type="checkbox" id="express">Express delivery</label>
<button id="add" onclick="lines.insertAdjacentHTML('beforeend','<li>Line '+(lines.children.length+1)+': paper, 5 boxes</li>')">Add line</button>
<button onclick="msg.textContent='In stock: '+(lines.children.length*5+12)+' boxes'">Check stock</button>
<ol id="lines"></ol><p id="msg"></p><label>Notes <textarea id="notes" rows="3" cols="40"></textarea></label>
<div class="tall"><p>Terms of supply: orders placed before noon ship the same day.</p></div>
<a href="/review">Review order</a></main>`

const replayReviewPage = `<!doctype html><title>Review order</title>
<style>body{font:15px system-ui,sans-serif;margin:0;color:#333}header{background:#2e5344;color:#fff;padding:16px 32px;font-size:22px}
main{padding:24px 32px}button{font:inherit;padding:8px 14px;margin:6px 6px 6px 0}#addr{display:none;background:#f3f6f1;padding:12px}.tall{height:1200px}</style>
<header>Supplies · Review</header><main><h1>Review your order</h1><table><tr><td>Paper</td><td>5 boxes</td></tr><tr><td>Toner</td><td>2</td></tr></table>
<button onclick="addr.style.display=addr.style.display==='block'?'none':'block'">Edit address</button><div id="addr">Main street 1, Lisbon</div>
<label>Coupon <input id="coupon"></label><button onclick="done.textContent='Coupon '+coupon.value+' applied'">Apply coupon</button><p id="done"></p>
<div class="tall"></div><button onclick="done.textContent='Order confirmed'">Confirm order</button></main>`

// On a real Chrome, a run of 25 steps over a small app leaves a replay in
// which every step that acted has a picture, the controls acted on
// marked, and the file within its bound.
func TestReplayRunOnChrome(t *testing.T) {
	browser := startTestBrowser(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/review" {
			_, _ = w.Write([]byte(replayReviewPage))
			return
		}
		_, _ = w.Write([]byte(replayAppPage))
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, nil, func(q decReq) (string, string) { return "", "" })
	h.model.off = true
	host := newLocalHost(hostConfig{Endpoint: browser, Headless: true})
	host.dl = t.TempDir()
	h.tools.runner.host = host
	h.tools.workspace = t.TempDir()

	start := time.Now()
	head, _ := h.call("browser_open", map[string]any{"url": srv.URL + "/"})
	run, _ := head["run"].(string)
	if run == "" || head["checkpoint"] != nil {
		t.Fatalf("open = %v", head)
	}
	// el is the number of the control labelled label on the page now.
	el := func(label string) int {
		t.Helper()
		_, text := h.call("browser_view", map[string]any{"run": run})
		m := regexp.MustCompile(`\[(\d+)\] \S+ "` + regexp.QuoteMeta(label)).FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("no control %q:\n%s", label, text)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	act := func(args map[string]any) {
		t.Helper()
		args["run"] = run
		if head, text := h.call("browser_act", args); head["checkpoint"] != nil || strings.Contains(fmt.Sprint(head["note"]), "nothing was done") {
			t.Fatalf("act %v = %v\n%s", args, head, text)
		}
	}
	typeInto := func(label, text string) { act(map[string]any{"action": "type", "element": el(label), "text": text}) }
	click := func(label string) { act(map[string]any{"action": "click", "element": el(label)}) }
	typeInto("Name", "Maya")
	typeInto("Email", "maya@example.com")
	typeInto("Quantity", "5")
	act(map[string]any{"action": "select", "element": el("Department"), "option": "Support"})
	act(map[string]any{"action": "select", "element": el("Priority"), "option": "High"})
	click("Gift wrap")
	click("Express delivery")
	click("Add line")
	click("Add line")
	typeInto("Notes", "Leave at the front desk")
	click("Check stock")
	// (A wheel scroll waits for a frame a headless tab may never draw.)
	click("Add line")
	click("Gift wrap")
	click("Review order")
	click("Edit address")
	typeInto("Coupon", "SPRING")
	click("Apply coupon")
	act(map[string]any{"action": "press", "key": "Tab"})
	click("Edit address")
	click("Confirm order")
	act(map[string]any{"action": "back"})
	typeInto("Name", "Maya Lin")
	click("Check stock")
	click("Review order")

	r, err := h.tools.runner.store.load(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.History) != 25 {
		t.Fatalf("%d steps: %+v", len(r.History), r.History)
	}
	steps, page := replayOf(t, h.tools.workspace, run)
	own, same, marked := 0, 0, 0
	for i, s := range steps {
		hr := r.History[i]
		pictured := s.Frame != "" || s.Same != 0 && steps[s.Same-1].Frame != ""
		if !viewStep(hr) && !pictured {
			t.Fatalf("acting step %d (%s) has no picture: %q", s.Step, s.What, s.Note)
		}
		switch {
		case s.Frame != "":
			own++
		case s.Same != 0:
			same++
		}
		if hr.Mark != nil {
			marked++
		}
	}
	// Steps that acted on a control on the page they stayed on have it
	// marked: the fields, dropdowns, checkboxes and buttons.
	if marked < 15 {
		t.Fatalf("%d steps marked: %+v", marked, r.History)
	}
	if !strings.HasPrefix(steps[1].What, `Typed into "Name"`) {
		t.Fatalf("step 2 = %q", steps[1].What)
	}
	frames := 0
	for _, s := range steps {
		frames += len(s.Frame)
	}
	t.Logf("%d steps in %v", len(steps), time.Since(start).Round(time.Millisecond))
	t.Logf("replay %d bytes: %d own pictures, %d the same as one before, %d marked; pictures %d bytes of base64 (%d a picture)", len(page), own, same, marked, frames, frames/max(1, own))
	if len(page) > replayMaxBytes {
		t.Fatalf("the replay is %d bytes, over %d", len(page), replayMaxBytes)
	}
	// Forty steps of such pictures would still fit.
	if 40*frames/max(1, own) > replayMaxBytes-replayHeadroom {
		t.Fatalf("pictures of %d bytes: forty do not fit", frames/max(1, own))
	}
	// The replay plays a step the same as one before with that one's
	// picture, and says so.
	at := -1
	for i, s := range steps {
		if s.Same != 0 {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("no step the same as one before: %+v", r.History)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := dialCDP(ctx, browser)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, "file://"+filepath.Join(h.tools.workspace, replayDir, run+".html")); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Img  bool
		Note string
	}
	expr := `(()=>{document.querySelectorAll('li')[` + strconv.Itoa(at) + `].click();const img=document.querySelector('#shot img');
	  return {Img:!!img&&img.src.startsWith('data:image/jpeg;base64,/9j/'),Note:document.getElementById('note').textContent}})()`
	if err := evaluate(ctx, tb, expr, &got); err != nil || !got.Img || got.Note != fmt.Sprintf("same as step %d", steps[at].Same) {
		t.Fatalf("step %d plays %+v (%v)", at+1, got, err)
	}
}
