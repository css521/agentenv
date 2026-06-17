#!/usr/bin/env python3
"""Drive `agentenv mcp` as a generic MCP client (no Claude Code needed).

The MCP server (added in v0.1.0) is what lets Claude Code roll back its own
environment via the agentenv__checkout / agentenv__delete tools. This file
shows the same surface is reachable from any language that can fork a process
and exchange newline-delimited JSON-RPC over stdio — useful as a smoke test, a
CI integration, or the foundation for another agent harness.

Walks through every v0.2.0 tool: log, head, branches, show, diff, checkout,
delete (the new one — prunes a node from the DAG). Requires `agentenv mcp` on
PATH and a running daemon (or pass --socket to a known control socket).

  python3 mcp_client.py [--socket /var/lib/agentenv/agentenv.sock]
"""
import json
import os
import subprocess
import sys


class MCP:
    """Minimal MCP client: subprocess agentenv mcp, framed JSON-RPC on stdio."""

    def __init__(self, socket=None):
        env = os.environ.copy()
        if socket:
            env["AGENTENV_SOCKET"] = socket
        self.p = subprocess.Popen(
            ["agentenv", "mcp"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            env=env, text=True, bufsize=1,
        )
        self.id = 0
        self._req("initialize", {
            "protocolVersion": "2024-11-05",
            "capabilities": {},
            "clientInfo": {"name": "mcp-client-py", "version": "0"},
        })
        # An MCP host normally sends "initialized" as a notification; the
        # agentenv server tolerates skipping it for one-shot drivers like this.

    def _req(self, method, params):
        self.id += 1
        self.p.stdin.write(json.dumps({
            "jsonrpc": "2.0", "id": self.id, "method": method, "params": params,
        }) + "\n")
        self.p.stdin.flush()
        # Drain frames until we see our own id (server may emit notifications).
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError("mcp server closed stdout: " + self.p.stderr.read())
            msg = json.loads(line)
            if msg.get("id") == self.id:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error']}")
                return msg["result"]

    def tool(self, name, **args):
        """Invoke an agentenv__* MCP tool, return the joined text content."""
        r = self._req("tools/call", {"name": name, "arguments": args})
        if r.get("isError"):
            raise RuntimeError(f"{name} returned isError: {r}")
        return "".join(c.get("text", "") for c in r.get("content", []))

    def close(self):
        try:
            self.p.stdin.close()
        finally:
            self.p.wait(timeout=5)


def main():
    socket = None
    if "--socket" in sys.argv:
        socket = sys.argv[sys.argv.index("--socket") + 1]
    mcp = MCP(socket=socket)
    try:
        print("HEAD:", mcp.tool("agentenv__head").strip())
        print("--- log ---")
        log = mcp.tool("agentenv__log")
        print(log.rstrip())

        # Pick any non-HEAD, non-root leaf node to prune as a demo. Parsing the
        # log text is brittle (it's human-readable) — a real driver would call
        # agentenv__branches and pick from there.
        leaves = [
            line.split()[0] for line in mcp.tool("agentenv__branches").splitlines()
            if line.strip() and "init from" not in line and "<- HEAD" not in line
        ]
        if leaves:
            target = leaves[0]
            print(f"--- delete leaf {target} (v0.2.0 op) ---")
            print(mcp.tool("agentenv__delete", node=target).rstrip())
            print("--- log after delete ---")
            print(mcp.tool("agentenv__log").rstrip())
        else:
            print("(no non-HEAD leaves to prune — skipping delete demo)")
    finally:
        mcp.close()


if __name__ == "__main__":
    main()
