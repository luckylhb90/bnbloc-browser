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

	"golang.org/x/net/publicsuffix"
)

// A run: one goal worked on in its own tab until a checkpoint (§2.4 of
// bnbloc docs/prd/2026-10-17-browser-component.md). The loop is a port of
// jev-ultrafast's agent.py (NOTICE): observe, decide (one SystemOne call:
// the operation and every operation's target), execute, observe again.
// Instead of the demo's exceptions every pause is a checkpoint the agent
// resumes with browse(run). A run never ends for time or a step count:
// it stops when the page stops changing (no progress) and waits at a
// checkpoint for as long as nobody answers.

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

// stallSteps is how many executed steps in a row may leave the page
// unchanged (a WAIT excepted) before the run is blocked: progress, never
// time, decides (jev_ultrafast/agent.py:153-158).
const stallSteps = 3

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
	// Values maps field descriptions to the text the agent supplied.
	Values map[string]string `json:"values,omitempty"`
	// Threshold is the irreversibility predicate's pause threshold (§2.5).
	Threshold float64 `json:"threshold"`
	// Checkpoint is where the run waits ("" while it runs).
	Checkpoint *checkpoint `json:"checkpoint,omitempty"`
	// PausedAtSeq is the message sequence of the turn the run paused in,
	// for a checkpoint only a newer turn of the requester may answer.
	PausedAtSeq int64 `json:"pausedAtSeq,omitempty"`
	// PausedJob is that turn's job: a needs_human pause keeps its live
	// view under it after the turn (keep.go).
	PausedJob string `json:"pausedJob,omitempty"`
	// PausedURL is the page the run paused on.
	PausedURL string          `json:"pausedUrl,omitempty"`
	History   []historyRecord `json:"history"`
	// Unchanged counts consecutive executed steps that changed nothing.
	Unchanged   int   `json:"unchanged"`
	Decisions   int   `json:"decisions"`
	StartedAtMs int64 `json:"startedAtMs"`
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

func (s runStore) remove(id string) { _ = os.Remove(s.path(id)) }

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
	// held answers who holds control of the call's browser from the room
	// ("" nobody; nil outside a call with a live view).
	held func() string
}

func (rn *runner) holder() string {
	if rn.held == nil {
		return ""
	}
	return rn.held()
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

// loop works the run from its current page until a checkpoint, the
// budget, or the context's end. p is the run's tab.
func (rn *runner) loop(ctx context.Context, r *runState, p browserPage) (*checkpoint, error) {
	deadline := rn.now().Add(rn.budget)
	for {
		if rn.now().After(deadline) {
			if who := rn.holder(); who != "" {
				return &checkpoint{Kind: cpRunning, Reason: who + " is driving the browser from the room; resume the run to keep going once they hand it back"}, nil
			}
			return &checkpoint{Kind: cpRunning, Reason: "still working; resume the run to keep going"}, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if who := rn.holder(); who != "" {
			// A person drives the browser from the room (§2.8): the run
			// takes no step until they hand control back.
			if err := sleepCtx(ctx, time.Second); err != nil {
				return nil, err
			}
			r.Unchanged = 0
			continue
		}
		if dl := rn.collectDownloads(r); dl != nil {
			return dl, nil
		}
		pg, err := observe(ctx, p)
		if err != nil {
			return nil, err
		}
		if posted, err := autoPost(ctx, p, pg); err != nil {
			return nil, err
		} else if posted {
			continue // the page's own hand-off went on: observe where it led
		}
		if !r.onSite(pg.URL) && signInPage(pg) {
			// A redirect to a sign-in elsewhere (an identity provider):
			// the person signs in, and the redirect brings the run back.
			return waitForPerson(nil, waitSignIn), nil
		}
		if !r.onSite(pg.URL) {
			return &checkpoint{Kind: cpLeavingSite, Site: siteOf(pg.URL), Reason: "the page is now on " + siteOf(pg.URL) + ", outside the run's sites; ask the person who asked you whether to continue there"}, nil
		}
		cp, err := rn.step(ctx, r, p, pg)
		if err != nil {
			if errors.Is(err, errStale) {
				continue // the page moved: observe again, act on nothing stale
			}
			return nil, err
		}
		if cp != nil {
			return cp, nil
		}
		if r.Unchanged >= stallSteps {
			return &checkpoint{Kind: cpBlocked, Reason: fmt.Sprintf("the last %d steps changed nothing on the page; read the page below and resume with a new instruction", stallSteps)}, nil
		}
	}
}

// step decides one step on pg and runs it, or answers the checkpoint it
// reaches.
func (rn *runner) step(ctx context.Context, r *runState, p browserPage, pg *page) (*checkpoint, error) {
	body, operations, targets, controls := stepRequest(rn.model, pg, r.goalText(), r.recent())
	raw, err := rn.dec.systemOne(ctx, body)
	if err != nil {
		return nil, err
	}
	r.Decisions++
	d, err := readDecision(raw, operations, targets, controls)
	if err != nil {
		return nil, err
	}
	switch d.Operation {
	case opDone:
		if ok, err := fresh(ctx, p, pg, nil); err != nil || !ok {
			return nil, errStale
		}
		return &checkpoint{Kind: cpDone, Reason: "the decision model judged the goal visibly met; check the page and fields below before you report it"}, nil
	case opBlocked:
		return &checkpoint{Kind: cpBlocked, Reason: "the decision model found no operation that makes progress here; read the page below and resume with a new instruction"}, nil
	case opUpload:
		return &checkpoint{Kind: cpUpload, Element: elementOf(d.Action), Reason: "the page asks for a file; uploading sends a room file to this site, so the person who asked you chooses it and agrees first", Pending: pending(d, pg)}, nil
	case opSecret:
		return waitForPerson(elementOf(d.Action), waitSecret), nil
	case opNeedsHuman:
		return waitForPerson(nil, waitSignIn), nil
	case opTypeText:
		text, ok := r.valueFor(d.Action)
		if !ok {
			return &checkpoint{Kind: cpNeedsInput, Element: elementOf(d.Action), Reason: "the next step types into this field and no value was given for it; resume with the text to type", Pending: pending(d, pg)}, nil
		}
		return nil, rn.execute(ctx, r, p, pg, d, text)
	case opClick:
		if d.Action.Href != "" && !r.onSite(d.Action.Href) {
			return &checkpoint{Kind: cpLeavingSite, Element: elementOf(d.Action), Site: siteOf(d.Action.Href), Reason: "the next click leads to " + siteOf(d.Action.Href) + ", outside the run's sites; ask the person who asked you whether to go there", Pending: pending(d, pg)}, nil
		}
		prob, err := rn.irreversible(ctx, r, p, d.Action)
		if err != nil {
			return nil, err
		}
		if prob >= r.Threshold {
			return &checkpoint{Kind: cpNeedsConfirmation, Element: elementOf(d.Action), Probability: prob, Reason: "the next click may commit something that cannot be undone; ask the person who asked you, and resume only after they answer in the room", Pending: pending(d, pg)}, nil
		}
	}
	return nil, rn.execute(ctx, r, p, pg, d, "")
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

// execute runs a decided step and records it.
func (rn *runner) execute(ctx context.Context, r *runState, p browserPage, pg *page, d decision, text string) error {
	if err := act(ctx, p, pg, d.Action, text); err != nil {
		if errors.Is(err, errUnconfirmed) {
			r.Unchanged = stallSteps // stop: a native select may have fired; never retried blindly
		}
		return err
	}
	rec := historyRecord{Step: len(r.History) + 1, Operation: d.Operation, Action: d.Action.Label, Kind: d.Action.Kind, Text: text, URL: pg.URL, Confidence: d.Confidence, AtMs: rn.now().UnixMilli()}
	after, err := observe(ctx, p)
	if err == nil {
		changed := after.Fingerprint != pg.Fingerprint
		rec.PageChanged = &changed
		switch {
		case changed:
			r.Unchanged = 0
		case d.Action.Kind != "wait":
			r.Unchanged++
		}
	}
	r.History = append(r.History, rec)
	return nil
}

// resumePending runs the step a checkpoint held, if the page it was
// decided on is still the page.
func (rn *runner) resumePending(ctx context.Context, r *runState, p browserPage, ps *pendingStep, text string) error {
	pg, err := observe(ctx, p)
	if err != nil {
		return err
	}
	if pg.Fingerprint != ps.Fingerprint {
		return errStale // decided on another page: the loop decides again
	}
	return rn.execute(ctx, r, p, pg, decision{Operation: ps.Operation, Action: ps.Action, Confidence: ps.Confidence}, text)
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
		Reason: "only the person can do this step (a sign-in, a password, a second factor); it never passes through you or the model. They do it in the live view in the room, which stays open for them; resume the run with reply once they say they are done"}
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

// goalText is the goal with the agent's supplied values named (never a
// selector: descriptions and text).
func (r *runState) goalText() string {
	if len(r.Values) == 0 {
		return r.Goal
	}
	var b strings.Builder
	b.WriteString(r.Goal)
	b.WriteString("\n\nValues to use:")
	for _, k := range sortedValueKeys(r.Values) {
		b.WriteString("\n- " + k + ": " + r.Values[k])
	}
	return b.String()
}

// valueFor finds the supplied text for a field: a value whose description
// matches the field's label (case-insensitive, either containing the
// other), or the only value when exactly one was given and one field is
// asked.
func (r *runState) valueFor(a action) (string, bool) {
	label := strings.ToLower(strings.TrimSpace(a.Label))
	for _, k := range sortedValueKeys(r.Values) {
		desc := strings.ToLower(strings.TrimSpace(k))
		if desc != "" && label != "" && (strings.Contains(label, desc) || strings.Contains(desc, label)) {
			return r.Values[k], true
		}
	}
	return "", false
}

func sortedValueKeys(m map[string]string) []string {
	out := keysOf(m)
	sortStrings(out)
	return out
}

// recent is the history the decision sees.
func (r *runState) recent() []historyEntry {
	out := make([]historyEntry, 0, len(r.History))
	for _, h := range r.History {
		out = append(out, historyEntry{Action: h.Action, Kind: h.Kind, Text: h.Text, PageChanged: h.PageChanged})
	}
	return out
}
