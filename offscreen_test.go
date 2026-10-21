package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// On a real Chrome, the page's controls below the fold are listed (marked
// off-screen), and an action on one scrolls it into view first.
func TestOffscreenControlOnChrome(t *testing.T) {
	c := startTestChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>long</title><div style="height:3000px">top</div><button onclick="document.title='clicked'">Start course</button>`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	pg, err := observe(ctx, tb)
	if err != nil {
		t.Fatal(err)
	}
	var start *action
	for i, a := range pg.Actions {
		if a.Kind == "click" && a.Label == "Start course" {
			start = &pg.Actions[i]
		}
	}
	if start == nil || !start.Offscreen {
		t.Fatalf("the button below the fold = %+v", start)
	}
	if err := act(ctx, tb, pg, *start, ""); err != nil {
		t.Fatalf("click = %v", err)
	}
	var title string
	if err := evaluate(ctx, tb, "document.title", &title); err != nil || title != "clicked" {
		t.Fatalf("title = %q, %v", title, err)
	}
}

// On a real Chrome, a control under a bar fixed to the window's edge is
// scrolled clear and clicked; one under an overlay is refused with what
// covers it, not as a changed page.
func TestCoveredControlOnChrome(t *testing.T) {
	c := startTestChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		overlay := ""
		if r.URL.Path == "/overlay" {
			overlay = `<div style="position:fixed;inset:0;z-index:9;background:#0003">Loading</div>`
		}
		_, _ = w.Write([]byte(`<title>quiz</title><style>#bar{position:fixed;bottom:0;left:0;right:0;height:200px;z-index:5;background:#eee}</style>
<div id="sp"></div><label style="display:block;height:100px"><input type="radio" name="q" onchange="document.title='picked'">China, Mexico, and Brazil</label>
<div style="height:2000px"></div><button id="bar">Submit</button>` + overlay + `<script>sp.style.height=(innerHeight-150)+'px'</script>`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	radio := func(path string) (*page, action) {
		if err := navigate(ctx, tb, srv.URL+path); err != nil {
			t.Fatal(err)
		}
		pg, err := observe(ctx, tb)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range pg.Actions {
			if a.Role == "radio" {
				return pg, a
			}
		}
		t.Fatalf("no radio on %s: %+v", path, pg.Actions)
		return nil, action{}
	}
	pg, a := radio("/")
	if err := act(ctx, tb, pg, a, ""); err != nil {
		t.Fatalf("click under the bar = %v", err)
	}
	var title string
	if err := evaluate(ctx, tb, "document.title", &title); err != nil || title != "picked" {
		t.Fatalf("title = %q, %v", title, err)
	}
	pg, a = radio("/overlay")
	var blocked blockedError
	if err := act(ctx, tb, pg, a, ""); !errors.As(err, &blocked) || !strings.Contains(blocked.why, `covered by "Loading"`) {
		t.Fatalf("click under the overlay = %v", err)
	}
}

// On a real Chrome, a replay pictured from a real page plays: its steps
// list, the latest picture shown, and a step chosen from the list.
func TestReplayPlaysOnChrome(t *testing.T) {
	c := startTestChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>quiz</title><h1>Question 2 of 15</h1>`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	tl := &tools{runner: &runner{store: runStore{dir: t.TempDir()}}, workspace: t.TempDir()}
	r := &runState{ID: "brun_chrome", Sites: []string{"example.com"}}
	r.History = []historyRecord{{Step: 1, Operation: "OPEN", Action: srv.URL, URL: srv.URL}, {Step: 2, Operation: "CLICK", Action: "Submit", URL: srv.URL}}
	for i := range r.History {
		tl.keepFrame(ctx, &runState{ID: r.ID, History: r.History[:i+1]}, tb)
		r.History[i].Frame = true
	}
	tl.writeReplay(r)
	if err := navigate(ctx, tb, "file://"+filepath.Join(tl.workspace, replayDir, r.ID+".html")); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Steps int
		Shown string
		Img   bool
	}
	// A JPEG in base64 starts "/9j/".
	expr := `(()=>{const s=document.querySelectorAll('li').length;document.querySelectorAll('li')[0].click();
	  const img=document.querySelector('#shot img');return {Steps:s,Shown:document.getElementById('what').textContent,Img:!!img&&img.src.startsWith('data:image/jpeg;base64,/9j/')}})()`
	if err := evaluate(ctx, tb, expr, &got); err != nil || got.Steps != 2 || !strings.HasPrefix(got.Shown, "1. Opened") || !got.Img {
		t.Fatalf("replay = %+v, %v", got, err)
	}
}

// A page laid out far smaller than its window is the shrunk view; one
// that fills it, or a phone-sized window, is not.
func TestShrunkViewport(t *testing.T) {
	for _, c := range []struct {
		w, h, ww, wh float64
		want         bool
	}{
		{128, 87, 1200, 900, true},
		{512, 347, 1200, 900, true},
		{1200, 813, 1200, 900, false},
		{390, 700, 390, 844, false},
		{0, 0, 1200, 900, false},
	} {
		if got := shrunkViewport(c.w, c.h, c.ww, c.wh); got != c.want {
			t.Fatalf("shrunkViewport(%v) = %v", c, got)
		}
	}
}

// On a real Chrome, fitting a page that fills its window changes nothing.
func TestFitViewportLeavesAFittingPageOnChrome(t *testing.T) {
	c := startTestChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var before, after [2]float64
	_ = evaluate(ctx, tb, "[innerWidth, innerHeight]", &before)
	tb.fitViewport(ctx)
	_ = evaluate(ctx, tb, "[innerWidth, innerHeight]", &after)
	if before != after || before[0] == 0 {
		t.Fatalf("viewport %v → %v", before, after)
	}
}

// On a real Chrome, two reads of one document are the same page, also
// after a click changed it; a load of the same address is another.
func TestSamePageOnChrome(t *testing.T) {
	c := startTestChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>q</title><label><input type="radio" name="a">A</label><p id="s"></p>`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	a, _ := observe(ctx, tb)
	_ = evaluate(ctx, tb, "document.getElementById('s').textContent='picked'", nil)
	b, _ := observe(ctx, tb)
	if !samePage(a, b) {
		t.Fatalf("one document read twice is not the same page: %s vs %s", a.PageKey, b.PageKey)
	}
	if err := navigate(ctx, tb, srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	if c, _ := observe(ctx, tb); samePage(a, c) {
		t.Fatal("a new load is the same page")
	}
}

// On a real Chrome, pictures taken at once from two sessions leave the
// page its size (a capture Chrome scaled shrank it, QA 2026-10-19), and a
// picture is scaled here to at most maxShotWidth.
func TestCapturesAtOnceKeepThePageSizeOnChrome(t *testing.T) {
	c := startTestChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, "data:text/html,<title>x</title><h1>hello</h1>"); err != nil {
		t.Fatal(err)
	}
	other, err := attachTab(ctx, tb.conn, tb.targetID, "")
	if err != nil {
		t.Fatal(err)
	}
	var before, after [2]float64
	_ = evaluate(ctx, tb, "[innerWidth, innerHeight]", &before)
	var wg sync.WaitGroup
	shots := make(chan string, 60)
	for _, p := range []*tab{tb, other} {
		wg.Add(1)
		go func(p *tab) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if shot, _, err := capture(ctx, p, 50); err == nil {
					shots <- shot
				}
			}
		}(p)
	}
	wg.Wait()
	close(shots)
	_ = evaluate(ctx, tb, "[innerWidth, innerHeight]", &after)
	if before != after {
		t.Fatalf("the page shrank: %v → %v", before, after)
	}
	raw, _ := base64.StdEncoding.DecodeString(<-shots)
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := shotSize(viewport{W: before[0], H: before[1]}, maxShotWidth)
	if img.Bounds().Dx() != want || want > maxShotWidth {
		t.Fatalf("picture %v wide, want %d", img.Bounds().Dx(), want)
	}
}
