package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
//   - the relay refuses its posts (the agent's session was released).
//
// No clock ends it: it waits for as long as the view waits.

// keepFailures is how many failed looks at the tab in a row mean it is gone.
const keepFailures = 3

// keepPostFor bounds one look and post of the keeper.
const keepPostFor = 20 * time.Second

func (s runStore) keepLock(id string) string { return s.path(id) + ".keep.lock" }
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
	defer tb.detach(context.Background())
	f := newFeed(gw, tb, r.ID)
	f.job, f.waiting = job, r.Checkpoint.Waiting
	f.ready = func(ctx context.Context) bool {
		pg, err := observe(ctx, tb)
		return err == nil && r.movedOn(pg)
	}
	// The owner's input runs as it arrives (inputs), beside the frames.
	go f.inputs()
	defer f.stopOnce.Do(func() { close(f.stop) })
	var last [32]byte
	failures := 0
	for {
		if stopped(store, runID) || f.isClosed() || !stillWaiting(store, runID) {
			logKeep("ended: resumed or closed")
			end()
			return nil
		}
		tctx, cancel := context.WithTimeout(ctx, keepPostFor)
		var state string
		if evaluate(tctx, tb, "document.readyState", &state) != nil {
			failures++
		} else {
			failures = 0
		}
		if failures >= keepFailures {
			cancel()
			end()
			return errTabGone
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
	return pg.URL != r.PausedURL && siteOf(pg.URL) != "" && r.onSite(pg.URL) && !hasSecret(pg)
}
