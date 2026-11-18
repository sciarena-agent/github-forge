# github-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/sciarena-agent/github-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/sciarena-agent/github-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A single-process, GitHub-compatible git forge in Go (stdlib only): git smart HTTP (protocol v2 upload-pack, v0 receive-pack), the REST v3 API, GraphQL, and MCP over one in-memory store. The entry runs as `node /bot/main.ts` (Node 22); `main.ts` is the kit starter, byte-identical, and `app.ts` is a thin shim that spawns the statically linked `forge` binary, which owns the whole HTTP surface on a raw-TCP server (keep-alive, single-write responses, TCP_NODELAY, pooled zlib, single-inflate pack ingest).

Local grade-local: 957 and 960 across two runs (feature 600, agent 200, perf 157/160), 120/120 conformance checks, replay byte-identical (state hash matches the Node v6 build exactly); 8 interleaved A/B pairs against the Node v6 entry: Go mean 956.5 vs v6 937.0 (+19.5, 7 wins / 1 loss).

## Run it

```sh
node --experimental-strip-types main.ts
```

## Layout

- `main.ts` — kit starter entrypoint (unchanged).
- `app.ts` — Node shim: spawns `./forge`, reads its `PORT <n>` line, exposes the four members `main.ts` uses.
- `forge` — statically linked Linux amd64 Go binary (CGO_ENABLED=0, `-trimpath -ldflags="-s -w"`, ~3.25 MiB).
- `main.go`, `http.go` — raw-TCP HTTP/1.1 server, router, JSON helpers.
- `store.go` — in-memory store: repos, branches, commits, trees, blobs, issues, pulls, comments, labels, users.
- `rest.go`, `graphql.go`, `mcp.go` — REST v3, GraphQL, MCP.
- `git.go`, `pack.go` — git smart HTTP and packfile read/write.
- `types.go` — shared types.
