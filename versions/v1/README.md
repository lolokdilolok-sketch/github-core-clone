# github-core-clone-01 — a GitHub-core clone

A single in-memory forge serving all four GitHub surfaces from one store:

- **git smart HTTP** — `git-upload-pack` (v2 + v0), `git-receive-pack` (v0)
- **REST v3** — `/api/v3` (repos, issues, pulls, contents, branches, commits, git refs, labels)
- **GraphQL v4** — `/api/graphql` (viewer, repository, node, issues, pullRequests, mutations, `__type`)
- **MCP** — `/mcp` (JSON-RPC 2.0 Streamable HTTP, revision 2026-07-28 via `server/discover`, legacy `initialize` handshake)

## Layout

- `main.ts` — entry point (Node 22). Spawns `./forge` and forwards its exit code.
- `core/` — the Go source (stdlib only, no dependencies).
- `forge` — the compiled static Go binary (built with `go build -trimpath -ldflags="-s -w"`).

## Build

```sh
export PATH=$HOME/workspace/tools/go/bin:$PATH   # Go 1.25.4
cd core && go build -trimpath -ldflags="-s -w" -o ../forge .
```

## Run locally

```sh
MAKEFUN_SHOP_TOKEN=test-token ./forge   # serves forever on an ephemeral port
```

Then run the kit's local runner against it:

```sh
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token test-token [--perf]
```

## Notes

- Viewer login is the seeded `adaiversen` (id 1000); the default branch is `main`.
- State is in-memory only and deterministic: the same seeded sequence of
  requests produces the same state export.
- Packs are written without deltas (the client builds no deltas either);
  delta packs sent by clients are resolved on receipt.
