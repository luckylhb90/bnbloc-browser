package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/component"
)

// lateSignIn is a site and its identity provider on a real Chrome: the
// site's pages at 127.0.0.1, the provider's at localhost (another site to
// the run). /task sends itself to the provider's login-name page by script
// once it loaded (a session check: QA 2026-10-20), /away to another site's
// ordinary page; /home stays.
type lateSignIn struct {
	h          *harness
	site, idp  string
	mu         sync.Mutex
	posts      []liveFramePost
	kept       [][2]string
	signInPath string
}

func newLateSignIn(t *testing.T, browser string) *lateSignIn {
	t.Helper()
	ls := &lateSignIn{signInPath: "/sso/login?app=task"}
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/sso/login":
			_, _ = w.Write([]byte(`<title>Company SSO</title><p>Enter your login name</p><label>Login name <input name="user"></label><button>Next</button>`))
		default:
			_, _ = w.Write([]byte(`<title>Elsewhere</title><p>Another site's page</p>`))
		}
	}))
	t.Cleanup(idp.Close)
	ls.idp = strings.Replace(idp.URL, "127.0.0.1", "localhost", 1)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		redirect := ""
		switch r.URL.Path {
		case "/task":
			redirect = ls.idp + ls.signInPath
		case "/away":
			redirect = ls.idp + "/elsewhere"
		}
		page := `<title>Portal</title><p>Your tasks</p><button>Start</button>`
		if redirect != "" {
			page += `<script>setTimeout(() => { location.href = "` + redirect + `" }, 800)</script>`
		}
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(site.Close)
	ls.site = site.URL

	ls.h = newHarness(t, nil, func(q decReq) (string, string) { return "", "" })
	host := newLocalHost(hostConfig{Endpoint: browser, Headless: true})
	host.dl = t.TempDir()
	ls.h.tools.runner.host = host
	ls.h.tools.keeper = func(run, job string) error {
		ls.mu.Lock()
		ls.kept = append(ls.kept, [2]string{run, job})
		ls.mu.Unlock()
		return nil
	}
	ls.h.relay.live = func(post liveFramePost) component.LiveFrameReply {
		ls.mu.Lock()
		ls.posts = append(ls.posts, post)
		ls.mu.Unlock()
		return component.LiveFrameReply{}
	}
	return ls
}

// handedOver checks that run waits for the person to sign in exactly as a
// pause settle found would: saved on the run, its view announced in the
// room with what it waits for, the keeper started under the turn's job,
// and the sign-in in the replay's record.
func (ls *lateSignIn) handedOver(t *testing.T, head map[string]any, text string) {
	t.Helper()
	run, _ := head["run"].(string)
	if head["checkpoint"] != cpNeedsHuman || !strings.Contains(text, "Enter your login name") {
		t.Fatalf("answer = %v\n%s", head, text)
	}
	r, err := ls.h.tools.runner.store.load(run)
	if err != nil || r.Checkpoint == nil || r.Checkpoint.Kind != cpNeedsHuman || r.Checkpoint.Waiting != waitSignIn || r.PausedJob != "job-1" {
		t.Fatalf("the run's pause = %+v (%v)", r, err)
	}
	if last := r.History[len(r.History)-1]; last.Operation != "PERSON" || !last.SignIn {
		t.Fatalf("history = %+v", r.History)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.kept) != 1 || ls.kept[0] != [2]string{run, "job-1"} {
		t.Fatalf("keeper started %v", ls.kept)
	}
	announced := false
	for _, p := range ls.posts {
		announced = announced || p.Run == run && p.Waiting == waitSignIn
	}
	if !announced {
		t.Fatalf("the view was not handed to the person: %+v", ls.posts)
	}
}

// A page that sends itself to the identity provider by script after it
// loaded pauses for the person, whichever call sees the provider's page:
// the open that waited for its page to come to rest, or a view after an
// open that had answered the page before it moved. A page that moves to
// another site's ordinary page still waits for the requester's word.
func TestLateSignInRedirectOnChrome(t *testing.T) {
	browser := startTestBrowser(t)

	t.Run("open", func(t *testing.T) {
		ls := newLateSignIn(t, browser)
		head, text := ls.h.call("browser_open", map[string]any{"url": ls.site + "/task"})
		ls.handedOver(t, head, text)
	})

	t.Run("view", func(t *testing.T) {
		ls := newLateSignIn(t, browser)
		head, _ := ls.h.call("browser_open", map[string]any{"url": ls.site + "/home"})
		run, _ := head["run"].(string)
		if head["checkpoint"] != nil || run == "" {
			t.Fatalf("open = %v", head)
		}
		// The page's own script sends it on after the open answered.
		r, _ := ls.h.tools.runner.store.load(run)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		tb, err := ls.h.tools.runner.host.attach(ctx, r.Tab, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := evaluate(ctx, tb, `setTimeout(() => { location.href = "`+ls.idp+ls.signInPath+`" }, 0), 1`, nil); err != nil {
			t.Fatal(err)
		}
		tb.detach(ctx)
		for u := ""; !strings.Contains(u, "/sso/login"); {
			if ctx.Err() != nil {
				t.Fatalf("the page did not move: %s", u)
			}
			time.Sleep(50 * time.Millisecond)
			u, _ = ls.h.tools.runner.host.url(ctx, r.Tab)
		}
		head, text := ls.h.call("browser_view", map[string]any{"run": run})
		ls.handedOver(t, head, text)
	})

	t.Run("leaving the site", func(t *testing.T) {
		ls := newLateSignIn(t, browser)
		head, text := ls.h.call("browser_open", map[string]any{"url": ls.site + "/away"})
		if head["checkpoint"] != cpLeavingSite || head["site"] != "localhost" || !strings.Contains(text, "Another site's page") {
			t.Fatalf("answer = %v\n%s", head, text)
		}
		ls.mu.Lock()
		defer ls.mu.Unlock()
		if len(ls.kept) != 0 {
			t.Fatalf("a keeper started for a page off the site: %v", ls.kept)
		}
	})
}

// On a real Chrome, a page reads as on its way for a moment after it
// loaded and after it began leaving for another document (here one that
// answers nothing, so the page stays), then as at rest.
func TestPageOnItsWayOnChrome(t *testing.T) {
	c := startTestChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/nothing" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>Portal</title><p>Your tasks</p>`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, srv.URL+"/tasks"); err != nil {
		t.Fatal(err)
	}
	pg, err := observe(ctx, tb)
	if err != nil || !onItsWay(pg) || pg.LoadedMs == nil || pg.LeftMs != nil {
		t.Fatalf("a page just loaded: %+v %v", pg, err)
	}
	time.Sleep(justMoved)
	if pg, _ = observe(ctx, tb); onItsWay(pg) {
		t.Fatalf("a page loaded a while ago: loaded %v ms ago", *pg.LoadedMs)
	}
	if err := evaluate(ctx, tb, `location.href = "/nothing", 1`, nil); err != nil {
		t.Fatal(err)
	}
	if pg, _ = observe(ctx, tb); !onItsWay(pg) || pg.LeftMs == nil || !strings.HasSuffix(pg.URL, "/tasks") {
		t.Fatalf("a page that began leaving: %s left %v", pg.URL, pg.LeftMs)
	}
	time.Sleep(justMoved)
	if pg, _ = observe(ctx, tb); onItsWay(pg) || !strings.HasSuffix(pg.URL, "/tasks") {
		t.Fatalf("a page whose navigation came to nothing: %s left %v ms ago", pg.URL, *pg.LeftMs)
	}
}
