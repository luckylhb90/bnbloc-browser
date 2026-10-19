package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
