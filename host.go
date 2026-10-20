package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The browser host: the member's dedicated, visible Chrome profile
// (bnbloc docs/prd/2026-10-17-browser-component.md §2.2). The executor only
// speaks CDP to a loopback endpoint; where that endpoint comes from:
//
//  1. --endpoint / BNBLOC_BROWSER_ENDPOINT: an http://127.0.0.1:<port>
//     DevTools address or a ws:// URL, named by whoever runs the browser;
//  2. the endpoint file (--endpoint-file / BNBLOC_BROWSER_ENDPOINT_FILE,
//     default <temp>/bnbloc-browser/endpoint.json) that `bnbloc-browser
//     launch` writes: the desktop supervises that launcher as the
//     package's sidecar, so the browser lives outside any agent's
//     confinement and outlives sessions;
//  3. otherwise the component launches the same browser itself, detached
//     (a member running the bridge without the desktop).
//
// Every session's component attaches to the one browser; each run gets
// its own tab.

// hostConfig is where the browser is found or how it is started.
type hostConfig struct {
	Endpoint     string
	EndpointFile string
	Profile      string
	Chrome       string
	Headless     bool
}

// defaultEndpointFile is the launcher's endpoint file: a temp directory,
// which a confined agent's tools may read (cmd/bnbloc-vmexec's temp dirs).
func defaultEndpointFile() string {
	return filepath.Join(os.TempDir(), "bnbloc-browser", "endpoint.json")
}

// defaultProfile is the dedicated profile a self-launched browser uses
// (the desktop passes its own data directory's).
func defaultProfile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "bnbloc-browser", "profile")
}

// endpointRecord is the endpoint file's content.
type endpointRecord struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
	// Flags are the flags the browser was started with (flagsKey): a
	// browser an older version started runs without this one's.
	Flags string `json:"flags,omitempty"`
}

// flagsKey is the flags this version starts the browser with, but its
// profile.
func flagsKey(cfg hostConfig) string {
	var out []string
	for _, a := range chromeArgs(cfg) {
		if !strings.HasPrefix(a, "--user-data-dir=") {
			out = append(out, a)
		}
	}
	return strings.Join(out, " ")
}

// localHost connects to the member's browser, once per process.
type localHost struct {
	cfg hostConfig
	dl  string // the browser's download directory (downloads.go)

	mu   sync.Mutex
	conn *cdpConn
	// downloads tracks each download the browser announced: its frame and
	// suggested name, until it completes.
	downloads map[string]downloadStart
}

func newLocalHost(cfg hostConfig) *localHost {
	if cfg.EndpointFile == "" {
		cfg.EndpointFile = defaultEndpointFile()
	}
	if cfg.Profile == "" {
		cfg.Profile = defaultProfile()
	}
	return &localHost{cfg: cfg, dl: filepath.Join(os.TempDir(), "bnbloc-browser", "downloads"), downloads: map[string]downloadStart{}}
}

// connect answers the live connection, (re)establishing it.
func (h *localHost) connect(ctx context.Context) (*cdpConn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != nil {
		select {
		case <-h.conn.closed:
			h.conn = nil
		default:
			return h.conn, nil
		}
	}
	var c *cdpConn
	var err error
	switch {
	case h.cfg.Endpoint != "":
		c, err = dialCDP(ctx, h.cfg.Endpoint)
	default:
		if rec, ok := readEndpoint(h.cfg.EndpointFile); ok {
			c, err = dialCDP(ctx, rec.URL)
			if err == nil && rec.Flags != flagsKey(h.cfg) && h.closeOutdated(ctx, c, rec) {
				c = nil
			}
		}
		if c == nil {
			var url string
			url, err = h.launchDetached(ctx)
			if err == nil {
				c, err = dialCDP(ctx, url)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(h.dl, 0o700); err == nil {
		_ = c.call(ctx, "", "Browser.setDownloadBehavior", map[string]any{"behavior": "allowAndName", "downloadPath": h.dl, "eventsEnabled": true}, nil)
	}
	h.conn = c
	return c, nil
}

// closeOutdated closes a browser an older version started (its flags are
// not this version's: QA 2026-10-19, a browser started the day before kept
// running without the flags that keep background windows painting) when
// it shows no web page, and reports whether it did; the next launch starts
// it with this version's flags. A browser showing a web page (someone may
// be signing in) is used as it is.
func (h *localHost) closeOutdated(ctx context.Context, c *cdpConn, rec endpointRecord) bool {
	var targets struct {
		TargetInfos []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"targetInfos"`
	}
	if c.call(ctx, "", "Target.getTargets", nil, &targets) != nil {
		return false
	}
	for _, t := range targets.TargetInfos {
		if t.Type == "page" && (strings.HasPrefix(t.URL, "http://") || strings.HasPrefix(t.URL, "https://")) {
			return false
		}
	}
	// Closed only where a new one can start: a confined process (an
	// agent's sandbox) cannot, and would leave no browser at all (QA,
	// 2026-10-19).
	if !profileWritable(h.cfg.Profile) {
		return false
	}
	_ = c.call(ctx, "", "Browser.close", nil, nil)
	c.close()
	deadline := time.Now().Add(15 * time.Second)
	for rec.PID > 0 && processAlive(rec.PID) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if rec.PID > 0 && processAlive(rec.PID) {
		return false // it did not go: keep using it rather than start a second one
	}
	_ = os.Remove(h.cfg.EndpointFile)
	return true
}

// profileWritable reports whether this process may write the browser's
// profile (a browser started from here could run on it).
func profileWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".bnbloc-write-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	return os.Remove(name) == nil
}

// browserHost is where runs' tabs live: the member's dedicated profile
// (localHost) or, by build order, another host behind the same executor.
type browserHost interface {
	// open starts a run's tab; attach reattaches it by id (errTabGone).
	// Both show its pages as dev shows them (the run's device, device.go).
	open(ctx context.Context, dev string) (tabHandle, error)
	attach(ctx context.Context, id, dev string) (tabHandle, error)
	// close closes a run's tab by id (errTabGone when it is gone).
	close(ctx context.Context, id string) error
	// url is a run's tab's address, and forward brings its window to the
	// front: both from the browser, never attached to the page (a keeper
	// that let go of it, keep.go).
	url(ctx context.Context, id string) (string, error)
	forward(ctx context.Context, id string) error
	// tabs are the browser's tabs, each with the tab that opened it ("" none).
	tabs(ctx context.Context) map[string]string
	// finishedDownloads moves the run's finished downloads into dir and
	// answers their dir-relative names.
	finishedDownloads(tabID, dir string) []string
}

// tabHandle is one run's tab.
type tabHandle interface {
	browserPage
	target() string
	detach(ctx context.Context)
}

// open starts a run's tab.
func (h *localHost) open(ctx context.Context, dev string) (tabHandle, error) {
	c, err := h.connect(ctx)
	if err != nil {
		return nil, err
	}
	return openTab(ctx, c, !h.cfg.Headless, dev)
}

// attach reattaches a run's tab by its target id.
func (h *localHost) attach(ctx context.Context, targetID, dev string) (tabHandle, error) {
	c, err := h.connect(ctx)
	if err != nil {
		return nil, err
	}
	return attachTab(ctx, c, targetID, dev)
}

// close closes a run's tab by its target id.
func (h *localHost) close(ctx context.Context, targetID string) error {
	c, err := h.connect(ctx)
	if err != nil {
		return err
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := c.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, &out); err != nil {
		var ce *cdpError
		if errors.As(err, &ce) {
			return errTabGone // no such target
		}
		return err
	}
	return nil
}

// url answers a tab's address from the browser (errTabGone when it is
// gone).
func (h *localHost) url(ctx context.Context, targetID string) (string, error) {
	c, err := h.connect(ctx)
	if err != nil {
		return "", err
	}
	var out struct {
		TargetInfo struct {
			URL string `json:"url"`
		} `json:"targetInfo"`
	}
	if err := c.call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": targetID}, &out); err != nil {
		var ce *cdpError
		if errors.As(err, &ce) {
			return "", errTabGone
		}
		return "", err
	}
	return out.TargetInfo.URL, nil
}

// tabs lists the browser's tabs with their openers, from the browser.
func (h *localHost) tabs(ctx context.Context) map[string]string {
	out := map[string]string{}
	c, err := h.connect(ctx)
	if err != nil {
		return out
	}
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			OpenerID string `json:"openerId"`
		} `json:"targetInfos"`
	}
	if c.call(ctx, "", "Target.getTargets", nil, &targets) != nil {
		return out
	}
	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			out[t.TargetID] = t.OpenerID
		}
	}
	return out
}

// forward brings a tab's window to the front and selects the tab, from the
// browser.
func (h *localHost) forward(ctx context.Context, targetID string) error {
	c, err := h.connect(ctx)
	if err != nil {
		return err
	}
	return (&tab{conn: c, targetID: targetID}).bringForward(ctx)
}

// readEndpoint reads a launcher's endpoint file.
func readEndpoint(path string) (endpointRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return endpointRecord{}, false
	}
	var rec endpointRecord
	if json.Unmarshal(raw, &rec) != nil || rec.URL == "" {
		return endpointRecord{}, false
	}
	return rec, true
}

// writeEndpoint writes the endpoint file atomically (0600).
func writeEndpoint(path string, rec endpointRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, _ := json.Marshal(rec)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// chromeArgs are the browser's flags: the dedicated profile, DevTools on
// a loopback port the browser picks (it writes DevToolsActivePort into
// the profile). It starts with no window, so it never takes the member's
// screen (each run opens its own window behind theirs: openTab), and keeps
// rendering pages whose window is covered, which the live view captures.
func chromeArgs(cfg hostConfig) []string {
	args := []string{
		"--user-data-dir=" + cfg.Profile,
		"--remote-debugging-port=0",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
	}
	if cfg.Headless {
		return append(args, "--headless=new", "--window-size=1120,780", "about:blank")
	}
	return append(args, "--no-startup-window")
}

// chromiumApps are the Chromium-based browsers a run can drive (all speak
// CDP), in the order they are tried: Chrome, Chromium, Edge, Brave. Each is
// its macOS app, its Windows install under Program Files or the user's
// local app data, and its Linux command names.
var chromiumApps = []struct {
	mac, windows string
	linux        []string
}{
	{"Google Chrome.app/Contents/MacOS/Google Chrome", `Google\Chrome\Application\chrome.exe`, []string{"google-chrome", "google-chrome-stable", "chrome"}},
	{"Chromium.app/Contents/MacOS/Chromium", `Chromium\Application\chrome.exe`, []string{"chromium", "chromium-browser"}},
	{"Microsoft Edge.app/Contents/MacOS/Microsoft Edge", `Microsoft\Edge\Application\msedge.exe`, []string{"microsoft-edge", "microsoft-edge-stable"}},
	{"Brave Browser.app/Contents/MacOS/Brave Browser", `BraveSoftware\Brave-Browser\Application\brave.exe`, []string{"brave-browser", "brave"}},
}

// findChrome locates the browser runs drive: the member's setting (the
// component's "Chrome executable", on the Components page), else the first
// Chromium-based browser installed (browserCandidates).
func findChrome(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	home, _ := os.UserHomeDir()
	for _, c := range browserCandidates(runtime.GOOS, home, os.Getenv, exec.LookPath) {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("no Chrome, Chromium, Edge or Brave is installed on this computer: install one, or set the browser component's Chrome executable on the Components page")
}

// browserCandidates are the paths a Chromium-based browser may be at, in
// chromiumApps' order: on macOS its app in /Applications or ~/Applications,
// on Windows under Program Files or the user's local app data, elsewhere its
// commands on PATH.
func browserCandidates(goos, home string, getenv func(string) string, lookPath func(string) (string, error)) []string {
	var out []string
	for _, app := range chromiumApps {
		switch goos {
		case "darwin":
			out = append(out, filepath.Join("/Applications", app.mac))
			if home != "" {
				out = append(out, filepath.Join(home, "Applications", app.mac))
			}
		case "windows":
			for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
				if base := getenv(env); base != "" {
					out = append(out, filepath.Join(base, app.windows))
				}
			}
		default:
			for _, name := range app.linux {
				if p, err := lookPath(name); err == nil {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// startChrome starts the browser and waits for its DevTools endpoint.
// detached keeps it running after this process exits.
func startChrome(ctx context.Context, cfg hostConfig, detached bool) (*exec.Cmd, string, error) {
	bin, err := findChrome(cfg.Chrome)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(cfg.Profile, 0o700); err != nil {
		return nil, "", fmt.Errorf("the browser profile %s: %w", cfg.Profile, err)
	}
	// A browser that cannot write its profile (a process confined away
	// from it: an agent's sandbox) never comes up, and an earlier
	// browser's port file left behind would name a browser that is gone.
	if !profileWritable(cfg.Profile) {
		return nil, "", fmt.Errorf("the browser cannot be started from here: its profile %s is not writable (this process is confined, e.g. an agent's sandbox); start it from the desktop", cfg.Profile)
	}
	portFile := filepath.Join(cfg.Profile, "DevToolsActivePort")
	if err := os.Remove(portFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, "", fmt.Errorf("clear the browser's last port file: %w", err)
	}
	cmd := exec.Command(bin, chromeArgs(cfg)...)
	if detached {
		detach(cmd)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("start the browser: %w", err)
	}
	// Starting a browser is bounded (it either comes up or it does not);
	// a run itself never is.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if url, ok := devToolsURL(portFile); ok {
			return cmd, url, nil
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	return nil, "", errors.New("the browser started but never opened its DevTools port")
}

// devToolsURL reads the profile's DevToolsActivePort: the port, then the
// browser's WebSocket path.
func devToolsURL(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lines []string
	for sc.Scan() {
		lines = append(lines, strings.TrimSpace(sc.Text()))
	}
	if len(lines) < 2 || lines[0] == "" || !strings.HasPrefix(lines[1], "/devtools/browser/") {
		return "", false
	}
	return "ws://127.0.0.1:" + lines[0] + lines[1], true
}

// launchDetached starts the member's browser for this and later
// sessions, unless another process already is (the endpoint file's lock).
func (h *localHost) launchDetached(ctx context.Context) (string, error) {
	unlock, err := lockFile(h.cfg.EndpointFile + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if rec, ok := readEndpoint(h.cfg.EndpointFile); ok {
		if c, err := dialCDP(ctx, rec.URL); err == nil {
			c.close()
			return rec.URL, nil
		}
	}
	cmd, url, err := startChrome(ctx, h.cfg, true)
	if err != nil {
		return "", err
	}
	if err := writeEndpoint(h.cfg.EndpointFile, endpointRecord{URL: url, PID: cmd.Process.Pid, Flags: flagsKey(h.cfg)}); err != nil {
		return "", err
	}
	_ = cmd.Process.Release()
	return url, nil
}

// runLauncher is `bnbloc-browser launch`: start the browser in the
// foreground, publish its endpoint, and exit with it (the desktop
// supervises this process as the package's sidecar).
func runLauncher(ctx context.Context, cfg hostConfig) error {
	if cfg.EndpointFile == "" {
		cfg.EndpointFile = defaultEndpointFile()
	}
	if cfg.Profile == "" {
		cfg.Profile = defaultProfile()
	}
	cmd, url, err := startChrome(ctx, cfg, false)
	if err != nil {
		return err
	}
	if err := writeEndpoint(cfg.EndpointFile, endpointRecord{URL: url, PID: cmd.Process.Pid, Flags: flagsKey(cfg)}); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	defer os.Remove(cfg.EndpointFile)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Signal(os.Interrupt)
		<-done
		return nil
	}
}
