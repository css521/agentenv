# Examples

agentenv is **language-agnostic** on both sides:

- **Workloads**: capture/rollback happens at the *filesystem* level, so the agent
  can build/run anything — `go build`, `mvn package`, `pip install`, `cargo build`,
  `npm i` — and it's all snapshotted regardless of language.
- **Drivers**: three ways to drive agentenv, each language-neutral:
  1. **CLI** — `agentenv checkout <id>`, `agentenv log`, `agentenv delete <id>`,
     `agentenv tournament …`. When a `daemon`/`supervise` holds the lock,
     mutating commands auto-route through its socket — same command works
     whether or not a daemon is up.
  2. **Socket protocol** (newline-JSON, one request → one response or stream)
     — what `daemon` + `supervise` serve; the `goclient` / `branch_explore.py`
     examples here use it directly.
  3. **HTTP + OpenAPI** (v0.3.0) — same operations as REST, with an
     OpenAPI 3.1 spec at `/openapi.json` and interactive docs at `/docs`.
     Enable with `agentenv daemon --http 127.0.0.1:8911`. The
     [`http_client.sh`](./http_client.sh) walkthrough shows curl. Streaming
     `exec` stays socket-only.
  4. **MCP** — `agentenv mcp` is a Model Context Protocol server over stdio.
     Use it from **Claude Code** (the headline path); the
     [`claude-code/`](./claude-code/) sub-example ships a ready-to-run image.

## The socket protocol (one JSON request/response per line)

```
# environment changes (multi-frame streams)
{"op":"exec","cmd":"go build ./..."}     -> {"stdout":"..."} {"stdout":"..."} {"ok":true,"exit":0,"node":"<id>","head":"<id>"}
{"op":"spawn","cmd":"server &"}          -> {"ok":true,"pid":N}

# history
{"op":"head"}                             -> {"ok":true,"head":"<id>"}
{"op":"log"} {"op":"branches"}            -> {"ok":true,"nodes":[…]}
{"op":"show","node":"<id>"}               -> {"ok":true,"changes":[…]}
{"op":"diff","a":"<id>","b":"<id>"}       -> {"ok":true,"changes":[…]}

# mutation
{"op":"commit","message":"…"}             -> {"ok":true,"node":"<id>","head":"<id>"}
{"op":"checkout","node":"<id>"}           -> {"ok":true,"head":"<id>"}
{"op":"delete","node":"<id>"}             -> {"ok":true,"head":"<id>"}                       # v0.2.0
{"op":"tag","name":"winner","ref":"<id>"} -> {"ok":true}
{"op":"tournament","base":"<ref>","test":"<cmd>","candidates":[…]} -> {"ok":true,"branches":[…],"winner":"…"}

# processes / disk
{"op":"ps"} {"op":"kill","pid":N} {"op":"gc"}
```

Error frames have `{"error":"…"}` instead of `{"ok":true,…}`. Streaming ops
(currently `exec`) end with a terminal frame carrying `ok` or `error`.

## Clients (all stdlib, no dependencies)

Each demonstrates a distinct v0.2.0 pattern, so they're complements rather than
translations of each other.

| File | Driver | Pattern |
|------|--------|---------|
| `branch_explore.py` | Socket (NDJSON) | Hand-rolled exploration: fork base → try 3 candidates → keep winner → **`delete` the losing branches** |
| `goclient/main.go`  | Socket (NDJSON) | High-level `tournament` op: one round-trip, daemon runs the candidates in parallel workspaces; then `delete` the losers |
| `http_client.sh`    | HTTP            | Plain `curl` against the REST endpoints; opens `/docs` in a browser for the auto-generated UI |
| `mcp_client.py`     | MCP (JSON-RPC over stdio) | Drive `agentenv mcp` from any language (no Claude Code needed); calls `agentenv__log` / `__branches` / `__delete` |

## Simplest driver: the CLI

Any orchestrator can just shell out. When a daemon/supervise is running, the
CLI transparently routes through its socket (no `--socket` needed):

```sh
node=$(agentenv head)
agentenv exec -- bash -lc 'make test' || agentenv checkout "$node"   # undo on failure
agentenv delete "$node"                                              # prune a dead end
```

## Claude Code via MCP

The fastest way to see agentenv shine. See [`claude-code/`](./claude-code/):
one `docker run -it ghcr.io/css521/rewindable-claude` lands you in a sandbox
where Claude Code can edit, install, and **roll back its OWN environment** via
the `agentenv__checkout` / `agentenv__delete` MCP tools — without exiting the
session.
