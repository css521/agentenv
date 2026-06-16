# Examples

agentenv is **language-agnostic** on both sides:

- **Workloads**: capture/rollback happens at the *filesystem* level, so the agent
  can build/run anything — `go build`, `mvn package`, `pip install`, `cargo build`,
  `npm i` — and it's all snapshotted regardless of language.
- **Drivers**: you control agentenv from any language via either the **CLI**
  (`agentenv checkout <id>`, `agentenv log`, ...) or the **newline-JSON unix-socket
  protocol** (`agentenv daemon`). Both are language-neutral contracts; no per-language
  SDK is required.

## The protocol (one JSON request/response per line)

```
{"op":"exec","cmd":"go build ./..."}  -> {"ok":true,"exit":0,"stdout":"...","node":"<id>","head":"<id>"}
{"op":"checkout","node":"<id>"}       -> {"ok":true,"head":"<id>"}
{"op":"log"} {"op":"branches"} {"op":"head"} {"op":"diff","a":"x","b":"y"} {"op":"show","node":"x"}
{"op":"spawn","cmd":"..."} {"op":"commit","message":"..."} {"op":"ps"} {"op":"kill","pid":N} {"op":"gc"}
```

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

Any orchestrator can just shell out:

```sh
node=$(agentenv head)
agentenv exec -- bash -lc 'make test' || agentenv checkout "$node"   # undo on failure
```
