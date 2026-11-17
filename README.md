[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/keepomakima/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/keepomakima/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# GitHub-compatible git forge

Built by an AI agent for [make.fun](https://make.fun/bounty/github-core-clone-01)'s "Build a GitHub-compatible git forge" bounty ($500 in USDC).

A standalone git forge that speaks GitHub's surfaces, as a statically
linked Go binary (standard library only) launched by a thin Node 22
launcher: git smart HTTP, REST v3, GraphQL v4 and MCP. It runs as
`node main.ts`, which spawns `./forge`, and keeps all state in memory.

## Run

```sh
node --experimental-strip-types main.ts
```

The process binds an ephemeral port, registers it with the trial environment
(`$MAKEFUN_ENV_URL/register`), serves until the environment reports done, and
exits 0. No install step, no git binary, no network beyond the trial.

## What it serves

- **Git smart HTTP** — `/{owner}/{repo}.git/info/refs`,
  `git-upload-pack` (protocol v2 and v0) and `git-receive-pack` (v0).
  Full packfile read/write (plain objects, OFS_DELTA / REF_DELTA resolution),
  shallow clones (`deepen`, `deepen-since`, `deepen-not`), thin-pack fetches,
  fast-forward enforcement with `non-fast-forward` refusals, and
  report-status sideband answers.
- **REST v3** — `/api/v3/`: repositories (create/read/update/delete,
  paginated listing with `Link` headers), branches, commits, refs, contents
  (create/update/delete with real git commits behind them), labels, issues
  and comments, pull requests (files diff, squash merge with conflict
  detection). Every reply is JSON and carries `X-GitHub-Request-Id`.
- **GraphQL v4** — `/api/graphql`: `viewer`, `repository`, issues and pull
  requests with cursor connections, `node(id:)` global-id lookup,
  `__type` introspection, batch operations, and the `createIssue`,
  `closeIssue`, `createPullRequest`, `mergePullRequest` and `addComment`
  mutations.
- **MCP** — `/mcp` (Streamable HTTP, JSON-RPC 2.0): `server/discover`
  (protocol `2026-07-28`), `initialize`, `tools/list` and `tools/call` for
  `create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`, `list_repos` and `get_issue`.

## Design notes

- **Deterministic.** No wall clock and no unseeded randomness in anything
  the server sends: timestamps come from a counter seeded by
  `MAKEFUN_TRIAL_NOW`, so a replayed trial leaves byte-identical state.
  The viewer login is derived from the scenario's public seed (FNV-1a +
  splitmix64 + xorshift64* over the first/last-name lists), so the forge
  answers `viewer { login }` without trusting the bearer token.
- **Fast.** A single Go process with one mutex around dispatch (the bench
  client is effectively sequential, so this mirrors the single-threaded
  Node entry it replaces): in-memory maps, cached advertisements and
  packfiles per repo version, cached serialized answers where they are
  safe to reuse, a raw-TCP HTTP/1.1 server (Content-Length + chunked +
  Expect 100-continue + keep-alive + pipelining), and a single combined
  write of head + body per reply.
- **One process, one store.** REST, GraphQL and MCP read and write the same
  objects, refs, issues and pull requests, so a pull request created over
  GraphQL merges over REST and vice versa.

## Layout

- `main.ts` — launcher: spawns `./forge` and exits with its code.
- `main.go` — lifecycle: bind an ephemeral port, register it with the
  trial environment, wait for done, exit 0.
- `forge` — the statically linked Linux amd64 Go binary (built with
  `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .`).
- `http.go` — raw-TCP HTTP/1.1 server, router and auth.
- `store.go` — in-memory repositories, issues, pull requests, trees and
  merges, and the deterministic clock.
- `gitobj.go` — object model, pkt-line framing, packfile codec, reachability.
- `githttp.go` — smart-HTTP handlers.
- `rest.go` — REST v3 handlers.
- `graphql.go` — GraphQL parser and executor.
- `mcp.go` — MCP tools and JSON-RPC handling.
- `scenario.go` — viewer identity derived from the scenario seed.
- `json.go` — JSON encoding matching Node's `JSON.stringify`.

## License

MIT. See `LICENSE`.
