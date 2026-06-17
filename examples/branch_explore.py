#!/usr/bin/env python3
"""Branch-exploration demo against the agentenv JSON API (unix socket).

An "agent" forks the environment from one base, tries N candidate approaches in
separate branches, evaluates each against a goal, then keeps the winning branch
and discards the rest — all through the socket, no agentenv CLI calls.

Usage: python3 branch_explore.py /path/to/agentenv.sock
"""
import json
import socket
import sys


class Env:
    """Thin client for the agentenv newline-JSON socket API."""

    def __init__(self, path):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(path)
        self.f = self.sock.makefile("rwb")

    def call(self, **req):
        """Send one request, read one terminal frame (works for non-exec ops)."""
        self.f.write((json.dumps(req) + "\n").encode())
        self.f.flush()
        resp = json.loads(self.f.readline())
        if not resp.get("ok"):
            raise RuntimeError(f"{req['op']} failed: {resp.get('error')}")
        return resp

    def exec(self, cmd, must_succeed=False, stream=None):
        """Run a shell command in the env. Reads streamed stdout/stderr frames
        until a terminal frame ({"ok":true,...} or {"error":...}) and returns the
        latter, with full stdout/stderr accumulated. Pass stream=sys.stdout to
        also echo live output as it arrives."""
        self.f.write((json.dumps({"op": "exec", "cmd": cmd}) + "\n").encode())
        self.f.flush()
        out, err = [], []
        while True:
            frame = json.loads(self.f.readline())
            if (s := frame.get("stdout")) is not None:
                out.append(s)
                if stream is not None:
                    stream.write(s); stream.flush()
                continue
            if (s := frame.get("stderr")) is not None:
                err.append(s)
                if stream is not None:
                    stream.write(s); stream.flush()
                continue
            if (msg := frame.get("error")):
                raise RuntimeError(f"exec failed: {msg}")
            if frame.get("ok"):
                r = {**frame, "stdout": "".join(out), "stderr": "".join(err)}
                if must_succeed and r.get("exit") != 0:
                    raise RuntimeError(
                        f"command failed (exit {r.get('exit')}): {cmd}\n{r['stderr']}{r['stdout']}"
                    )
                return r

    def head(self):
        return self.call(op="head")["head"]

    def checkout(self, node):
        return self.call(op="checkout", node=node)["head"]

    def delete(self, node):
        """Remove a node from the DAG (children re-parent). Used to prune
        dead-end exploration branches once we know they lost."""
        return self.call(op="delete", node=node)


def main():
    sock = sys.argv[1] if len(sys.argv) > 1 else "/agentfs/agentenv.sock"
    env = Env(sock)

    base = env.head()
    print("common base:", base)

    # Three candidate environments — each installs a different tool.
    approaches = {"A": "tree", "B": "jq", "C": "figlet"}
    apt = "apt-get -o APT::Sandbox::User=root install -y -qq"
    tips = {}
    for name, pkg in approaches.items():
        env.checkout(base)                       # fork from the common base
        env.exec(f"{apt} {pkg}", must_succeed=True)  # build this candidate
        tips[name] = env.head()                  # the branch tip (auto-snapshotted)
        if tips[name] == base:
            raise RuntimeError(f"approach {name}: no snapshot created (env unchanged?)")
        print(f"  explored {name}: installed {pkg} -> node {tips[name]}")

    # Goal: we want the environment where `jq` works.
    winner = None
    for name, tip in tips.items():
        env.checkout(tip)
        if env.exec("command -v jq >/dev/null")["exit"] == 0:
            winner = name
    print("winner:", winner)

    # Keep the winner; PRUNE the losing branches with `delete` (v0.2.0) so the
    # DAG doesn't keep dead-end snapshots forever. Children of a deleted node
    # re-parent to its parent — harmless here because the losers are leaves.
    env.checkout(tips[winner])
    for name, tip in tips.items():
        if name != winner:
            env.delete(tip)
            print(f"  pruned dead-end {name} (node {tip})")

    have = env.exec("command -v jq && (command -v tree || echo no-tree) && (command -v figlet || echo no-figlet)")
    print("final env:\n" + have["stdout"].rstrip())

    if winner == "B":
        print("PASS: agent explored 3 environments, kept the winner, pruned the losers")
    else:
        print("FAIL: unexpected winner", winner)
        sys.exit(1)


if __name__ == "__main__":
    main()
