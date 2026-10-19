package main

import (
	"context"
	"errors"
	"strings"
)

// A page whose renderer crashed in the browser answers nothing, so a call
// to it would wait for good (QA 2026-10-19: sign-in pages crashed while
// this component was attached to them, never in the owner's own browser).
// The connection keeps each session's crash (Inspector.targetCrashed) and
// refuses calls to the page at once (errTabCrashed) until it is reloaded.

// errTabCrashed: the run's page crashed in the browser.
var errTabCrashed = errors.New("the page crashed in the browser")

// noteCrash keeps a session's crash (the read loop).
func (c *cdpConn) noteCrash(session, method string) {
	if method != "Inspector.targetCrashed" {
		return
	}
	c.mu.Lock()
	if c.crashed == nil {
		c.crashed = map[string]bool{}
	}
	c.crashed[session] = true
	c.mu.Unlock()
}

// crashGate refuses a call to a crashed page (errTabCrashed), but the ones
// that bring it back (a reload, a navigation) and the browser's own.
func (c *cdpConn) crashGate(session, method string) error {
	if session == "" || method == "Page.reload" || method == "Page.navigate" || strings.HasPrefix(method, "Target.") || strings.HasPrefix(method, "Browser.") || strings.HasPrefix(method, "Inspector.") {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.crashed[session] {
		return errTabCrashed
	}
	return nil
}

// isCrashed reports whether the tab's page crashed.
func (t *tab) isCrashed() bool {
	t.conn.mu.Lock()
	defer t.conn.mu.Unlock()
	return t.conn.crashed[t.session]
}

// reload reloads the tab's page (one that crashed comes back).
func (t *tab) reload(ctx context.Context) error {
	t.conn.mu.Lock()
	delete(t.conn.crashed, t.session)
	t.conn.mu.Unlock()
	return t.call(ctx, "Page.reload", nil, nil)
}

// crashPage is a page that reports and recovers from a crash (a tab).
type crashPage interface {
	isCrashed() bool
	reload(ctx context.Context) error
}

// crashedOf reports whether p's page crashed (false for a page that
// cannot tell).
func crashedOf(p browserPage) bool {
	cp, ok := p.(crashPage)
	return ok && cp.isCrashed()
}
