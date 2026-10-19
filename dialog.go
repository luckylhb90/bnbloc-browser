package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/luckylhb90/bnbloc/component"
)

// A page's dialogs (alert, confirm, prompt, leave-page; bnbloc
// docs/prd/2026-10-17-browser-component.md §2.8). Verified on Chrome 146:
// while one is open the page runs no script, so an observation or a
// capture would wait for good; only the CDP session that had the Page
// domain enabled when it opened can answer it, and none can once that
// session is gone. So the connection keeps each session's open dialog,
// releases a call stuck behind one at once (errDialogOpen), and every
// process answers the dialogs it owns before it lets go of the tab.

// errDialogOpen: the page waits on its dialog.
var errDialogOpen = errors.New("the page waits on a dialog")

// dialogState is a session's open dialog and the signal that one opened.
type dialogState struct {
	open   *component.LiveDialog
	opened chan struct{} // closed when a dialog opens; replaced when it closes
}

// noteDialog keeps the dialog events of one session (the read loop).
func (c *cdpConn) noteDialog(session, method string, params json.RawMessage) {
	switch method {
	case "Page.javascriptDialogOpening":
		var p struct {
			Message       string `json:"message"`
			Type          string `json:"type"`
			DefaultPrompt string `json:"defaultPrompt"`
		}
		_ = json.Unmarshal(params, &p)
		c.mu.Lock()
		d := c.dialogLocked(session)
		if d.open == nil {
			close(d.opened)
		}
		d.open = &component.LiveDialog{Kind: p.Type, Message: p.Message, Default: p.DefaultPrompt}
		c.mu.Unlock()
	case "Page.javascriptDialogClosed":
		c.mu.Lock()
		if d := c.dialogs[session]; d != nil && d.open != nil {
			d.open, d.opened = nil, make(chan struct{})
		}
		c.mu.Unlock()
	case "Target.detachedFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(params, &p)
		c.mu.Lock()
		delete(c.dialogs, p.SessionID)
		c.mu.Unlock()
	}
}

// dialogLocked is session's state, made on first use. Callers hold mu.
func (c *cdpConn) dialogLocked(session string) *dialogState {
	if c.dialogs == nil {
		c.dialogs = map[string]*dialogState{}
	}
	d := c.dialogs[session]
	if d == nil {
		d = &dialogState{opened: make(chan struct{})}
		c.dialogs[session] = d
	}
	return d
}

// dialogGate answers whether a call of method on session may go to a page
// with a dialog open (errDialogOpen when not), and the signal that ends
// its wait when one opens (nil: nothing to wait for). Answering the
// dialog, and calls to the browser or about targets, always go.
func (c *cdpConn) dialogGate(session, method string) (<-chan struct{}, error) {
	if session == "" || method == "Page.handleJavaScriptDialog" || strings.HasPrefix(method, "Target.") || strings.HasPrefix(method, "Browser.") {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.dialogLocked(session)
	if d.open != nil {
		return nil, errDialogOpen
	}
	return d.opened, nil
}

// openDialog is the tab's open dialog (nil none).
func (t *tab) openDialog() *component.LiveDialog {
	t.conn.mu.Lock()
	defer t.conn.mu.Unlock()
	if d := t.conn.dialogs[t.session]; d != nil && d.open != nil {
		c := *d.open
		return &c
	}
	return nil
}

// answerDialog accepts or dismisses the tab's open dialog (text answers a
// prompt).
func (t *tab) answerDialog(ctx context.Context, accept bool, text string) error {
	params := map[string]any{"accept": accept}
	if accept && text != "" {
		params["promptText"] = text
	}
	return t.call(ctx, "Page.handleJavaScriptDialog", params, nil)
}

// dialogPage is a page that reports and answers its dialogs (a tab).
type dialogPage interface {
	openDialog() *component.LiveDialog
	answerDialog(ctx context.Context, accept bool, text string) error
}

// dialogOf is p's open dialog (nil none, or a page that cannot tell).
func dialogOf(p browserPage) *component.LiveDialog {
	if dp, ok := p.(dialogPage); ok {
		return dp.openDialog()
	}
	return nil
}

// dismissDialog dismisses p's open dialog, if any: a process does it
// before it lets go of the tab, since nobody could answer it after.
func dismissDialog(ctx context.Context, p browserPage) {
	if dp, ok := p.(dialogPage); ok && dp.openDialog() != nil {
		_ = dp.answerDialog(ctx, false, "")
	}
}
