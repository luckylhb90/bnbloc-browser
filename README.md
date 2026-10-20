# bnbloc-browser

**A [bnbloc](https://github.com/luckylhb90/bnbloc) room agent operates web pages.**

An installable MCP tool component. The agent opens a page in a dedicated,
visible Chrome profile on the machine of the person who asked, signed in as
them, reads it and acts on its controls one call at a time; the component
pauses whenever a person has to say something: before anything
irreversible, for a sign-in only they can do, before leaving the site.
People in the room watch the run live and can take it over.

| Tool | What |
|---|---|
| `browser_open {url, device?}` | the room's run: its tab opened on `url`, as a `desktop` (default), `phone` or `tablet` shows it |
| `browser_view {run, screenshot?}` | the run's page: its text and numbered controls (and its picture) |
| `browser_find {run, what}` | the controls a description most likely names |
| `browser_act {run, action, …, then?}` | click, type, select, press, scroll, back, wait, upload; then the page |
| `browser_close {run?, procedure?, purpose?, gif?}` | closes the run; saves its steps as a skill pack draft, or its pictures as a GIF guide |

**Phone view and GIF guides** (0.7.0). A run opened with `device: phone`
(390×844 CSS pixels at 3× density) or `tablet` (820×1180 at 2×) is emulated
on every session that drives its tab: its size, touch, and the user agent
and client hints of Chrome on Android, so sites serve the layout their phone
users get. Clicks stay mouse clicks, which reach a page's click handlers
under touch emulation. Pictures and the live view stay one image pixel per
CSS pixel, so a phone run's are phone-shaped. Chrome keeps an emulation
only while its session is attached, so between calls the window itself is
no phone; each call, and a keeper waiting for a sign-in, shows the page as
the device again.
`browser_close {run, gif: true}` turns the run's pictures into
`browser-runs/<run>.gif` beside its replay: each pictured step 1.5 s (the
last 3 s) with its caption, at most 480 wide for a phone run and 800 for
others, a median-cut palette per frame, at most 8 MB (it shrinks, then
drops every other step). Sign-ins are never pictured, so never in it.
Captions are drawn in the system's own fonts (PingFang, Hiragino Sans GB,
STHeiti or Arial Unicode on macOS; Microsoft YaHei, SimSun or Segoe UI on
Windows; Noto Sans CJK, WenQuanYi or DejaVu on Linux), each character in the
first that has it, so Chinese and Vietnamese read as written, wrapped by
width (between any two CJK characters too). On a machine with none of
them, captions fall back to a 7×13 ASCII font where other characters read
`...`. The replay links the GIF: the room runs the replay sandboxed, without
downloads, and opens the room file a relative link names in its own tab,
where it can be downloaded.

Nothing to configure: the room is the credential (`mcp.credentials: "room"`).
Each call reaches the gateway through the bridge's room relay, as the person
the agent is answering. How it works: [`docs/design.md`](docs/design.md).

Needs Chrome (or Chromium) on the member's machine; `--endpoint` /
`BNBLOC_BROWSER_ENDPOINT` points at a running one instead.

## Install into bnbloc

Download the binaries and `package.json` from the
[releases](https://github.com/luckylhb90/bnbloc-browser/releases) (or build them,
below), then as a bnbloc super-admin:

```sh
bnbloc cores publish-mcp --gateway https://<gateway> --package package.json \
  --binary darwin-arm64=dist/bnbloc-browser-darwin-arm64 --binary darwin-amd64=dist/bnbloc-browser-darwin-amd64 \
  --binary linux-amd64=dist/bnbloc-browser-linux-amd64 --binary windows-amd64=dist/bnbloc-browser-windows-amd64.exe \
  --rollout optin
```

Members who opt in (or everyone, with `--rollout auto`) get it in their
desktop app.

## Build and test

```sh
go test ./...
go build -o bnbloc-browser .
```

It builds against bnbloc's public packages (`component`, `core`,
`core/pkgmeta`), pinned in `go.mod`. A tag `vX.Y.Z` builds every platform
binary in CI and attaches them to the release; the tag's version is the
`version` in `package.json`.

## License

[Apache License 2.0](LICENSE).
