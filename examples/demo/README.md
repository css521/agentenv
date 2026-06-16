# Killer demo

Two superpowers in ~30 seconds, **rootless and unprivileged**:

1. **Time-travel rollback of a whole environment** — the agent deletes a system
   binary (`/usr/local/bin/greet`), corrupts a system config (`/etc/motd`), and
   `rm -rf`s the project. One `agentenv checkout` restores *all of it* — not just
   git-tracked files.
2. **Branch exploration** — from one base, fork 3 candidate fixes into parallel
   branches, evaluate each, and keep the one that passes.

## Run it

```sh
docker run --rm --user 1001:1001 --security-opt seccomp=unconfined \
  -v "$PWD":/src:ro -e CGO_ENABLED=0 -e GOCACHE=/tmp/gocache -e HOME=/tmp \
  -w /src golang:1.26 bash /src/examples/demo/killer-demo.sh
```

(`--user 1001` + no `--privileged` = it runs exactly like a restricted pod.)

## Record a shareable cast / GIF

```sh
asciinema rec -c '<the docker run command above>' demo.cast
agg demo.cast demo.gif      # render to GIF — github.com/asciinema/agg
# or upload demo.cast to asciinema.org
```

## What it looks like

```
━━━ 2) The agent WRECKS the environment ━━━
greet: GONE
CORRUPTED
project: GONE

━━━ 3) Time travel: ONE command restores the WHOLE environment ━━━
$ agentenv checkout 3ca45f333fee
hello from greet
welcome v1
print(6*7)
RESTORED

━━━ 4) Branch exploration: try 3 fixes, keep the one that passes ━━━
  branch A fails
  branch B PASSES
  branch C fails
» kept branch B

└─ init from ubuntu
   └─ working: /usr/local/bin/greet + /etc/motd + project
      ├─ broken
      ├─ approach A -> 41
      ├─ approach B -> 42   <- HEAD
      └─ approach C -> 99
```
