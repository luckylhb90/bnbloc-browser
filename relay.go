package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/luckylhb90/bnbloc/internal/wire"
)

// relay is the session's room relay (the bridge's loopback address and the
// session's bearer): the decision model and the bound turn's facts. The
// relay names the room and the job; the tool never does.
type relay struct{ base, token string }

// relayError is a gateway refusal relayed back: its status and message.
type relayError struct {
	Status  int
	Message string
}

func (e *relayError) Error() string {
	return fmt.Sprintf("the gateway answered %d: %s", e.Status, e.Message)
}

// systemOne asks the decision model one SystemOne request.
func (g *relay) systemOne(ctx context.Context, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	out, err := g.do(ctx, http.MethodPost, "/v1/systemone", raw)
	var re *relayError
	if errors.As(err, &re) && re.Status == http.StatusConflict {
		return nil, fmt.Errorf("%w (%s)", errDecisionsOff, re.Message)
	}
	return out, err
}

// session reads the bound turn (wire.RoomRelaySession).
func (g *relay) session(ctx context.Context) (wire.RoomRelaySession, error) {
	var s wire.RoomRelaySession
	raw, err := g.do(ctx, http.MethodGet, wire.RoomRelaySessionPath, nil)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, errors.New("the room relay's session answer could not be read")
	}
	return s, nil
}

func (g *relay) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the room relay: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &relayError{Status: resp.StatusCode, Message: errorText(data)}
	}
	return data, nil
}

// errorText is a reply's error message ({"error": "..."} or the body).
func errorText(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
