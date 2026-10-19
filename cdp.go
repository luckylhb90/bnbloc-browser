package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The Chrome DevTools Protocol over one browser-level WebSocket, sessions
// flattened (Target.attachToTarget flatten): every call names the session
// of the tab it drives. No automation library: requests, responses and the
// few events a run needs.

// cdpConn is one connection to a browser.
type cdpConn struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	nextID int64
	wait   map[int64]chan cdpReply
	// events receives every event (method, session, params); a reader drops
	// what nobody consumes.
	events chan cdpEvent
	closed chan struct{}
	err    error
	// dialogs is each session's open dialog (dialog.go); crashed the
	// sessions whose page crashed (crash.go).
	dialogs map[string]*dialogState
	crashed map[string]bool
}

type cdpReply struct {
	Result json.RawMessage
	Err    *cdpError
}

// cdpError is a protocol error answer.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("cdp %d: %s", e.Code, e.Message) }

type cdpEvent struct {
	Method  string
	Session string
	Params  json.RawMessage
}

// dialCDP connects to the browser at base: an http(s) DevTools address
// (its /json/version names the browser's WebSocket) or a ws:// URL.
func dialCDP(ctx context.Context, base string) (*cdpConn, error) {
	wsURL := strings.TrimSpace(base)
	if strings.HasPrefix(wsURL, "http://") || strings.HasPrefix(wsURL, "https://") {
		u, err := browserWebSocketURL(ctx, wsURL)
		if err != nil {
			return nil, err
		}
		wsURL = u
	}
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to the browser: %w", err)
	}
	// A screenshot or a page's snapshot is one message; neither is bounded
	// by the library's 32 KiB default.
	ws.SetReadLimit(-1)
	c := &cdpConn{ws: ws, wait: map[int64]chan cdpReply{}, events: make(chan cdpEvent, 256), closed: make(chan struct{})}
	go c.read()
	return c, nil
}

// browserWebSocketURL reads the browser's WebSocket from /json/version.
func browserWebSocketURL(ctx context.Context, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach the browser: %w", err)
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(raw, &v); err != nil || v.WebSocketDebuggerURL == "" {
		return "", errors.New("the browser did not name its DevTools socket")
	}
	return v.WebSocketDebuggerURL, nil
}

// read delivers responses to their callers and events to the queue until
// the socket closes.
func (c *cdpConn) read() {
	defer close(c.closed)
	for {
		_, raw, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.err = err
			for id, ch := range c.wait {
				ch <- cdpReply{Err: &cdpError{Code: -1, Message: "the browser connection closed"}}
				delete(c.wait, id)
			}
			c.mu.Unlock()
			return
		}
		var m struct {
			ID        int64           `json:"id"`
			Result    json.RawMessage `json:"result"`
			Error     *cdpError       `json:"error"`
			Method    string          `json:"method"`
			SessionID string          `json:"sessionId"`
			Params    json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.wait[m.ID]
			delete(c.wait, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- cdpReply{Result: m.Result, Err: m.Error}
			}
			continue
		}
		c.noteDialog(m.SessionID, m.Method, m.Params)
		c.noteCrash(m.SessionID, m.Method)
		select {
		case c.events <- cdpEvent{Method: m.Method, Session: m.SessionID, Params: m.Params}:
		default: // nobody is reading: the run polls what it needs
		}
	}
}

// call sends one method (session "" = the browser) and waits for its
// answer; out, when set, receives the result.
func (c *cdpConn) call(ctx context.Context, session, method string, params any, out any) error {
	if err := c.crashGate(session, method); err != nil {
		return err
	}
	opened, err := c.dialogGate(session, method)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return errors.New("the browser connection closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan cdpReply, 1)
	c.wait[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := c.ws.Write(ctx, websocket.MessageText, raw); err != nil {
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		return fmt.Errorf("send %s: %w", method, err)
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return r.Err
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-opened:
		// The page opened a dialog: it answers nothing until someone
		// answers that (dialog.go).
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		return errDialogOpen
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// drain returns the queued events of one session (or the browser's, "")
// whose method has prefix, dropping the rest it reads.
func (c *cdpConn) drain(session, prefix string) []cdpEvent {
	var out []cdpEvent
	for {
		select {
		case ev := <-c.events:
			if (ev.Session == session || session == "*") && strings.HasPrefix(ev.Method, prefix) {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

// close ends the connection.
func (c *cdpConn) close() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
	select {
	case <-c.closed:
	case <-time.After(time.Second):
	}
}

// tab is one page target the run drives, attached on a connection.
type tab struct {
	conn     *cdpConn
	targetID string
	session  string
}

// openTab creates a run's tab on a blank start, attached and ready. With
// behind, in a window of its own opened behind the member's other windows,
// so a run never takes their screen or focus (§2.8: people watch it in the
// room's live view, and "Open in the browser window" brings it forward).
// Not minimized: Chrome stops painting a minimized window, and its capture
// (the live view's frames) never answers. A browser that cannot open such
// a window gets a background tab; a headless one has no screen to take.
func openTab(ctx context.Context, c *cdpConn, behind bool) (*tab, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	err := errors.New("headless")
	if behind {
		err = c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "newWindow": true, "background": true}, &created)
	}
	if err != nil {
		err = c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &created)
	}
	if err != nil {
		return nil, fmt.Errorf("open a tab: %w", err)
	}
	return attachTab(ctx, c, created.TargetID)
}

// attachTab attaches to an existing tab; errTabGone when it is closed.
func attachTab(ctx context.Context, c *cdpConn, targetID string) (*tab, error) {
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := c.call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
		return nil, err
	}
	found := false
	for _, t := range targets.TargetInfos {
		if t.TargetID == targetID && t.Type == "page" {
			found = true
		}
	}
	if !found {
		return nil, errTabGone
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &att); err != nil {
		return nil, fmt.Errorf("attach to the tab: %w", err)
	}
	t := &tab{conn: c, targetID: targetID, session: att.SessionID}
	// The page's crash is reported (crash.go); no focus emulation: no
	// more of the browser is changed than the run needs.
	_ = t.call(ctx, "Inspector.enable", nil, nil)
	_ = t.call(ctx, "Page.enable", nil, nil)
	// A page laid out far smaller than its window is sized anew (live.go).
	t.fitViewport(ctx)
	return t, nil
}

// errTabGone: the run's tab is closed.
var errTabGone = errors.New("the run's tab is closed")

// target is the tab's CDP target id (a run's key to its tab).
func (t *tab) target() string { return t.targetID }

func (t *tab) call(ctx context.Context, method string, params any, out any) error {
	return t.conn.call(ctx, t.session, method, params, out)
}

// detach releases the tab (it stays open in the window).
func (t *tab) detach(ctx context.Context) {
	_ = t.conn.call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": t.session}, nil)
}
