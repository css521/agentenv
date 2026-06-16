# Deploying on Kubernetes

`agent-pod.yaml` runs an unmodified agent with a rewindable whole environment in an
**unprivileged** pod (no `privileged`, no added capabilities).

## The one hard requirement: unprivileged user namespaces

agentenv's rootless backend creates a nested user namespace. Confirm the node
allows it (run in any pod scheduled there):

```sh
unshare -Urm true; echo $?      # 0 = allowed
```

If that works, mirror that pod's security settings. If it fails, you need:

| Symptom | Fix |
|---|---|
| `clone ... operation not permitted` | `securityContext.seccompProfile.type: Unconfined` (RuntimeDefault blocks nested userns) |
| Fails on Ubuntu 22.04/24.04 nodes | AppArmor `Unconfined` (annotation, or `appArmorProfile.type: Unconfined` on v1.30+) |
| Admission webhook rejects Unconfined seccomp | Namespace is PodSecurity `restricted`; relabel to `baseline` |
| Truly no userns (locked node) | Not possible in-pod — use a `runtimeClass` microVM (Kata/gVisor) or a CSI-snapshot backend |

No `CAP_SYS_ADMIN`, no `privileged`, no host mounts are required.

## Rolling back (out-of-band, never touches the agent)

The control socket lives at `$AGENTENV_ROOT/agentenv.sock` inside the pod. Drive it
with the `agentenv ctl` client via `kubectl exec`:

```sh
kubectl exec my-agent -- agentenv ctl log                 # list snapshot nodes
kubectl exec my-agent -- agentenv ctl checkout <node-id>  # roll the whole env back
kubectl exec my-agent -- agentenv ctl diff <a> <b>        # inspect what changed
```

On checkout the agent process is killed and relaunched from the restored
environment; the agent's loop/state should live in your orchestrator (outside the
pod), which is the standard rewindable-sandbox model.

## Orchestrator-driven rollback (sidecar)

For a programmatic controller, add a sidecar container that shares the
`agentenv-data` volume (so it sees the same socket) and speaks the JSON protocol
(or shells out to `agentenv ctl`). It decides when to roll back / branch based on
whatever signal you have (test failures, evaluation, etc.). See `examples/` for
Python/Go/Java socket clients.

## Storage

`AGENTENV_ROOT` (default `/var/lib/agentenv`) holds the one-time seeded rootfs copy
plus snapshot diffs.

- **emptyDir** (in the manifest): ephemeral, per-pod history; simplest.
- **PVC**: persists history across pod restarts.
- Any filesystem works (the copy backend is fs-agnostic); on slow network storage,
  the first `init --from /` copy is the main cost — later snapshots are incremental.
