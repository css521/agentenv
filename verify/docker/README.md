# verify/docker — MCP smoke harnesses

Three Docker-based test harnesses, complementary to `scripts/verify-*.sh`
(which cover the rootless / btrfs / supervise paths). These focus on the
**MCP server** layer that landed in v0.1.0.

All three need a cross-compiled binary in the build context:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o verify/docker/agentenv-linux-arm64 .
```

(swap `arm64` → `amd64` on Intel Macs / Linux x86; the Dockerfiles pin
`--platform=linux/arm64` for Apple Silicon — adjust to match the binary.)

## 1. `mcp-smoke` — protocol-level

Asserts the MCP server speaks the protocol correctly. No Claude Code,
no auth, no real LLM — just JSON-RPC over stdio against `agentenv mcp`,
the way Claude Code would drive it.

```bash
docker build --platform=linux/arm64 -f Dockerfile.mcp-smoke -t agentenv-mcp-smoke .
docker run  --rm --platform=linux/arm64 agentenv-mcp-smoke
```

Checks: initialize handshake, tools/list (all 6 tools with schemas), each
tool call routes to the daemon, schema validation rejects bad arguments
before the handler runs. Runs in ~2 seconds.

Wired into `make verify-mcp`.

## 2. `rollback-smoke` — end-to-end

The headline feature: drive `agentenv__checkout` through MCP and verify
the rootfs actually rolls back (not just the HEAD pointer).

```bash
docker build --platform=linux/arm64 -f Dockerfile.rollback-smoke -t agentenv-rollback-smoke .
docker run  --rm --platform=linux/arm64 \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  agentenv-rollback-smoke
```

Flow: init → mutate the rootfs (file2, file3) → checkout root via MCP →
assert file2/file3 are gone → checkout forward to the latest node → assert
they're back. Also smoke-checks the v0.1.0 diagnostics: anti-hallucination
prefix on tool errors, cross-uid hint on socket connect failure, daemon
uid logged on startup.

Requires relaxed seccomp/AppArmor for nested userns (the same constraint
as the K8s pod path). Wired into `make verify-rollback`.

## 3. `claude-shell` — interactive

Interactive Docker shell with the daemon running and Claude Code
preconfigured with the `agentenv` MCP server. Use this to manually drive
the model and watch it inspect/rewind history.

```bash
docker build --platform=linux/arm64 -f Dockerfile.claude-shell -t agentenv-claude-shell .
./run-claude-shell.sh
```

Mounts `~/.claude` (Claude Code's config/state). On macOS the OAuth
credentials live in the system keychain (not under `~/.claude`), so you'll
need to authenticate inside the container — either via `claude` login flow
or by exporting `ANTHROPIC_API_KEY` (and optionally `ANTHROPIC_BASE_URL` /
`ANTHROPIC_MODEL`) before running `run-claude-shell.sh`.

## Cleanup

```bash
docker rmi agentenv-mcp-smoke agentenv-rollback-smoke agentenv-claude-shell
rm verify/docker/agentenv-linux-arm64
```
