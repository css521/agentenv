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
     — what `daemon` + `supervise` serve; the `goclient` / `branch_explore.py` /
     `Client.java` examples here use it directly.
  3. **MCP** — `agentenv mcp` is a Model Context Protocol server over stdio.
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

| File | Language | Run |
|------|----------|-----|
| `branch_explore.py` | Python | `python3 branch_explore.py <sock>` |
| `goclient/main.go`  | Go      | `go run goclient/main.go <sock>` |
| `Client.java`       | Java 16+| `javac Client.java -d /tmp && java -cp /tmp Client <sock>` |

Each forks the environment from one base, tries three candidate environments in
parallel DAG branches (each in its own isolated workspace), and keeps the one
that passes the test — purely over the socket.

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
