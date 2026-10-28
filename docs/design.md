# Browser component

Design: [the browser component PRD](https://github.com/luckylhb90/bnbloc/blob/main/docs/prd/2026-10-17-browser-component.md) in the bnbloc repository.

A room agent operates web pages in a dedicated, visible Chrome profile on
the machine of the person who asked, signed in as them (PRD §3.6; until
calls are routed to the requester's machine, a turn whose agent is
someone else's answers `needs_human`). The agent states
a goal and the values to type; the component works the page one step at a
time, and stops at a checkpoint whenever a person has to say something.

It is an installable MCP tool component (`package.json`, this
repository's binary), `credentials: "room"`: it reaches the
gateway only through the room relay, as the person the agent is answering.
The platform gives it two generic things (§3.1, §3.5 of the PRD); nothing
else in the platform knows it exists. Installing it is the switch: a
turn gets every MCP tool its requester installed, with no skill pack
(`mcptool.Resolve`, PRD §3.6).

## Owners

- This repository:
  - `tools.go` — the two tools, their wording, owner and requester
    checks, consent, the result shape.
  - `run.go` — the run loop, checkpoints, saved runs.
  - `step.go` — the action space and the decision request.
  - `browser.go` — observe, freshness, input, screenshot.
  - `cdp.go` — the DevTools socket.
  - `host.go` — which browser, launching it.
  - `downloads.go` — finished downloads into the workspace.
  - `relay.go` — the relay calls.
  - `live.go` — the live view: frames out, a viewer's input in.
  - `keep.go` — the keeper: a run paused for a person keeps its view in
    the room after the call.
  - `snapshot.js` — the page reader, a modified port of browser-use's
    jev-ultrafast (MIT, `NOTICE`).
- In bnbloc (the platform, all generic): `component.RelaySession` and the
  bridge's relay (`services/bridge/roomtools/relay.go`), the bound job's
  facts; `services/gateway/decisions.go`, the decision rate.
- Live view (any tool's, not only this one's), in bnbloc: `component/live.go`
  (this side of the contract), `internal/wire/job_live.go` (the room's), `services/gateway/job_live.go` (`POST /v1/live` under
  the job grant), `services/realtime/job_live.go` (newest frame per job,
  watching, control, input), `web/src/views/live/` and
  `web/src/lib/jobLive.ts` (thumbnail, large view, pop-out).

## The tools

| Tool | What |
|---|---|
| `browse {goal, url, values?, threshold?}` | opens `url` in a new tab and runs to a checkpoint |
| `browse {run, value?, confirmed?, file?, reply?}` | with a run id: resumes that run from its checkpoint |
| `browser_look {run}` | the run's page: a screenshot with its text and fields |

A result is one JSON head (`run`, `checkpoint`, `site`, `reason`, the
element in question), then, fenced by `roomlog.Fence` as data from the
site, the page's visible text and its fields as filled now (a text
field's value, a box's state, a select's option; a secret only as filled
or empty), which the visible text alone does not show. Only
`browser_look` adds a JPEG screenshot, one image pixel per CSS pixel and
at most 1024 wide: an image costs the agent far more than the text.

## A step

1. **Observe.** `snapshot.js` reads the page's interactive elements into
   a numbered list (role, label, value, a guard per element) and the
   visible text. Secrets (`type=password`, secret `autocomplete`) are
   kind `secret` with no value, only whether one is filled; file inputs
   are kind `upload`; hidden inputs are skipped. A fingerprint (URL, a
   page key, the elements) tells whether anything changed.
2. **Decide.** One `POST /v1/systemone` through the relay: an
   `operation` choice among the operations the page offers (`CLICK`,
   `TYPE_TEXT`, `SELECT`, `UPLOAD`, `FILL_SECRET` on its elements;
   `SCROLL_DOWN`, `SCROLL_UP`, `WAIT` as page controls; `DONE`,
   `BLOCKED`) and one `<op>_target` choice per element operation over the
   code-numbered elements.
   Only the head of the chosen operation is read; the model never
   writes a selector, a URL or a value.
3. **Check.** Before a click, a `predicate` (irreversible?) on the
   element and its guard scope; at or above the run's threshold (default
   0.3; a refused answer counts as 1) the run stops at
   `needs_confirmation`. A typed value comes from `values` by label or
   description containment, else `needs_input`. A secret, or the
   decision's `NEEDS_HUMAN` (a sign-in, a second factor, a captcha, a
   blank sign-in redirect), is `needs_human`; so is a sign-in page off
   the run's sites (an identity provider: a secret field, or an address or
   title naming a sign-in), before any decision there, however late the
   page gets there: a step waits out a page that just loaded or began
   leaving (a script's redirect once it loaded), and every answer of a
   page checks it by the same rule. A blank hand-off
   page (one form of hidden fields, a SAML or OIDC POST binding) still
   standing after 1.5 s is submitted, which is all its own onload does:
   in a background window Chrome can leave it unsubmitted, and the page
   stays blank. A link off the run's sites (registrable domains,
   `publicsuffix`), or a page that has landed off them, is
   `leaving_site`. A file input is `upload`. A paused step is kept and
   executed as decided once the checkpoint is answered.
4. **Execute.** A freshness check first (the page key and the target's
   guard for click and select, the whole marker otherwise): stale →
   re-observe and decide again. Then a hit-test at the element's point:
   covered → stale. A native select that does not report its new value
   ends the run `blocked`, never retried blindly.

Three steps in a row without a change in the fingerprint (waits excepted)
are `blocked`. A call works up to its budget (`--call-budget`, default
45s) and answers `running`; the agent continues it. There is no limit on
a run's length (`no-fixed-time-limits`).

## The live view

While a call works, `live.go` captures the run's tab (JPEG, at most
1024 wide) and posts it to `POST /v1/live` when the picture changed:
every 250 ms while someone watches or holds control, every 2 s
otherwise. Realtime keeps the newest frame per job in memory and sends
the channel a `job_live_frame` event without the image; a viewer fetches
`GET /v1/rooms/{room}/jobs/{job}/live/frame`.

- **Where it shows.** A thumbnail in the turn's progress strip; clicking
  it opens the run in the workbench (a `l:<jobId>` tab), which pops out
  like any workbench tab (a desktop window `live-*`, or a browser tab at
  `#/live/<room>/<job>`).
- **Taking over** (the job's owner): "Take over" in the large view;
  pointer, wheel, keys and paste go to `POST .../live/input`, and the
  tool receives them in the reply to its next frame post and dispatches
  them as CDP input. While someone holds control the run takes no step;
  "Hand back" resumes it. Control lapses 15 s after the view stops
  renewing it.
- **The window** stays behind the member's other windows: Chrome starts
  with no window, each run opens one in the background, and a covered
  window still renders captures (a minimized one does not, so it is
  restored, never minimized). "Open in the browser window" and a `needs_human`
  checkpoint does not: the person does it in the room's view.
- **Waiting for a person** (bnbloc PRD §3.7). At a `needs_human` pause the
  call posts the view saying what the person does (`waiting`) before it
  returns, then starts `bnbloc-browser keep --run <id> --job <turn's job>`,
  detached. The keeper posts the paused tab under that job (the relay's
  `?job=`, a job of the session), at least every 10 s, runs the owner's
  input, says `ready` once the page is back on the run's site with no
  password field, and posts `end` when the run is resumed (the resuming
  call writes `<run>.keep.stop` and waits for it), the owner closes the
  view, the tab is gone, or the relay refuses it. One keeper per run
  (`<run>.keep.lock`); its log is `<run>.keep.log`. The room opens the view
  in its owner's sidebar, takes control for them, and offers Continue.

## Who may run it, and who confirms

- The relay's `GET /session` answers the bound job's facts
  (`wire.RoomRelaySession`): job, room, channel, requester, the bridge's
  owner, the turn's message seq, and whether the turn is the requester's
  own message (`byRequester`: false for a schedule's post and a job
  another agent dispatched).
- A run starts only for the owner's own requests. Another member's
  request returns `needs_human` asking the owner to let them use the
  browser; the grant itself is slice 6.
- A run's requester is fixed when it starts. Resuming it from a turn of
  anyone else is refused.
- A confirmation (`confirmed: true` on `needs_confirmation`,
  `leaving_site`, `upload`) holds only when the current turn is the
  requester's own message and newer than the one the run paused on
  (`messageSeq > pausedAtSeq`). The agent cannot confirm on its own:
  the person has to say yes in the room after seeing the question.
  `confirmed: false` declines, and the run continues with the decline
  added to its goal.

## The browser host

In order:

1. `--endpoint` / `BNBLOC_BROWSER_ENDPOINT` (an `http://127.0.0.1:<port>`
   or `ws://` DevTools address).
2. The endpoint file (`--endpoint-file`, default
   `<tmp>/bnbloc-browser/endpoint.json`), written by
   `bnbloc-browser launch`, the long-lived launcher a desktop runs.
3. Otherwise the component launches Chrome itself, detached so it outlives
   the call: the dedicated profile (`--profile`, default
   `<user config>/bnbloc-browser/profile`),
   `--remote-debugging-port=0`, the port read from `DevToolsActivePort`.

**Where it runs.** The package places the tool on the host
(`mcp.stdio.placement: "host"`, bnbloc's host-run MCP tools): in a sandboxed
session the bridge runs it beside the agent, not as a child of the confined
agent CLI, and the agent reaches it as an MCP server. A confined process cannot
write the profile, so a tool started inside the agent's sandbox could never
launch Chrome ("start it from the desktop", QA 2026-10-28). `privatePaths`
names `bnbloc-browser` under the user's config directory: the profile with
the member's sign-ins, which the agent's CLI may therefore not read. An
unsandboxed session keeps the tool as the agent's child, as before.

Runs are kept as `<state-dir>/brun_*.json` (default
`<tmp>/bnbloc-browser/runs`, a place the agent sandbox allows), one call
at a time per run (a file lock). Downloads finish into
`<tmp>/bnbloc-browser/downloads` and move to the workspace's
`downloads/`, so the turn publishes them to the room like any other
deliverable; the run stops at `download` with the file's name.

## Tests

- `step_test.go`, `run_test.go` — the action space, the request, every
  checkpoint, confirmation by a newer turn, owner and requester, saved
  runs, against a fake page and a fake relay.
- `cdp_test.go` — the transport against a fake DevTools socket.
- `chrome_test.go` — the executor on a real headless Chrome against
  `testdata/fixture.html` (skipped when no Chrome is installed, or in
  `-short`; `BNBLOC_BROWSER_CHROME` names one).
- `live_test.go` — frames posted only on change, a viewer's click and
  typing reaching the page, a held run taking no step.
- `visible_test.go` — the live view on a real, visible Chrome window kept
  in the background (opt-in: `BNBLOC_BROWSER_VISIBLE_TEST=1`).
