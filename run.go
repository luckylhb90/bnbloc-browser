package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/luckylhb90/bnbloc/component"
	"golang.org/x/net/publicsuffix"
)

// A run: the requester's tab in a room, which the agent drives one action
// at a time (agent.go; §2.3 of bnbloc
// docs/prd/2026-10-17-browser-component.md). A run waits at a checkpoint
// (§2.4) when a step must not go on without someone: a sign-in, a
// confirmation, a page's question, a download. It never ends for time; it
// lives as long as its tab.

// Checkpoints (§2.4).
const (
	cpDone              = "done"
	cpBlocked           = "blocked"
	cpNeedsInput        = "needs_input"
	cpNeedsConfirmation = "needs_confirmation"
	cpNeedsHuman        = "needs_human"
	cpLeavingSite       = "leaving_site"
	cpUpload            = "upload"
	cpDownload          = "download"
	cpRunning           = "running"
)

// runState is one run, kept in the state directory by run id.
type runState struct {
	ID        string `json:"id"`
	Tab       string `json:"tab"` // the CDP target id
	Goal      string `json:"goal"`
	Requester string `json:"requester"`
	Owner     string `json:"owner"`
	RoomID    string `json:"roomId"`
	// Sites are the registrable domains the run may act on (§2.4
	// leaving_site): the start URL's, plus each a requester allowed.
	Sites []string `json:"sites"`
	// Threshold is the irreversibility predicate's pause threshold (§2.5).
	Threshold float64 `json:"threshold"`
	// Device is how the run's tab shows pages: phone, tablet, or desktop
	// ("" too); every session that drives or shows the tab emulates it
	// (device.go).
	Device string `json:"device,omitempty"`
	// Checkpoint is where the run waits ("" while it runs).
	Checkpoint *checkpoint `json:"checkpoint,omitempty"`
	// PausedJob is that turn's job: a needs_human pause keeps its live
	// view under it after the turn (keep.go).
	PausedJob string `json:"pausedJob,omitempty"`
	// PausedURL is the page the run paused on.
	PausedURL string `json:"pausedUrl,omitempty"`
	// AcceptDialog is the page's question the person agreed to (a
	// confirmed repeat of the action a dialog's checkpoint held): the run accepts the next
	// dialog asking it, with DialogText for a prompt.
	AcceptDialog string          `json:"acceptDialog,omitempty"`
	DialogText   string          `json:"dialogText,omitempty"`
	History      []historyRecord `json:"history"`
	Decisions    int             `json:"decisions"`
	StartedAtMs  int64           `json:"startedAtMs"`
}

// historyRecord is one executed step.
type historyRecord struct {
	Step        int     `json:"step"`
	Operation   string  `json:"operation"`
	Action      string  `json:"action"`
	Kind        string  `json:"kind"`
	Text        string  `json:"text,omitempty"`
	URL         string  `json:"url"`
	PageChanged *bool   `json:"pageChanged,omitempty"`
	Confidence  float64 `json:"confidence"`
	AtMs        int64   `json:"atMs"`
	// Frame: the page after the step is pictured (replay.go); SignIn: it
	// was a sign-in, never pictured (the replay says so).
	Frame  bool `json:"frame,omitempty"`
	SignIn bool `json:"signIn,omitempty"`
}

// checkpoint is a pause: what it is, what it is about, and what the agent
// does next.
type checkpoint struct {
	Kind string `json:"kind"`
	// Reason says why, in a sentence (page text never appears unfenced).
	Reason string `json:"reason"`
	// Field / Element is the control the checkpoint is about.
	Element *checkpointElement `json:"element,omitempty"`
	// Probability is the irreversibility predicate's (needs_confirmation).
	Probability float64 `json:"probability,omitempty"`
	// Site is the destination of leaving_site.
	Site string `json:"site,omitempty"`
	// Artifacts are the room files a download published.
	Artifacts []string `json:"artifacts,omitempty"`
	// Waiting is what the person does in the live view (needs_human),
	// in their words; the room shows it.
	Waiting string `json:"waiting,omitempty"`
	// Dialog is the question a page's dialog asked (page text: fenced
	// wherever it is shown to the agent).
	Dialog string `json:"dialog,omitempty"`
	// pending is the step decided before the pause, run when it is answered
	// (a confirmed click, a supplied value, an allowed site).
	Pending *pendingStep `json:"pending,omitempty"`
}

type checkpointElement struct {
	Label string `json:"label"`
	Role  string `json:"role,omitempty"`
	Href  string `json:"href,omitempty"`
}

// pendingStep is the decided step a checkpoint holds.
type pendingStep struct {
	Operation   string  `json:"operation"`
	Action      action  `json:"action"`
	Fingerprint string  `json:"fingerprint"`
	Confidence  float64 `json:"confidence"`
}

// newRunID is a random run id.
func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "brun_" + hex.EncodeToString(b[:])
}

// siteOf is the registrable domain of a URL ("" for none: about:, data:).
func siteOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return host
	}
	if site, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return site
	}
	return host
}

// onSite reports whether raw is on one of the run's sites (or is not a
// web address at all).
func (r *runState) onSite(raw string) bool {
	s := siteOf(raw)
	return s == "" || contains(r.Sites, s)
}

// store keeps runs in a directory: one JSON file per run, written
// atomically. A run survives the component's restart for as long as its tab
// exists.
type runStore struct{ dir string }

func (s runStore) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s runStore) load(id string) (*runState, error) {
	if !strings.HasPrefix(id, "brun_") || strings.ContainsAny(id, `/\.`) {
		return nil, fmt.Errorf("no run %q", id)
	}
	raw, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("no run %q", id)
	}
	var r runState
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("run %q is unreadable", id)
	}
	return &r, nil
}

func (s runStore) save(r *runState) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(r.ID) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(r.ID))
}

func (s runStore) remove(id string) {
	_ = os.Remove(s.path(id))
	_ = os.RemoveAll(s.framesDir(id))
}

// ids lists the stored runs.
func (s runStore) ids() []string {
	entries, _ := os.ReadDir(s.dir)
	var out []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && strings.HasPrefix(id, "brun_") {
			out = append(out, id)
		}
	}
	return out
}

// decider asks the decision model through the relay.
type decider interface {
	systemOne(ctx context.Context, body any) ([]byte, error)
}

// runner drives runs: the decision model, the host's tabs and the room
// (downloads become room files).
type runner struct {
	model string
	dec   decider
	host  browserHost
	store runStore
	// downloads is where a run's finished downloads land: downloads/ in
	// the agent's workspace (the tool's working directory), so the turn's
	// deliverable publishing makes each a room file.
	downloads string
	// budget is how long one tool call may work before it answers
	// `running` (the agent CLI's tool-call timeout, §7 finding 1).
	budget time.Duration
	now    func() time.Time
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// errDecisionsOff: the deployment or organization has no decision model.
var errDecisionsOff = errors.New("decisions are off here: the deployment has no decision model, or the organization turned AI off")

// answerDialog is the run's answer to the page's dialog while it works
// (dialog.go): an alert is accepted and recorded in the history; a dialog
// that asks for a decision (confirm, prompt, leave-page) is the person's,
// so the run dismisses it and pauses with the question, unless the person
// already agreed to that very question (AcceptDialog). nil goes on.
func (rn *runner) answerDialog(ctx context.Context, r *runState, p browserPage, d *component.LiveDialog) *checkpoint {
	dp, ok := p.(dialogPage)
	if !ok {
		return nil
	}
	if d.Kind == component.LiveDialogAlert {
		_ = dp.answerDialog(ctx, true, "")
		r.History = append(r.History, historyRecord{Step: len(r.History) + 1, Operation: "DIALOG", Action: "the page showed a message: " + d.Message, Kind: "dialog", AtMs: rn.now().UnixMilli()})
		return nil
	}
	question := dialogQuestion(d)
	if r.AcceptDialog != "" && r.AcceptDialog == question {
		text := r.DialogText
		r.AcceptDialog, r.DialogText = "", ""
		_ = dp.answerDialog(ctx, true, text)
		return nil
	}
	_ = dp.answerDialog(ctx, false, "")
	if d.Kind == component.LiveDialogPrompt {
		return &checkpoint{Kind: cpNeedsInput, Dialog: question, Reason: "the page asks for text before going on (its question is shown below); ask the person who asked you, then repeat the action with confirmed and their text"}
	}
	return &checkpoint{Kind: cpNeedsConfirmation, Dialog: question, Reason: "the page asked a question before going on (shown below); it was answered no for now. Ask the person who asked you, and repeat the action with confirmed in the turn their answer opens"}
}

// dialogQuestion is what a dialog asks (a leave-page dialog has no text of
// its own).
func dialogQuestion(d *component.LiveDialog) string {
	if d.Message == "" && d.Kind == component.LiveDialogBeforeUnload {
		return "Leave this page? Changes may not be saved."
	}
	return d.Message
}

// irreversible asks the §2.5 predicate about a chosen click, with its
// form/dialog scope as context.
func (rn *runner) irreversible(ctx context.Context, r *runState, p browserPage, a action) (float64, error) {
	scope := ""
	if a.Node != nil {
		if g, ok := r.guardScope(ctx, p, *a.Node); ok {
			scope = g
		}
	}
	raw, err := rn.dec.systemOne(ctx, irreversibleRequest(rn.model, r.Goal, a, scope))
	if err != nil {
		return 0, err
	}
	r.Decisions++
	return readIrreversible(raw)
}

// guardScope reads the text of a node's form/dialog/row scope (the guard's
// last field).
func (r *runState) guardScope(ctx context.Context, p browserPage, node int) (string, bool) {
	var g []json.RawMessage
	expr := fmt.Sprintf("(() => { const c=window.__bnblocBrowse; return c ? c.guard(c.nodes.get(%d)) : null; })()", node)
	if evaluate(ctx, p, expr, &g) != nil || len(g) == 0 {
		return "", false
	}
	var scope string
	if json.Unmarshal(g[len(g)-1], &scope) != nil {
		return "", false
	}
	if len(scope) > 2000 {
		scope = scope[:2000]
	}
	return scope, true
}

// What a needs_human pause asks of the person (checkpoint.Waiting).
const (
	waitSecret = "Type the password in the page, then let the agent continue"
	waitSignIn = "Finish signing in on the page, then let the agent continue"
)

// waitForPerson is a needs_human pause: the person does it in the live
// view in the room (or the browser window), and the run waits for them.
func waitForPerson(el *checkpointElement, waiting string) *checkpoint {
	return &checkpoint{Kind: cpNeedsHuman, Element: el, Waiting: waiting,
		Reason: "only the person can do this step (a sign-in, a password, a second factor); it never passes through you or the model. They do it in the live view in the room, which stays open for them; once they say they are done, go on with browser_view"}
}

// signInPage reports a page that asks someone to sign in: a secret field,
// or an address or title that names a sign-in (an identity provider's
// first step asks only for a login name).
func signInPage(pg *page) bool {
	if hasSecret(pg) {
		return true
	}
	return signInWords.MatchString(strings.ToLower(pg.URL + " " + pg.Title))
}

// signInWatch is the one rule for when a sign-in waits for the person
// (settle and every answer of a page, agent.go): a page off the run's
// sites that asks someone to sign in is theirs at once when it asks for a
// secret, else once it stayed idpSettle (a single sign-on the person
// already has passes through it by itself) or the caller's deadline came.
type signInWatch struct {
	url   string
	since time.Time
}

// at answers pg's pause (nil: none yet), and whether pg is an identity
// provider's page that may still pass through: look again.
func (w *signInWatch) at(r *runState, pg *page, deadline time.Time) (*checkpoint, bool) {
	if r.onSite(pg.URL) || !signInPage(pg) {
		return nil, false
	}
	if pg.URL != w.url {
		w.url, w.since = pg.URL, time.Now()
	}
	if hasSecret(pg) || time.Since(w.since) >= idpSettle || time.Now().After(deadline) {
		return waitForPerson(nil, waitSignIn), false
	}
	return nil, true
}

// pastSignIn reports whether a run paused for the person to sign in has
// a page past it: no longer a sign-in by signInWatch's rule (the person
// already had their time), and no secret field left to fill.
func (r *runState) pastSignIn(pg *page) bool {
	var w signInWatch
	cp, _ := w.at(r, pg, time.Time{})
	return cp == nil && !hasSecret(pg)
}

var signInWords = regexp.MustCompile(`(^|[^a-z])(login|log-in|signin|sign-in|sign_in|logon|sso|saml|oauth2?|oidc|openid|authorize|auth|idp|adfs|mfa|2fa|otp)([^a-z]|$)`)

// hasSecret reports a password or other secret field on the page.
func hasSecret(pg *page) bool {
	for _, a := range pg.Actions {
		if a.Kind == "secret" {
			return true
		}
	}
	return false
}

func pending(d decision, pg *page) *pendingStep {
	return &pendingStep{Operation: d.Operation, Action: d.Action, Fingerprint: pg.Fingerprint, Confidence: d.Confidence}
}

func elementOf(a action) *checkpointElement {
	return &checkpointElement{Label: a.Label, Role: a.Role, Href: a.Href}
}

// staleAfter is how long the files a run leaves (its locks, its keeper's
// log and stop file, its frames, a job's keeper file) stay after the run
// itself is gone: long past any call or keeper still using them.
const staleAfter = time.Hour

// sweep removes what runs that are gone left behind, untouched for
// staleAfter. A run that exists keeps everything.
func (s runStore) sweep(now time.Time) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	alive := map[string]bool{}
	for _, id := range s.ids() {
		alive[id] = true
	}
	for _, e := range entries {
		name := e.Name()
		run := ""
		switch {
		case strings.HasPrefix(name, "brun_"):
			run, _, _ = strings.Cut(name, ".")
		case strings.HasPrefix(name, "keeper-"):
			raw, err := os.ReadFile(filepath.Join(s.dir, name))
			if err != nil {
				continue
			}
			run = strings.TrimSpace(string(raw))
		default:
			continue
		}
		info, err := e.Info()
		if err != nil || alive[run] || now.Sub(info.ModTime()) < staleAfter {
			continue
		}
		_ = os.RemoveAll(filepath.Join(s.dir, name))
	}
}
