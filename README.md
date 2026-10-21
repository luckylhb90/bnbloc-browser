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

**Sign-ins** (0.7.2). A page off the run's sites that asks someone to sign
in (a password field, or an address or title naming a sign-in) pauses the
run at `needs_human`: a password at once, an identity provider's
login-name page once it stayed 3 s (a single sign-on the person already has
passes through by itself). The live view stays in the room for the person
to sign in, and the agent goes on once they say so: `browser_view` (or
`browser_find`, or an act) on a page past the sign-in ends the kept view
and answers the page, while the sign-in page itself still answers the
pause. A page that sends
itself to the identity provider by script after it loaded (an expired
session) is caught too: a step waits a moment after a page loaded, and
while a navigation it began is on its way, before it takes the page as
where it came to, and every call that answers a page (`browser_open`,
`browser_act`, `browser_view`, `browser_find`) checks it for a sign-in
first. Sign-in pages are never pictured; the replay says so.

**Replay pictures** (0.7.3). Each run leaves `browser-runs/<run>.html`,
its steps with the page after each. Pictures are JPEG (quality 70) at one
pixel per CSS pixel, at most 1280 wide (a phone run's are 390), and a
replay may be up to 5 MiB: that is how large a page the room still plays
in its live preview (past 2 MiB the bridge publishes it as a blob, which
needs a core that reads files byte-exact, as the GIF guide does). A
1280-wide page measured 50 KB a picture (125 KB for one dense with text
and tables), so a 40-step run of ordinary pages fits whole and one of dense
pages keeps about 30 pictures. A step that acted on a control has it framed in
red on its picture (a dot where it pressed when the control is gone), and
its line names it (`Clicked "Send"`). A step that left the page looking as
it did keeps no picture of its own: the replay plays the earlier one and
says "same as step N". When the pictures still do not all fit, plain view
steps (scrolls) lose theirs first, then acting steps evenly, while the
first and last steps and any that paused keep theirs; a step without its
picture says why. The GIF guide is made of the same pictures, marks
included. Sign-in pages are still never pictured.

**Controls a script made** (0.7.4). Besides links, buttons, fields and
elements with a control role, a page's controls include what a script made
clickable, as component-library admin pages build their menus, tabs and
cards: the outermost element showing the pointer cursor, or one with an
`onclick` attribute, that has a name. Each is a `button` labelled with its
text (at most 120 characters). One inside a listed control or holding one
is left to that control (a card's own button is the button), and a label
stands for its checkbox or radio only when that field is hidden (a styled
checkbox).

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
