// Command bnbloc-browser is the browser MCP component
// (bnbloc.mcp.package/v1, runtime stdio; docs/prd/2026-10-17-browser-
// component.md): a room agent works web pages in the browser of the
// machine it runs on, the owner's dedicated Chrome profile.
//
// It is an installable component, never a built-in. Its credentials are the
// room (mcp.credentials "room"): the bridge gives it the session's room
// relay (BNBLOC_ROOM_RELAY_URL, BNBLOC_ROOM_RELAY_TOKEN), through which
// each step's decision (POST /v1/systemone) acts as the requester of the
// turn being driven, and the relay's session route tells it who that is.
// It holds no gateway session and needs no configuration.
//
// Usage:
//
//	bnbloc-browser [flags]          the stdio MCP server (the agent spawns it)
//	bnbloc-browser launch [flags]   start the dedicated browser in the
//	                                foreground and publish its endpoint (the
//	                                desktop runs this as the package's sidecar)
//	bnbloc-browser keep [flags]     hold a paused run's live view after its
//	                                call (keep.go; the server starts it)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/luckylhb90/bnbloc/core/pkgmeta"
)

func main() {
	args := os.Args[1:]
	launch := len(args) > 0 && args[0] == "launch"
	keeping := len(args) > 0 && args[0] == "keep"
	if launch || keeping {
		args = args[1:]
	}
	fs := flag.NewFlagSet("bnbloc-browser", flag.ExitOnError)
	cfg := hostConfig{}
	fs.StringVar(&cfg.Endpoint, "endpoint", os.Getenv("BNBLOC_BROWSER_ENDPOINT"), "the browser's DevTools address (http://127.0.0.1:<port> or ws://…)")
	fs.StringVar(&cfg.EndpointFile, "endpoint-file", os.Getenv("BNBLOC_BROWSER_ENDPOINT_FILE"), "where the launcher publishes the endpoint")
	fs.StringVar(&cfg.Profile, "profile", os.Getenv("BNBLOC_BROWSER_PROFILE"), "the dedicated profile directory")
	fs.StringVar(&cfg.Chrome, "chrome", os.Getenv("BNBLOC_BROWSER_CHROME"), "the Chrome or Chromium executable")
	fs.BoolVar(&cfg.Headless, "headless", false, "run the browser headless (tests, a machine without a display)")
	budget := fs.Duration("call-budget", 45*time.Second, "how long one tool call works before it answers `running`")
	stateDir := fs.String("state-dir", os.Getenv("BNBLOC_BROWSER_STATE_DIR"), "where runs are kept")
	model := fs.String("model", "", "the decision model to ask (default: the provider's)")
	keepRun := fs.String("run", "", "keep: the paused run")
	keepJob := fs.String("job", "", "keep: the job its view is posted under")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if launch {
		if err := runLauncher(ctx, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "bnbloc-browser launch:", err)
			os.Exit(1)
		}
		return
	}
	gw := &relay{
		base:  strings.TrimRight(os.Getenv(pkgmeta.RoomRelayURLEnv), "/"),
		token: strings.TrimSpace(os.Getenv(pkgmeta.RoomRelayTokenEnv)),
	}
	if gw.base == "" || gw.token == "" {
		fmt.Fprintf(os.Stderr, "bnbloc-browser: %s and %s are required (the bridge sets them for a room-credential tool)\n", pkgmeta.RoomRelayURLEnv, pkgmeta.RoomRelayTokenEnv)
		os.Exit(1)
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(os.TempDir(), "bnbloc-browser", "runs")
	}
	store := runStore{dir: *stateDir}
	if keeping {
		if err := keep(ctx, gw, newLocalHost(cfg), store, *keepRun, *keepJob); err != nil {
			fmt.Fprintln(os.Stderr, "bnbloc-browser keep:", err)
			os.Exit(1)
		}
		return
	}
	cwd, _ := os.Getwd()
	t := &tools{gw: gw, runner: &runner{model: *model, dec: gw, host: newLocalHost(cfg), store: store,
		downloads: filepath.Join(cwd, "downloads"), budget: *budget, now: time.Now}, workspace: cwd}
	if exe, err := os.Executable(); err == nil {
		t.keeper = func(run, job string) error { return startKeeper(exe, args, store, run, job) }
	}
	if err := serve(ctx, t, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bnbloc-browser:", err)
		os.Exit(1)
	}
}

// serve reads newline-delimited JSON-RPC from in and writes every response
// to out, one JSON object per line, until in closes.
func serve(ctx context.Context, t *tools, in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(in)
	for {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read a request: %w", err)
		}
		if len(req.ID) == 0 {
			continue // a notification: accepted and dropped
		}
		result, rerr := t.dispatch(ctx, req.Method, req.Params)
		if rerr != nil {
			writeJSON(out, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": rerr})
			continue
		}
		writeJSON(out, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

// dispatch answers one method.
func (t *tools) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "bnbloc-browser", "version": "1"},
		}, nil
	case "notifications/initialized", "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": []any{toolOpen, toolView, toolFind, toolAct, toolClose}}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{Code: -32602, Message: "tools/call needs a tool name"}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage(`{}`)
		}
		switch p.Name {
		case "browser_open":
			return t.open(ctx, p.Arguments)
		case "browser_view":
			return t.view(ctx, p.Arguments)
		case "browser_find":
			return t.find(ctx, p.Arguments)
		case "browser_act":
			return t.act(ctx, p.Arguments)
		case "browser_close":
			var in struct {
				Run string `json:"run"`
			}
			if err := json.Unmarshal(p.Arguments, &in); err != nil {
				return nil, invalidArgs(err)
			}
			return t.stop(ctx, strings.TrimSpace(in.Run))
		default:
			return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
		}
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

// rpcError is one JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// writeJSON writes one JSON object and a newline.
func writeJSON(w io.Writer, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}

// mustJSON is v's indented JSON (a tool's text body).
func mustJSON(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(raw)
}
