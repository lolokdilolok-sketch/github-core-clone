# BUILD_NOTES — github-core-clone-01 (GitHub core clone, $500)

## Brief (from kit RULES.txt / competition.yaml)

Build a GitHub-core clone serving four surfaces from one store: git smart HTTP
(upload-pack v2 + v0, receive-pack v0), REST v3 at `/api/v3`, GraphQL v4 at
`/api/graphql`, MCP at `/mcp`. Lifecycle: bind an ephemeral port (never 8080),
POST `$MAKEFUN_ENV_URL/register {"port": N}`, poll GET
`$MAKEFUN_ENV_URL/done` until `{"done": true}`, exit 0. Viewer login is the
seeded `adaiversen` (id 1000); default branch `main`. Max 16 MiB source total,
8 MiB per file. Score 0–1000 = Feature 600 (120 conformance checks) +
Agent-native 200 (15 MCP checks) + Perf 200 (4 workloads vs house reference
`T_ref` in `sdk/conformance/calibration.json`: push 1640.82 ms, clone
266.941 ms, read 2759.715 ms, write 348.965 ms).

## Autopsy — winners' formula

Top entries are a static compiled Go core vendored in the repo; `main.ts`
(Node 22) at the root only spawns that binary; raw HTTP, no framework;
zero-alloc hot paths; one in-memory store for all four surfaces. This entry
copies that formula exactly, then beats it on one side: **zlib pooling**.
On this host a fresh `compress/zlib` writer costs ~440 µs (reader similar),
while a pooled `Reset` costs ~6 µs — a ~70× difference that moved clone
29 → 42–48 pts. Writers and readers are pooled with `sync.Pool`.

## Pay line / top-10 line (live, checked 2026-11-18)

Rank 1 = 966, rank 10 = 957, rank 20 (last paid) = 955. Top 100 all score
600/200 — perf is the only differentiator. Target: score ≥ 960
(feature 600 + agent 200 + perf ≥ 160, p50 ≤ ~345,000 µs).

## Measurements (kit local runner, fresh server per run)

| run | conformance | MCP | feature | agent | perf | score | p50_us |
|-----|-------------|-----|---------|-------|------|-------|--------|
| 1 (2026-11-18) | 120/120 | 15/15 | 600 | 200 | 191 | **991** | 210,564 |
| 2 (2026-11-18) | 120/120 | 15/15 | 600 | 200 | 166 | **966** | 320,250 |
| 3 (2026-11-18) | 120/120 | 15/15 | 600 | 200 | 183 | **983** | 237,697 |

Best per workload (run 1): push 50/50 (299 ms), clone 48/50 (75 ms),
read 49/50 (736 ms), write 44/50 (119 ms). Weakest workload is write
(44–46 pts); read varies most with host CPU noise (736–1692 ms across runs).

## Margin

- vs top-10 line (957): **+9 to +34** (mean ~+23).
- vs rank 1 (966): **0 to +25** (mean ~+14). Two of three runs beat rank 1
  outright; the lowest run ties it.
- Conformance is perfectly stable: 120/120 and 15/15 on every fresh-server run.

## What would raise the position further

Write workload (44–46/50) is the remaining gap: hand-roll the JSON for the
hot `POST /issues` / `GET /issues/{n}` paths instead of `map[string]any` +
`encoding/json` (est. +2–4 pts). Read is client-bound on the grader's
hardware; further server-side wins there are small. A pack cache keyed by
(wants, depth, haves) would help clone on repeated identical fetches, but
clone is already 42–48/50 and the cache adds invalidation risk.

## Server score 0 — root cause and fix (2026-11-18, after first submit)

- First submit scored **0 on the server** (rank 8329/8262, feature 0, agent 0,
  perf 0, p50 1e9 sentinel, working 0) despite local runs of 991/966/983.
  Root cause: the `forge` binary in the GitHub repo had git mode **100644**
  (not executable) — the GitHub web UI stores every uploaded file as 100644
  and strips the exec bit. The grader checks out the repo, `main.ts` spawns
  `./forge`, the spawn fails with **EACCES**, the program never binds a port.
  Reproduced locally: scratch copy with every file chmod 644 + old `main.ts`
  → `[entry] failed to start forge: spawn .../forge EACCES`, exit 127.
- Fix: `main.ts` now runs `fs.chmodSync(binary, 0o755)` in a try/catch
  (logs a warning, still attempts the spawn) **before** spawning `./forge`.
  Verified from a full-644 scratch copy: server starts (forge mode becomes
  755 at runtime), local runner **992** (feature 600, agent 200, perf 192,
  p50 203,653 µs, 120/120, MCP 15/15). Re-run from the normal repo state:
  also **992** (p50 220,003 µs). No regression.
- **UPDATE 2026-11-18 22:31 WIB — the chmod fix did NOT work on the server.**
  Round 2 (same commit 017b274, received 15:12:11Z) still scored **0 on every
  column** (rank 8360/8360, working 0, p50 1e9). The 644 scratch-copy test
  was a false negative: chmod only needs file *ownership*, not writability,
  so it succeeded on the writable scratch copy. The kit's RULES.txt states
  the grading pod runs with a **read-only /bot** ("a read-only /bot (your
  repository at the submitted commit)"), and on a read-only mount chmod
  fails with EROFS for every user including root. Reproduced faithfully
  with `unshare -rm` + read-only bind mount of the repo: `[entry] warning:
  could not chmod forge: EROFS: read-only file system` then `[entry] failed
  to start forge: spawn /tmp/clone-ro/forge EACCES`, no register — exactly
  the server's signature.
- **Real fix (2026-11-18 ~22:40 WIB):** `main.ts` now resolves the
  executable in `resolveExecutable()`: try chmod in place; check
  `accessSync(X_OK)`; if still not executable, copy the binary to a fresh
  `mkdtempSync(os.tmpdir()/forge-)` dir, chmod the *copy* 0o755, and spawn
  the copy. Verified under the SAME read-only reproduction: registers,
  kit runner **990** (feature 600, agent 200, perf 190, 120/120, MCP 15/15,
  p50 216,194 µs, working 1). Verified under a normal writable checkout:
  **994** (perf 194, p50 205,426 µs, working 1), no copy needed, no
  regression. The binary is statically linked ELF x86-64 (verified with
  `file`/`ldd`); Node 22 vs 24 is not a factor (the kit's own starter
  main.ts uses type annotations and runs as `node /bot/main.ts` on Node 22;
  this entry uses only stable stdlib APIs).
- Resubmit note: the fix must go up in a NEW commit; the browser upload
  will strip the exec bit again, which is now harmless because `main.ts`
  either re-applies it in place or runs a writable copy from tmpdir.

## Bugs found and fixed during testing

1. v2 `ls-refs`: peel must be an attribute on the ref line, not a second
   line; HEAD symref must be omitted when a `ref-prefix` filters it out.
2. v2 fetch section name is `acknowledgments` (not `acknowledgment`).
3. v2 fetch packfile section must be sideband-framed (band 1 packets), and
   `wanted-refs` lines are `<oid> <ref>`, not bare oids.
4. Sideband chunks must be ≤ 65,515 bytes payload (pkt-line max 65,520 total).
5. `Link rel="next"` must be an absolute URL on the entry's origin.
6. `GET /api/v3/` must carry `X-OAuth-Scopes`.
7. Empty list pages must serialise as `[]`, not `null`.
8. PR head/base SHAs must resolve live from branch refs (pushes after PR
   creation change them) — this is what made merge-conflict detection work.
9. GraphQL mutations return payload types (`{issue}`, `{pullRequest}`,
   `{commentEdge {node}}`); connection pagination args (`first`/`after`)
   live on the connection field, not on `nodes`/`pageInfo`.
10. Deepen fetch: a have that is also a shallow boundary must not subtract
    its ancestors (shallow-aware have closure).
11. Concurrency: 50 concurrent clone fetches raced on `rp.shallowSet`
    (fatal: concurrent map iteration and map write). Git fetch/info-refs now
    hold the store read lock; shallow recording takes the write lock after.
    Verified clean under `-race` (120/120, 0 data races; race-detector perf
    is ~2× slower and not representative).
12. v0 fetch wants carry space-separated capabilities on the first line, and
    haves arrive after the first flush — both must be parsed.

## Version / environment notes

- Go 1.25.4 at `~/workspace/tools/go/bin/go`; binary built with
  `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"` — static, 6.7 MB
  (under the 8 MiB per-file cap). Repo total ~7 MB (under 16 MiB).
- Node on this machine is v24.22.1; the kit runner and grader use Node 22.
  `main.ts` only spawns `./forge` (no Node APIs beyond `child_process`),
  so the version difference is low-risk; the local runner ran fine on 24.
- Python is not used by this entry.
- State is in-memory per process and there is no reset endpoint (same as a
  real entry): **restart the server before each local-runner invocation**.
  Re-running the runner against a dirty server fails with 422 / non-FF
  errors because the fixed scenario seed reuses repo names (observed once,
  32/120 — not a code defect).

## How to test

```sh
export PATH=$HOME/workspace/tools/go/bin:$PATH
cd core && go test ./...            # pack round-trip regression test
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../forge .
cd .. && MAKEFUN_SHOP_TOKEN=test-token ./forge   # fresh server, note the port
node --experimental-strip-types \
  ../github-core-clone-01-kit/sdk/run.ts \
  --url http://127.0.0.1:PORT --token test-token --perf
```

## Files

- `main.ts` — entry: spawns `./forge`, forwards its exit code.
- `core/` — Go source, stdlib only (`main.go` lifecycle+dispatch,
  `store.go` model+objects+packs, `git.go` smart HTTP, `api.go` REST,
  `graphql.go` GraphQL, `mcp.go` MCP, `pack_test.go` regression test).
- `forge` — compiled static binary (vendored).
- `README.md`, `LICENSE` (MIT), `BUILD_NOTES.md` (this file).
- `versions/v1/` — source snapshot of this version (no binary).

## Status

Built and verified locally. **Not submitted, no GitHub repo created.**
Submit only on boy's go-ahead, after the pre-submit top-10 check (passed:
991/966/983 vs line 957). Max 5 submits/day, 10 total, Force_Last.
