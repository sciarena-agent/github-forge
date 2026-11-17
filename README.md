# github-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/sciarena-agent/github-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/sciarena-agent/github-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A single-process, GitHub-compatible git forge in Node 22 TypeScript (stdlib only): git smart HTTP (protocol v2 upload-pack, v0 receive-pack), the REST v3 API, GraphQL, and MCP over one in-memory store. Local grade-local: 928/1000 on Node 24 (feature 600, agent 200, perf 128) and 914/1000 on Node 22.18 (feature 600, agent 200, perf 114), 120/120 conformance checks, replay byte-identical.

## Run it

```sh
node --experimental-strip-types main.ts
```

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

## The surface it serves

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe |
| `GET /api/v3/` | 200, `X-Oauth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: the documents `gh` and `github-mcp-server` send |
| `POST /mcp` | Model Context Protocol over streamable HTTP: initialize, tools/list, tools/call |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

The env presents every API call with `Authorization: Bearer $MAKEFUN_SHOP_TOKEN` (the same token is in your environment); `/healthz` takes none. No dependencies at run time, no internet and no `git` binary in the trial; the repository is mounted read-only at `/bot` and it may write under `/tmp` only.

## Layout

- `main.ts`: the lifecycle (bind ephemeral port, register with the env, poll `/done`, exit 0). Kept as the starter ships it.
- `app.ts`: the server entry, routing to REST, GraphQL, MCP and git.
- `store.ts`: the in-memory store shared by all four surfaces, with real SHA-1 git objects (blob/tree/commit), branches, and pack generation/parsing.
- `git.ts`: git smart HTTP, packfiles (plain objects, no deltas), receive-pack ref updates with fast-forward rules, sideband report-status.
- `graphql.ts`: GraphQL schema, introspection, queries and mutations over the same store.
- `rest.ts`: the REST v3 routes.
- `mcp.ts`: the MCP endpoint (6 tools) over the same store.
- `package.json`, `tsconfig.json`, `LICENSE` (MIT): metadata and licence.
