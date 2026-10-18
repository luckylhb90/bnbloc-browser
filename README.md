# bnbloc-browser

**A [bnbloc](https://github.com/luckylhb90/bnbloc) room agent operates web pages.**

An installable MCP tool component. The agent states a goal and the values to
type; the component works the page one step at a time in a dedicated, visible
Chrome profile on the machine of the person who asked, signed in as them, and
stops at a checkpoint whenever a person has to say something: before anything
irreversible, for a value only a person can give, before leaving the site.
People in the room watch the run live and can take it over.

| Tool | What |
|---|---|
| `browse {goal, url, values?, threshold?}` | opens `url` in a new window and runs to a checkpoint |
| `browse {run, value?, confirmed?, file?, reply?}` | resumes that run from its checkpoint |
| `browser_look {run}` | the run's page: a screenshot with its text and fields |

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
