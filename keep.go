package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A run paused for a person (needs_human: a sign-in, a password, a second
// factor) keeps its live view in the room after the call (bnbloc
// docs/prd/2026-10-17-browser-component.md §2.8, §3.7): the person does
// the step from the room, in the sidebar or a pop-out, on any device.
//
// The call's process ends with the agent's turn, so the view is kept by a
// keeper: this executable again (`bnbloc-browser keep`), detached like the
// browser, posting the paused run's frames under the turn's job through the
// same relay. Each post says what the view waits for, which is what lets
// the gateway take it after the turn. It stops when:
//
//   - the run is resumed: the resuming call stops it (stopKeeper) and
//     shows the run under its own turn;
//   - the owner closes the view from the room (LiveInputClose);
//   - the tab or the browser is gone;
//   - the relay refuses its posts (the agent's session was released);
//   - another run's keeper takes the job: the room shows one view per job,
//     so the newest holds it and an older one leaves quietly (its End
//     would close the newer view).
//
// No clock ends it: it waits for as long as the view waits.

// keepFailures is how many failed looks at the page in a row make the
// keeper ask whether its tab is still open.
const keepFailures = 3

// keepPostFor bounds one look and post of the keeper.
const keepPostFor = 20 * time.Second

// handsOffLook is how often a keeper that let go of the page looks at its
// address.
const handsOffLook = time.Second

// handsOffWaiting is what a view waits for while the person works in the
// browser window itself.
const handsOffWaiting = "Doing it in the browser window on the computer; let the agent continue when it is done"

func (s runStore) keepLock(id string) string { return s.path(id) + ".keep.lock" }

// jobKeeper names the file that says which run's keeper holds a job's view.
func (s runStore) jobKeeper(job string) string {
	return filepath.Join(s.dir, "keeper-"+strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return '_'
	}, job))
}

// holdsJob reports whether run's keeper still holds job's view.
func (s runStore) holdsJob(job, run string) bool {
	raw, err := os.ReadFile(s.jobKeeper(job))
	return err != nil || string(raw) == run
}
func (s runStore) keepStop(id string) string { return s.path(id) + ".keep.stop" }

// stopKeeper stops the run's keeper, if one runs, and waits for it to post
// the view's end (bounded: a stuck keeper does not hold the call).
func (s runStore) stopKeeper(id string) {
	stop := s.keepStop(id)
	if err := os.WriteFile(stop, nil, 0o600); err != nil {
		return
	}
	defer os.Remove(stop)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		unlock, ok, err := tryLockFile(s.keepLock(id))
		if err != nil {
			return
		}
		if ok {
			unlock()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startKeeper starts exe's keeper of run under job, detached, with the
// flags the server was started with (where the browser and the runs are)
// and its environment (the relay). Its output goes to a log beside the run.
func startKeeper(exe string, flags []string, store runStore, run, job string) error {
	args := append([]string{"keep"}, flags...)
	args = append(args, "--state-dir", store.dir, "--run", run, "--job", job)
	cmd := exec.Command(exe, args...)
	if logf, err := os.OpenFile(store.path(run)+".keep.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		cmd.Stdout, cmd.Stderr = logf, logf
		defer logf.Close()
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// keep holds a paused run's live view until one of the ends above. One
// keeper per run: a second one exits at once.
func keep(ctx context.Context, gw *relay, host browserHost, store runStore, runID, job string) error {
	unlock, ok, err := tryLockFile(store.keepLock(runID))
	if err != nil || !ok {
		return err
	}
	defer unlock()
	if stopped(store, runID) {
		logKeep("stopped before it began")
		return nil
	}
	r, err := store.load(runID)
	if err != nil || r.Checkpoint == nil || r.Checkpoint.Kind != cpNeedsHuman {
		logKeep("the run is not paused for a person")
		return err
	}
	if err := os.WriteFile(store.jobKeeper(job), []byte(runID), 0o600); err != nil {
		return err
	}
	logKeep("holding the view of %s under %s", runID, job)
	end := func() {
		ectx, cancel := context.WithTimeout(context.Background(), keepPostFor)
		defer cancel()
		_, _ = gw.liveFrame(ectx, job, liveFramePost{Run: r.ID, End: true})
	}
	tb, err := host.attach(ctx, r.Tab)
	if err != nil {
		end()
		return err
	}
	f := newFeed(gw, tb, r.ID)
	// The page the keeper holds at its end is let go of, a dialog of ours
	// on it answered first: nobody could answer it after (dialog.go).
	defer func() {
		if t := f.page(); t != nil {
			dismissDialog(context.Background(), t)
			t.detach(context.Background())
		}
	}()
	f.job, f.waiting = job, r.Checkpoint.Waiting
	waiting := f.waiting
	f.ready = func(ctx context.Context) bool {
		t := f.page()
		if t == nil {
			return false
		}
		pg, err := observe(ctx, t)
		return err == nil && r.movedOn(pg)
	}
	// "Open on this computer": the window comes forward, and the keeper
	// lets go of the page while the person works there (below).
	f.onFocus = func(ctx context.Context) {
		_ = host.forward(ctx, r.Tab)
		f.mu.Lock()
		f.wantHandsOff = true
		f.mu.Unlock()
	}
	// The owner's input runs as it arrives (inputs), beside the frames.
	go f.inputs()
	defer f.stopOnce.Do(func() { close(f.stop) })
	var last [32]byte
	failures := 0
	for {
		if !store.holdsJob(job, runID) {
			logKeep("ended: another run's view took the job")
			return nil
		}
		if stopped(store, runID) || f.isClosed() || !stillWaiting(store, runID) {
			logKeep("ended: resumed or closed")
			end()
			return nil
		}
		// Hands off (QA 2026-10-19: sign-in pages crashed while this
		// component was attached to them, and went through once nothing
		// was): while the person works in the browser window, nothing of
		// this component is on the page. The view keeps its last picture,
		// the address says when it looks done, and the person's next
		// action in the room's view takes the page back.
		goOff, comeBack := f.requests()
		if t := f.page(); t != nil && goOff && dialogOf(t) == nil {
			t.detach(ctx)
			f.setPage(nil)
			f.mu.Lock()
			f.waiting, f.posted = handsOffWaiting, time.Time{}
			f.mu.Unlock()
			logKeep("hands off: the person works in the browser window")
		}
		if f.page() == nil {
			if comeBack {
				nt, err := host.attach(ctx, r.Tab)
				if errors.Is(err, errTabGone) {
					end()
					return errTabGone
				}
				if err == nil {
					f.setPage(nt)
					f.mu.Lock()
					f.waiting, f.posted = waiting, time.Time{}
					f.mu.Unlock()
					logKeep("the room's view took the page back")
					continue
				}
			}
			tctx, cancel := context.WithTimeout(ctx, keepPostFor)
			u, err := host.url(tctx, r.Tab)
			if errors.Is(err, errTabGone) {
				cancel()
				end()
				return errTabGone
			}
			if err == nil {
				f.mu.Lock()
				if ready := r.movedOnURL(u); ready != f.isReady {
					f.isReady, f.posted = ready, time.Time{}
				}
				f.mu.Unlock()
			}
			every, ok := f.tick(tctx, &last)
			cancel()
			if !ok {
				logKeep("ended: the relay refuses the view (%s)", f.refusal())
				return nil
			}
			if !waitStop(ctx, store, runID, min(every, handsOffLook), f.poke) {
				end()
				return ctx.Err()
			}
			continue
		}
		tb := f.page()
		tctx, cancel := context.WithTimeout(ctx, keepPostFor)
		var state string
		switch err := evaluate(tctx, tb, "document.readyState", &state); {
		case err == nil, errors.Is(err, errDialogOpen), errors.Is(err, errTabCrashed):
			failures = 0 // a dialog or a crash is reported as such
		default:
			failures++
		}
		if failures >= keepFailures {
			// A page that does not answer (a dialog the picture cannot
			// show, a page that hangs) is not a closed tab: the view stays,
			// with its last picture and the owner's input (focus brings the
			// window forward), until the tab is gone from the browser.
			probe, err := host.attach(tctx, r.Tab)
			if errors.Is(err, errTabGone) {
				cancel()
				end()
				return errTabGone
			}
			if err == nil {
				probe.detach(tctx)
			}
			if failures == keepFailures {
				logKeep("the page does not answer; holding the view while the tab is open")
			}
		}
		every, ok := f.tick(tctx, &last)
		cancel()
		if !ok {
			logKeep("ended: the relay refuses the view (%s)", f.refusal())
			return nil // the session is gone, and the view with it
		}
		if !waitStop(ctx, store, runID, every, f.poke) {
			end()
			return ctx.Err()
		}
	}
}

// stopped reports a resuming call's stop request.
func stopped(store runStore, runID string) bool {
	_, err := os.Stat(store.keepStop(runID))
	return err == nil
}

// logKeep writes one line of the keeper's log (<run>.keep.log).
func logKeep(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format(time.RFC3339)+" "+format+"\n", args...)
}

// stillWaiting reports whether the run is still paused for a person (a
// resume that raced the keeper's start has moved it on).
func stillWaiting(store runStore, runID string) bool {
	r, err := store.load(runID)
	return err == nil && r.Checkpoint != nil && r.Checkpoint.Kind == cpNeedsHuman
}

// waitStop waits d, looking for a stop request meanwhile, or less when
// input just ran (poke: the page is looked at liveAfterInput after it);
// false when ctx ended.
func waitStop(ctx context.Context, store runStore, runID string, d time.Duration, poke <-chan struct{}) bool {
	until := time.Now().Add(d)
	for time.Now().Before(until) && !stopped(store, runID) {
		select {
		case <-ctx.Done():
			return false
		case <-poke:
			return sleepCtx(ctx, liveAfterInput) == nil
		case <-time.After(min(100*time.Millisecond, time.Until(until))):
		}
	}
	return true
}

// isClosed reports whether the owner closed the view from the room.
func (f *liveFeed) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// movedOn reports whether the page a needs_human pause waited on moved on:
// back on one of the run's sites, off the page it paused on, with no secret
// field left. The room then offers the person to let the agent continue.
func (r *runState) movedOn(pg *page) bool {
	return r.movedOnURL(pg.URL) && !hasSecret(pg)
}

// movedOnURL is movedOn by the address alone (a keeper that let go of the
// page sees nothing more).
func (r *runState) movedOnURL(u string) bool {
	return u != r.PausedURL && siteOf(u) != "" && r.onSite(u)
}
