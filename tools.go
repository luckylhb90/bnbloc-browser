package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/luckylhb90/bnbloc/component"
	"github.com/luckylhb90/bnbloc/core"
)

// The agent's two tools (§2.3). Their wording lives here only (the
// package lists the names).

// tools is the server: the relay, the host and the runs.
type tools struct {
	gw     *relay
	runner *runner
	// keeper starts the keeper of a run paused for a person, under the
	// pausing turn's job (keep.go); nil keeps no view after the call.
	keeper func(run, job string) error
	// workspace is the agent's workspace, where a run's replay is written
	// (replay.go); "" writes none.
	workspace string
}

// errNotRequester: a run is resumed only by the turns of the person it
// acts for.
var errNotRequester = errors.New("this run acts for someone else; only their turns use it")

// waitingSignIn answers a run of the requester that waits for them to sign
// in to site (needs_human), its view shown again under this turn; nil when
// none does. A run whose tab is gone is dropped; one another call drives
// is not waiting.
func (t *tools) waitingSignIn(ctx context.Context, site string, sess component.RelaySession) any {
	for _, id := range t.runner.store.ids() {
		r, err := t.runner.store.load(id)
		if err != nil || r.Checkpoint == nil || r.Checkpoint.Kind != cpNeedsHuman || r.Requester != sess.Requester || !contains(r.Sites, site) {
			continue
		}
		unlock, r, rerr := t.lockRun(id)
		if rerr != nil {
			continue
		}
		// One session on the page at a time, the keeper's or this one's: a
		// session leaving clears the run's device for the others (device.go).
		t.runner.store.stopKeeper(r.ID)
		tb, err := t.runner.host.attach(ctx, r.Tab, r.Device)
		if errors.Is(err, errTabGone) {
			t.runner.store.remove(r.ID)
			unlock()
			continue
		}
		var res any
		if err == nil {
			res = t.result(ctx, r, tb, r.Checkpoint, "this run already waits for the person to sign in to "+site+" (shown in the room): once they say they are done, go on with browser_view; a second tab would start a second sign-in that undoes this one", false)
			tb.detach(context.Background())
		}
		if t.keeper != nil {
			_ = t.keeper(r.ID, sess.JobID)
		}
		unlock()
		if res != nil {
			return res
		}
	}
	return nil
}

// stop ends run id, or ("" ) every run of the requester in the session's
// room: its keeper ends its view in the room, its tab closes, and the run
// is forgotten. A run another call drives is left (reported busy). With
// gif, each run's pictures become its GIF guide first (gif.go).
func (t *tools) stop(ctx context.Context, id, procedure, purpose string, gif bool) (any, *rpcError) {
	sess, err := t.gw.session(ctx)
	if err != nil {
		return nil, toolFailure(err)
	}
	if procedure != "" && id == "" {
		return nil, &rpcError{Code: -32602, Message: "procedure: name the run to save"}
	}
	draft := ""
	ids := []string{id}
	if id == "" {
		ids = t.runner.store.ids()
	}
	var stopped, busy, gifs, noGIF []string
	for _, rid := range ids {
		r, err := t.runner.store.load(rid)
		switch {
		case err != nil && id != "":
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		case err != nil:
			continue
		case r.Requester != sess.Requester && id != "":
			return nil, toolFailure(errNotRequester)
		case r.Requester != sess.Requester || r.RoomID != sess.RoomID:
			continue
		}
		unlock, r, rerr := t.lockRun(rid)
		if rerr != nil {
			busy = append(busy, rid)
			continue
		}
		if procedure != "" {
			// Saved before the run goes: a wrong name leaves it open.
			if draft, err = t.writeProcedure(r, procedure, purpose); err != nil {
				unlock()
				return nil, &rpcError{Code: -32602, Message: err.Error()}
			}
		}
		if gif {
			// Made before the run goes: its pictures go with it.
			if path, err := t.writeGIF(r); err == nil {
				gifs = append(gifs, path)
				t.writeReplay(r)
			} else {
				noGIF = append(noGIF, r.ID+": "+err.Error())
			}
		}
		t.runner.store.stopKeeper(r.ID)
		if err := t.runner.host.close(ctx, r.Tab); err != nil && !errors.Is(err, errTabGone) {
			unlock()
			return nil, toolFailure(err)
		}
		t.runner.store.remove(r.ID)
		unlock()
		stopped = append(stopped, r.ID)
	}
	head := map[string]any{"stopped": stopped, "note": "their tabs are closed and their views gone from the room"}
	if draft != "" {
		head["procedure"] = draft
		head["note"] = head["note"].(string) + "; the steps are a skill pack draft in " + draft + ": tell the person they can save it from the turn's files (Save as skill pack)"
	}
	if len(gifs) > 0 {
		head["gif"] = gifs
		head["note"] = head["note"].(string) + "; the GIF guide is in " + strings.Join(gifs, ", ") + ", shared with the turn's files: point the person to it"
	}
	if len(noGIF) > 0 {
		head["noGif"] = noGIF
	}
	if len(busy) > 0 {
		head["busy"] = busy
		head["note"] = "busy runs are in another call and were not stopped; " + head["note"].(string)
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": mustJSON(head)}}}, nil
}

// defaultThreshold is the irreversibility pause (decided 2026-10-17).
const defaultThreshold = 0.3

// consentHolds is the §2.5 rule (revised 2026-10-19): a confirmation counts
// in a turn the run's requester opened with their own message, before or
// after the pause, so a person who said "do all fifteen, don't ask me" is
// not asked fifteen times; the agent confirms only what their words
// cover. A turn a schedule or another agent opened never confirms, and a
// page cannot open a turn. It answers why not, "" when it holds.
func consentHolds(sess component.RelaySession, r *runState) string {
	switch {
	case sess.Requester != r.Requester:
		return "still waiting: only " + r.Requester + " can agree to this"
	case !sess.ByRequester:
		return "still waiting: only " + r.Requester + "'s own message can agree to this, not a scheduled or delegated turn. Ask them in the room; go on in the turn their answer opens"
	}
	return ""
}

// lockRun loads a run and holds it for this call (one call per run at a
// time, across sessions).
func (t *tools) lockRun(id string) (func(), *runState, *rpcError) {
	r, err := t.runner.store.load(strings.TrimSpace(id))
	if err != nil {
		return nil, nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	unlock, ok, err := tryLockFile(t.runner.store.path(r.ID) + ".lock")
	if err != nil {
		return nil, nil, toolFailure(err)
	}
	if !ok {
		return nil, nil, &rpcError{Code: -32000, Message: "the run is busy in another call; try again when it returns"}
	}
	// Re-read under the lock: the holder may have just saved.
	if r, err = t.runner.store.load(r.ID); err != nil {
		unlock()
		return nil, nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	return unlock, r, nil
}

// result renders a checkpoint with the page: the run's own fields as JSON
// and the page's text and fields fenced as the site's data. Only a look
// (withShot) adds a screenshot: an image costs the agent far more than
// the text, which usually says enough.
func (t *tools) result(ctx context.Context, r *runState, tb tabHandle, cp *checkpoint, note string, withShot bool) any {
	return t.resultSince(ctx, r, tb, nil, cp, note, withShot, nil)
}

// resultSince is result for pg (nil: the page read now), answering only
// what changed since the page was since, when it is the same page and the
// change is small (view.go).
func (t *tools) resultSince(ctx context.Context, r *runState, tb tabHandle, pg *page, cp *checkpoint, note string, withShot bool, since *page) any {
	var img string
	if tb != nil {
		if pg == nil {
			pg, _ = observe(ctx, tb)
		}
		if withShot {
			img, _ = screenshot(ctx, tb)
		}
	}
	return checkpointResult(r, cp, &pageView{page: pg, shot: img, since: since}, note)
}

type pageView struct {
	page *page
	shot string
	// since is the page before the call's actions (nil: answer it whole).
	since *page
}

// checkpointResult is a tool result: the checkpoint (ours), the page
// (theirs, fenced) and its screenshot.
func checkpointResult(r *runState, cp *checkpoint, pv *pageView, note string) any {
	head := map[string]any{}
	if r != nil {
		head["run"] = r.ID
		head["steps"] = len(r.History)
		head["sites"] = r.Sites
		if _, ok := devices[r.Device]; ok {
			head["device"] = r.Device
		}
	}
	if cp != nil {
		head["checkpoint"] = cp.Kind
		head["reason"] = cp.Reason
		if cp.Probability > 0 {
			head["irreversibleProbability"] = cp.Probability
		}
		if cp.Site != "" {
			head["site"] = cp.Site
		}
		if len(cp.Artifacts) > 0 {
			head["files"] = cp.Artifacts
		}
	}
	if note != "" {
		head["note"] = note
	}
	text := mustJSON(head)
	var site strings.Builder
	if cp != nil && cp.Dialog != "" {
		site.WriteString("The page asks: " + cp.Dialog + "\n\n")
	}
	if cp != nil && cp.Element != nil {
		site.WriteString("The element: " + cp.Element.Label)
		if cp.Element.Href != "" {
			site.WriteString(" → " + cp.Element.Href)
		}
		site.WriteString("\n\n")
	}
	content := []any{}
	changes, small := pageChanges{}, false
	if pv != nil && pv.page != nil && samePage(pv.since, pv.page) {
		changes, small = changesOf(pv.since, pv.page)
	}
	if small {
		writeChanges(&site, pv.page, changes)
	} else if pv != nil && pv.page != nil {
		site.WriteString("Page: " + pv.page.Title + "\n" + pv.page.URL + "\n\n" + pv.page.Text)
		if ctrls := controlsOf(pv.page); len(ctrls) > 0 {
			site.WriteString("\n\nControls (act on one by its number):\n")
			for _, c := range ctrls {
				site.WriteString(c.line + "\n")
			}
			if pv.page.OmittedActions > 0 {
				site.WriteString(fmt.Sprintf("(%d more controls not listed; scroll or browser_find)\n", pv.page.OmittedActions))
			}
		}
	}
	if site.Len() > 0 {
		text += "\n\n" + component.Fence("the web page"+siteSuffix(pv), site.String())
	}
	content = append(content, map[string]any{"type": "text", "text": text})
	if pv != nil && pv.shot != "" {
		if _, err := base64.StdEncoding.DecodeString(pv.shot); err == nil {
			content = append(content, map[string]any{"type": "image", "mimeType": "image/jpeg", "data": pv.shot})
		}
	}
	return map[string]any{"content": content}
}

func quoteValue(v string) string {
	if v == "" {
		return "(empty)"
	}
	return strconv.Quote(v)
}

func siteSuffix(pv *pageView) string {
	if pv == nil || pv.page == nil {
		return ""
	}
	if s := siteOf(pv.page.URL); s != "" {
		return " at " + s
	}
	return ""
}

// workspaceFile resolves a path inside the working directory (the agent's
// workspace) to an absolute path of an existing file.
func workspaceFile(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("still waiting: name the file to upload (a path inside your workspace)")
	}
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	abs := rel
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, rel)
	}
	abs = filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("no file %s in your workspace", rel)
	}
	rootResolved, _ := filepath.EvalSymlinks(root)
	inside, err := filepath.Rel(rootResolved, resolved)
	if _, ok := core.CleanRelPath(filepath.ToSlash(inside)); err != nil || !ok {
		return "", fmt.Errorf("%s is outside your workspace", rel)
	}
	if st, err := os.Stat(resolved); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", rel)
	}
	return resolved, nil
}

// upload sets the held file input to path (DOM.setFileInputFiles on the
// observed node), when the page is still the one decided on. The act that
// asked records the step.
func (rn *runner) upload(ctx context.Context, p browserPage, ps *pendingStep, path string) error {
	if ps == nil || ps.Action.Node == nil {
		return errStale
	}
	pg, err := observe(ctx, p)
	if err != nil {
		return err
	}
	if pg.Fingerprint != ps.Fingerprint {
		return errStale
	}
	var obj struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	expr := fmt.Sprintf("window.__bnblocBrowse?.nodes.get(%d)", *ps.Action.Node)
	if err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expr}, &obj); err != nil || obj.Result.ObjectID == "" {
		return errStale
	}
	return p.call(ctx, "DOM.setFileInputFiles", map[string]any{"files": []string{path}, "objectId": obj.Result.ObjectID}, nil)
}

func invalidArgs(err error) *rpcError {
	return &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
}

// toolFailure is a failed call the agent reads.
func toolFailure(err error) *rpcError {
	return &rpcError{Code: -32000, Message: err.Error()}
}
