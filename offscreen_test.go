package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	tb, err := openTab(ctx, c, false)
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
	tb, err := openTab(ctx, c, false)
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
