//go:build linux

// Package repo is the orchestration layer: it ties dag (metadata) + btrfs
// (snapshots) + image (rootfs) + sandbox (execution) together and exposes the
// high-level operations init/exec/spawn/commit/checkout/log/gc.
//
// A long-lived agent (see `agentenv serve`) keeps one *Repo for its whole
// session. Background processes started with Spawn are tracked in memory; on
// Checkout they are killed (the agent process itself, living in the control-root,
// is never touched). This realizes "agent survives, other processes reset".
//
// On-disk layout (AGENTENV_ROOT, default /agentfs, must live on btrfs):
//
//	<root>/
//	  nodes/<id>/   one read-only snapshot per immutable node
//	  work/current/ the live writable inner-env subvolume (commands run here)
//	  logs/<id>.log output of background (spawned) processes
//	  meta.json     commit-DAG metadata
package repo

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/css521/agentenv/internal/backend"
	"github.com/css521/agentenv/internal/dag"
	"github.com/css521/agentenv/internal/image"
)

// Proc is a tracked background process running in the inner-env.
type Proc struct {
	PID     int
	Args    []string
	Started time.Time
	Log     string

	cmd *exec.Cmd // handle for Wait/Kill
}

type Repo struct {
	root string
	dag  *dag.Repo
	be   *backend.Backend // pluggable runner + snapshotter (btrfs / copy-rootless)

	mu    sync.Mutex
	procs map[int]*Proc // background processes started via Spawn
	fg    *exec.Cmd     // foreground interactive agent (supervise PTY mode); killAll kills it too

	opMu          sync.Mutex // serializes DAG/work mutations (Commit/Checkout/auto-capture/retention)
	cap           *Capturer  // background auto-snapshot loop (nil unless started)
	keepRecent    int        // retention: number of most-recent non-structural nodes to always keep
	checkoutCount int        // number of checkouts performed (for the supervisor)
	preserveProcs bool       // self-rollback mode: checkout reverts in place without killing processes
}

// SetPreserveProcs enables self-rollback mode: Checkout reverts the working
// rootfs in place WITHOUT killing running processes, so the agent that
// triggered the rollback survives and observes the reverted environment.
func (r *Repo) SetPreserveProcs(v bool) { r.preserveProcs = v }

// token returns the backend's opaque change token for the working rootfs — the
// signal the Capturer polls (btrfs generation / copy fingerprint).
func (r *Repo) token() (string, error) { return r.be.Snapshotter.Token() }

// workRoot is the writable rootfs path the Runner executes in.
func (r *Repo) workRoot() string { return r.be.Snapshotter.WorkRoot() }

func Open(root string, be *backend.Backend) (*Repo, error) {
	d, err := dag.Load(root)
	if err != nil {
		return nil, err
	}
	keep := 30
	if v := os.Getenv("AGENTENV_KEEP_RECENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			keep = n
		}
	}
	r := &Repo{root: root, dag: d, be: be, procs: map[int]*Proc{}, keepRecent: keep}
	r.reconcile()
	return r, nil
}

// reconcile drops DAG nodes whose on-disk snapshot is missing (e.g. after a crash
// between freezing a snapshot and saving metadata, or vice versa), re-linking
// their children to the nearest surviving ancestor so the tree stays consistent.
func (r *Repo) reconcile() {
	nodes := r.dag.Nodes
	missing := map[string]bool{}
	for id := range nodes {
		if _, err := os.Stat(r.be.Snapshotter.NodePath(id)); err != nil {
			missing[id] = true
		}
	}
	if len(missing) == 0 {
		return
	}
	// Nearest surviving ancestor, following the original parent links.
	survAnc := func(id string) string {
		for id != "" && missing[id] {
			id = nodes[id].Parent
		}
		return id
	}
	newParent := map[string]string{}
	for id, n := range nodes {
		if !missing[id] {
			newParent[id] = survAnc(n.Parent)
		}
	}
	newHead := survAnc(r.dag.Head)

	for id := range missing {
		delete(nodes, id)
	}
	for id, n := range nodes {
		n.Parent = newParent[id]
		n.Children = nil
	}
	for id, n := range nodes {
		if n.Parent != "" {
			p := nodes[n.Parent]
			p.Children = append(p.Children, id)
		}
	}
	r.dag.Head = newHead
	if err := r.dag.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "agentenv: WARN reconcile could not persist meta.json: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "agentenv: reconciled %d dangling node(s) with missing snapshots\n", len(missing))
}

// Backend returns the active backend (for display).
func (r *Repo) Backend() *backend.Backend { return r.be }

func (r *Repo) logsDir() string { return filepath.Join(r.root, "logs") }

// newID generates a fresh node ID — 96 bits of randomness (a single shot has a
// negligible collision probability even at millions of nodes), checks
// crypto/rand actually delivered, and rejects any value that already exists in
// the DAG. The old version returned 48 bits with the rand error swallowed; on a
// rand failure it produced all-zeros every time → dag.Add silently overwrote
// the existing node and Freeze removed its snapshot, losing history without a
// peep. Mostly theoretical, but on a tool that auto-snapshots a long agent
// session, a quiet history-loss bug is not OK.
//
// Holds opMu for the existence check; callers already hold it for the
// surrounding mutation (Commit / Init / etc.), so this is a no-op there.
func (r *Repo) newID() string {
	for tries := 0; tries < 8; tries++ {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("crypto/rand failure: " + err.Error()) // never silently produce zeros
		}
		id := hex.EncodeToString(b[:])
		if _, taken := r.dag.Nodes[id]; !taken {
			return id
		}
	}
	panic("agentenv: 8 consecutive ID collisions — your crypto/rand is broken")
}

// InitTarball builds the root node by extracting a .tar / .tar.gz at src
// (filesystem path or http(s) URL) into the working rootfs. Use this for demo
// rootfs tarballs and air-gapped setups; for production, use InitFrom to seed
// from the agent's own running container.
func (r *Repo) InitTarball(src string) (*dag.Node, error) {
	if len(r.dag.Nodes) > 0 {
		return nil, fmt.Errorf("already initialized (%d nodes exist)", len(r.dag.Nodes))
	}

	// 1) Fresh empty working rootfs; extract the tarball into it.
	if err := r.be.Snapshotter.NewEmptyWork(); err != nil {
		return nil, err
	}
	fmt.Printf("extracting tarball %s ...\n", src)
	if err := image.MaterializeTarball(src, r.workRoot()); err != nil {
		return nil, err
	}

	// 2) Freeze as the immutable root node.
	id := r.newID()
	if err := r.be.Snapshotter.Freeze(id, ""); err != nil {
		return nil, err
	}

	node := &dag.Node{
		ID:        id,
		Message:   "init from " + src,
		Command:   "init --tarball " + src,
		CreatedAt: time.Now(),
		Meta:      map[string]string{"tarball": src},
	}
	r.dag.Add(node)
	r.dag.Head = id
	return node, r.dag.Save()
}

// InitFrom builds the root node by seeding the rootfs from an existing directory
// (e.g. "/" — the current container filesystem — or a workspace dir) instead of
// downloading a base image. This makes the managed environment BE the agent's
// real environment, so the agent can run inside it (system installs + files all
// captured) without changing its code — only its launch command.
func (r *Repo) InitFrom(src string) (*dag.Node, error) {
	if len(r.dag.Nodes) > 0 {
		return nil, fmt.Errorf("already initialized (%d nodes exist)", len(r.dag.Nodes))
	}
	if err := r.be.Snapshotter.NewEmptyWork(); err != nil {
		return nil, err
	}
	exclude := map[string]bool{
		filepath.Clean(r.root): true, // never copy the snapshot store into itself
	}
	if filepath.Clean(src) == "/" {
		// Virtual/host filesystems: the sandbox mounts these fresh at run time.
		for _, p := range []string{"/proc", "/sys", "/dev"} {
			exclude[p] = true
		}
	}
	fmt.Fprintf(os.Stderr, "seeding rootfs from %s (one-time copy)...\n", src)
	// Throttled progress: `init --from /` on a multi-GB image takes minutes,
	// and a silent copy is indistinguishable from a hang. Repaint a single
	// stderr line ~4×/s with running file + byte counts. Throttling keeps the
	// hot copy loop from doing a syscall per file just to print.
	start := time.Now()
	var last time.Time
	var totFiles int
	var totBytes int64
	prog := func(files int, bytes int64) {
		totFiles, totBytes = files, bytes
		now := time.Now()
		if now.Sub(last) < 250*time.Millisecond {
			return
		}
		last = now
		fmt.Fprintf(os.Stderr, "\r  %d files, %s copied...", files, humanBytes(bytes))
	}
	if err := image.SeedDir(src, r.workRoot(), exclude, prog); err != nil {
		fmt.Fprintln(os.Stderr)
		return nil, err
	}
	// \r + trailing spaces overwrite the last (possibly longer) progress line.
	fmt.Fprintf(os.Stderr, "\r  %d files, %s copied in %s%s\n",
		totFiles, humanBytes(totBytes), time.Since(start).Round(time.Second), strings.Repeat(" ", 12))
	id := r.newID()
	if err := r.be.Snapshotter.Freeze(id, ""); err != nil {
		return nil, err
	}
	node := &dag.Node{
		ID:        id,
		Message:   "init from " + src,
		Command:   "init --from " + src,
		CreatedAt: time.Now(),
		Meta:      map[string]string{"from": src},
	}
	r.dag.Add(node)
	r.dag.Head = id
	return node, r.dag.Save()
}

// resetWorking re-derives the working rootfs from node id.
func (r *Repo) resetWorking(id string) error {
	return r.be.Snapshotter.RestoreWork(id)
}

// ensureWorking guarantees a working rootfs exists (derived from HEAD if missing).
func (r *Repo) ensureWorking() error {
	if r.be.Snapshotter.WorkExists() {
		return nil
	}
	if r.dag.Head == "" {
		return fmt.Errorf("not initialized; run init first")
	}
	return r.be.Snapshotter.RestoreWork(r.dag.Head)
}

// Exec runs a real command in the current working volume (foreground) and waits.
// It returns the command's exit code and an error. A command that runs to
// completion with a non-zero status is NOT an agentenv error: it returns
// (code, nil). A non-nil error means agentenv itself failed (could not set up
// the volume, start the process, etc.).
func (r *Repo) Exec(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if err := r.ensureWorking(); err != nil {
		return -1, err
	}
	err := r.be.Runner.Run(r.workRoot(), args, sandboxEnv(), stdin, stdout, stderr)
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil // command ran, returned non-zero
	}
	return -1, err // real failure (setup/start)
}

// sandboxEnv returns the env an agent-side command should see. It's an explicit
// allow-list (PATH, HOME, TERM, USER, SHELL, LANG, LC_*) — NOT the full parent
// env. The agentenv daemon/supervise process typically holds secrets (cloud
// tokens, K8s service-account tokens, ANTHROPIC_API_KEY, etc.) in its env, and
// the agent's commands MUST NOT see them by default.
//
// Two ways to forward extra vars through the allow-list:
//
//   - AGENTENV_PASS_<VAR>=val  → forwards <VAR>=val. Carries the value inline;
//     good for one-offs (AGENTENV_PASS_HTTPS_PROXY=http://...).
//   - AGENTENV_FORWARD=A,B,C   → forwards the CURRENT values of vars A, B, C by
//     name. Good when the values are already in agentenv's env (e.g. a wrapper
//     image bakes `ENV AGENTENV_FORWARD=ANTHROPIC_API_KEY,...` and the user
//     just passes `-e ANTHROPIC_API_KEY=...`). Names support a trailing `*`
//     wildcard, e.g. `ANTHROPIC_*`.
//
// Both are explicit and auditable; nothing leaks without being named.
func sandboxEnv() []string {
	allow := []string{"PATH", "HOME", "TERM", "USER", "SHELL", "LANG", "LOGNAME"}
	out := make([]string, 0, len(allow)+4)
	for _, k := range allow {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	forward := parseForwardList(os.Getenv("AGENTENV_FORWARD"))
	for _, kv := range os.Environ() {
		// LC_* (locale) is whitelisted as a family.
		if strings.HasPrefix(kv, "LC_") {
			out = append(out, kv)
			continue
		}
		// AGENTENV_PASS_<VAR>=val → <VAR>=val forwarded to the agent's command.
		if name, val, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "AGENTENV_PASS_") {
			out = append(out, strings.TrimPrefix(name, "AGENTENV_PASS_")+"="+val)
			continue
		}
		// AGENTENV_FORWARD names: forward the var verbatim if it matches.
		if name, _, ok := strings.Cut(kv, "="); ok && forwardMatch(forward, name) {
			out = append(out, kv)
		}
	}
	if !envHas(out, "PATH") {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !envHas(out, "HOME") {
		out = append(out, "HOME=/root")
	}
	return out
}

func envHas(env []string, key string) bool {
	p := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, p) {
			return true
		}
	}
	return false
}

// parseForwardList splits AGENTENV_FORWARD ("A,B,ANTHROPIC_*") into trimmed,
// non-empty patterns.
func parseForwardList(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// forwardMatch reports whether name matches any pattern, where a trailing '*'
// is a prefix wildcard (e.g. "ANTHROPIC_*" matches "ANTHROPIC_API_KEY").
func forwardMatch(patterns []string, name string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
		} else if p == name {
			return true
		}
	}
	return false
}

// Shell runs an interactive shell inside the current working volume on a PTY,
// returning its exit code. File changes made in the shell are auto-captured by the
// Capturer running alongside it — no per-command wrapping or commit needed.
func (r *Repo) Shell(args []string) (int, error) {
	if err := r.ensureWorking(); err != nil {
		return -1, err
	}
	return r.be.Runner.Shell(r.workRoot(), args, sandboxEnv())
}

// RunInteractive runs an interactive agent on a PTY (supervise's TTY mode) and
// registers it as the foreground process so a concurrent Checkout() can kill
// it. Returns the agent's exit code. The supervisor calls this in a loop:
// after a rollback kills the agent, CheckoutCount increases and the supervisor
// re-invokes RunInteractive to relaunch from the restored environment.
func (r *Repo) RunInteractive(args []string) (int, error) {
	if err := r.ensureWorking(); err != nil {
		return -1, err
	}
	code, err := r.be.Runner.ShellHook(r.workRoot(), args, sandboxEnv(), func(cmd *exec.Cmd) {
		r.mu.Lock()
		r.fg = cmd
		r.mu.Unlock()
	})
	r.mu.Lock()
	r.fg = nil
	r.mu.Unlock()
	return code, err
}

// Spawn starts a background process in the current working volume, tracks it, and
// returns its handle. Output is written to logs/<id>.log. A reaper goroutine
// removes it from the table when it exits.
func (r *Repo) Spawn(args []string) (*Proc, error) {
	if err := r.ensureWorking(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.logsDir(), 0o755); err != nil {
		return nil, err
	}
	logPath := filepath.Join(r.logsDir(), r.newID()+".log")
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd, err := r.be.Runner.Start(r.workRoot(), args, sandboxEnv(), lf)
	if err != nil {
		lf.Close()
		return nil, err
	}
	p := &Proc{PID: cmd.Process.Pid, Args: args, Started: time.Now(), Log: logPath, cmd: cmd}
	r.mu.Lock()
	r.procs[p.PID] = p
	r.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		lf.Close()
		r.mu.Lock()
		delete(r.procs, p.PID)
		r.mu.Unlock()
	}()
	return p, nil
}

// Procs returns a snapshot of the tracked background processes, sorted by PID.
func (r *Repo) Procs() []*Proc {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Proc, 0, len(r.procs))
	for _, p := range r.procs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// ProcAlive reports whether a tracked background process is still running.
func (r *Repo) ProcAlive(pid int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.procs[pid]
	return ok
}

// CheckoutCount returns how many checkouts (rollbacks) have happened — used by the
// supervisor to tell a rollback-kill from a normal agent exit.
func (r *Repo) CheckoutCount() int {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.checkoutCount
}

// Kill terminates one tracked background process (and its inner-env PID namespace).
func (r *Repo) Kill(pid int) error {
	r.mu.Lock()
	p := r.procs[pid]
	r.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no such process %d", pid)
	}
	return p.cmd.Process.Kill()
}

// killAll kills every tracked background process and waits (briefly) for the
// reaper goroutines to drain the table, so the working subvolume is no longer in
// use before it gets swapped.
func (r *Repo) killAll() {
	r.mu.Lock()
	cmds := make([]*exec.Cmd, 0, len(r.procs))
	for _, p := range r.procs {
		cmds = append(cmds, p.cmd)
	}
	// The foreground interactive agent (supervise PTY mode) isn't in the procs
	// table — kill it explicitly so a rollback can swap the rootfs out from
	// under it. The supervise loop sees it die, sees checkoutCount bumped, and
	// relaunches it from the restored env.
	fg := r.fg
	r.mu.Unlock()
	if fg != nil && fg.Process != nil {
		_ = fg.Process.Kill()
	}
	for _, c := range cmds {
		_ = c.Process.Kill()
	}
	// Wait up to ~5s for reapers to remove entries as processes die.
	for i := 0; i < 500; i++ {
		r.mu.Lock()
		n := len(r.procs)
		r.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Commit freezes the working volume as a child of HEAD and advances HEAD.
func (r *Repo) Commit(message, command string) (*dag.Node, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if err := r.ensureWorking(); err != nil {
		return nil, err
	}
	id := r.newID()
	if err := r.be.Snapshotter.Freeze(id, r.dag.Head); err != nil {
		return nil, err
	}
	node := &dag.Node{
		ID:        id,
		Parent:    r.dag.Head,
		Message:   message,
		Command:   command,
		CreatedAt: time.Now(),
	}
	r.dag.Add(node)
	r.dag.Head = id
	return node, r.dag.Save()
}

// ResolveRef turns a tag name OR a node ID (or unique ID prefix) into a node ID.
// It returns "" if nothing matches. Used by Checkout, Show, Diff, and the API so
// the agent/operator can refer to nodes by short, memorable names.
// ResolveRef resolves a ref (tag, full ID, or unique ID prefix) to a node ID,
// taking opMu around the lookup. Returns "" if not found / ambiguous / unsafe.
//
// IMPORTANT: ResolveRef takes the lock and releases it before returning. Any
// caller that needs to act on the resolved ID atomically (Checkout / Show /
// Diff / SetTag — they all read the DAG further after resolving) MUST use
// resolveRefLocked while already holding opMu, otherwise a concurrent op can
// invalidate the resolution between the resolve and the act on it (TOCTOU).
func (r *Repo) ResolveRef(ref string) string {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.resolveRefLocked(ref)
}

// resolveRefLocked is the lock-free guts of ResolveRef. Caller must hold opMu.
func (r *Repo) resolveRefLocked(ref string) string {
	if ref == "" {
		return ""
	}
	// Reject anything that could traverse paths if the result reached a backend
	// method that joins it into a filesystem path. Legitimate refs are either
	// hex node IDs or user-chosen tag names — none of which need '/' or NUL.
	if strings.ContainsAny(ref, "/\\\x00") {
		return ""
	}
	if id, ok := r.dag.Tags[ref]; ok {
		if _, exists := r.dag.Nodes[id]; exists {
			return id
		}
	}
	if _, exists := r.dag.Nodes[ref]; exists {
		return ref // exact ID match
	}
	// Unique prefix match (handy for the 12-char node IDs we print).
	var match string
	for id := range r.dag.Nodes {
		if len(id) >= len(ref) && id[:len(ref)] == ref {
			if match != "" {
				return "" // ambiguous
			}
			match = id
		}
	}
	return match
}

// SetTag assigns name to a node ID (or another ref). Empty id removes the tag.
// Tag names cannot collide with node IDs (12-hex chars) — guarded here.
func (r *Repo) SetTag(name, ref string) error {
	if name == "" {
		return fmt.Errorf("tag name is empty")
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if ref == "" {
		delete(r.dag.Tags, name)
		return r.dag.Save()
	}
	id, ok := r.dag.Tags[ref]
	if !ok {
		if _, exists := r.dag.Nodes[ref]; exists {
			id = ref
		}
	}
	if id == "" {
		return fmt.Errorf("unknown ref: %s", ref)
	}
	r.dag.Tags[name] = id
	return r.dag.Save()
}

// Tags returns a copy of the tag table (name → node ID), sorted by name.
func (r *Repo) Tags() map[string]string {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	out := make(map[string]string, len(r.dag.Tags))
	for k, v := range r.dag.Tags {
		out[k] = v
	}
	return out
}

// Checkout rolls back / switches to any node (also accepts tag names or unique ID
// prefixes via ResolveRef). It kills the inner-env processes, rebuilds the
// working volume from that node, and points HEAD there. A later commit then forks
// under it, forming a tree. The agent process is untouched.
func (r *Repo) Checkout(ref string) error {
	// Pause auto-capture so it does not snapshot the half-swapped subvolume, and
	// rebaseline it afterwards to the restored generation.
	if r.cap != nil {
		r.cap.pause()
		defer r.cap.resumeBaseline()
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()

	// Resolve the ref UNDER the same opMu we'll mutate under — closes the
	// TOCTOU window where a concurrent op could change the DAG between
	// resolution and checkout.
	id := r.resolveRefLocked(ref)
	if id == "" {
		return fmt.Errorf("unknown ref: %s", ref)
	}

	r.checkoutCount++ // bump before killing so a supervisor sees rollback vs normal exit
	if r.preserveProcs {
		// Self-rollback mode: revert the rootfs in place WITHOUT killing
		// processes, so the agent that requested the rollback (via MCP/ctl)
		// keeps running and sees the reverted environment. Viable because
		// RestoreWork (copy backend) is an in-place sync that touches only
		// changed files — the agent's own runtime (binary/libc/node) is
		// unchanged across a session's snapshots, so it survives while its work
		// files revert underneath it.
		if err := r.resetWorking(id); err != nil {
			return err
		}
		r.dag.Head = id
		return r.dag.Save()
	}
	r.killAll() // other processes "roll back" = get killed
	if err := r.resetWorking(id); err != nil {
		return err
	}
	r.dag.Head = id
	return r.dag.Save()
}

// Delete removes a node from the DAG and deletes its on-disk snapshot. Its
// children are re-parented to its parent so the tree stays connected (deleting
// a middle node keeps its descendants). Refuses to delete the current HEAD (it
// would orphan the live working tree — checkout elsewhere first) or the last
// remaining node. Deleting a node's snapshot is safe even if children
// hardlink-share its files: the shared inodes stay alive as long as a child
// still references them (same reason GC is safe).
func (r *Repo) Delete(ref string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	id := r.resolveRefLocked(ref)
	if id == "" {
		return fmt.Errorf("unknown ref: %s", ref)
	}
	if id == r.dag.Head {
		return fmt.Errorf("refusing to delete the current HEAD node %s — `agentenv checkout <other>` first", short(id))
	}
	if len(r.dag.Nodes) <= 1 {
		return fmt.Errorf("refusing to delete the only node")
	}
	if _, ok := r.dag.Delete(id); !ok {
		return fmt.Errorf("no such node: %s", id)
	}
	// Persist the metadata first (consistent DAG), then remove the snapshot dir.
	// If the rm fails afterward it's a harmless orphan that `gc` reclaims.
	if err := r.dag.Save(); err != nil {
		return err
	}
	if err := r.be.Snapshotter.DeleteNode(id); err != nil {
		fmt.Fprintf(os.Stderr, "agentenv: WARN deleted node %s from the DAG but could not remove its snapshot (gc will): %v\n", short(id), err)
	}
	return nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Head returns the current HEAD node ID.
func (r *Repo) Head() string { return r.dag.Head }

// Nodes returns all nodes oldest-first (for structured log output).
func (r *Repo) Nodes() []*dag.Node {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	out := make([]*dag.Node, 0, len(r.dag.Nodes))
	for _, n := range r.dag.Nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Leaves returns the branch tips (nodes with no children), oldest first. Each tip
// is a distinct explored end-state — the unit of branch exploration.
func (r *Repo) Leaves() []*dag.Node {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	var out []*dag.Node
	for _, n := range r.dag.Nodes {
		if len(n.Children) == 0 {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Tree renders the whole commit-DAG, marking HEAD.
func (r *Repo) Tree() string {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	var sb []byte
	var walk func(id, prefix string, last bool)
	walk = func(id, prefix string, last bool) {
		n := r.dag.Nodes[id]
		if n == nil {
			return
		}
		branch := "├─ "
		if last {
			branch = "└─ "
		}
		marker := ""
		if id == r.dag.Head {
			marker = "  <- HEAD"
		}
		sb = append(sb, fmt.Sprintf("%s%s%s  %q%s\n", prefix, branch, id, n.Message, marker)...)
		childPrefix := prefix + "│  "
		if last {
			childPrefix = prefix + "   "
		}
		for i, c := range n.Children {
			walk(c, childPrefix, i == len(n.Children)-1)
		}
	}
	roots := r.dag.Roots()
	for i, root := range roots {
		walk(root.ID, "", i == len(roots)-1)
	}
	return string(sb)
}

// GC deletes node subvolumes on disk that are not referenced by the DAG (orphans),
// e.g. nodes dropped by retention. This is where sparsified snapshots are actually
// freed from disk.
func (r *Repo) GC() ([]string, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.gcLocked()
}

func (r *Repo) gcLocked() ([]string, error) {
	ids, err := r.be.Snapshotter.NodeIDsOnDisk()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, id := range ids {
		if _, ok := r.dag.Get(id); !ok {
			if err := r.be.Snapshotter.DeleteNode(id); err != nil {
				return removed, err
			}
			removed = append(removed, id)
		}
	}
	return removed, nil
}

// Retain sparsifies the DAG: it keeps all "recent" and structural nodes and thins
// older interior nodes on an exponential (DVR ring) curve, so the node count stays
// ~ keepRecent + log2(total). Dropped nodes are removed from the DAG (their
// children are re-linked to the nearest surviving ancestor); their on-disk
// snapshots become orphans, reclaimed by GC. Returns the dropped node IDs.
//
// Always kept:
//   - the root and HEAD
//   - branch points (>=2 children) and leaves (branch tips) — pruning these would
//     lose reachable states
//   - the newest keepRecent non-structural nodes
//
// Then, among older non-structural nodes ordered newest->oldest by rank i, only
// ranks where i is 2^k-1 (0,1,3,7,15,31,...) are kept.
func (r *Repo) Retain(keepRecent int) []string {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.retainLocked(keepRecent)
}

func (r *Repo) retainLocked(keepRecent int) []string {
	nodes := r.dag.Nodes
	keep := make(map[string]bool, len(nodes))

	// Structural protections.
	for id, n := range nodes {
		if n.Parent == "" || len(n.Children) == 0 || len(n.Children) >= 2 || id == r.dag.Head {
			keep[id] = true
		}
	}

	// Non-structural candidates, newest first.
	var cand []*dag.Node
	for id, n := range nodes {
		if !keep[id] {
			cand = append(cand, n)
		}
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].CreatedAt.After(cand[j].CreatedAt) })
	for i, n := range cand {
		if i < keepRecent || (i&(i+1)) == 0 { // recent, or an exponential boundary rank
			keep[n.ID] = true
		}
	}

	// Collect drops.
	var dropped []string
	for id := range nodes {
		if !keep[id] {
			dropped = append(dropped, id)
		}
	}
	if len(dropped) == 0 {
		return nil
	}

	// Re-link each surviving node to its nearest surviving ancestor, then rebuild
	// children lists.
	for id := range keep {
		n := nodes[id]
		anc := n.Parent
		for anc != "" && !keep[anc] {
			anc = nodes[anc].Parent
		}
		n.Parent = anc
		n.Children = nil
	}
	for _, id := range dropped {
		delete(nodes, id)
	}
	for id := range keep {
		n := nodes[id]
		if n.Parent != "" {
			p := nodes[n.Parent]
			p.Children = append(p.Children, id)
		}
	}
	// Stable child ordering for a deterministic tree view.
	for _, n := range nodes {
		sort.Slice(n.Children, func(i, j int) bool {
			return nodes[n.Children[i]].CreatedAt.Before(nodes[n.Children[j]].CreatedAt)
		})
	}
	if err := r.dag.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "agentenv: WARN retention could not persist meta.json: %v\n", err)
	}
	return dropped
}

// applyRetention runs a retention pass and then reclaims the freed snapshots from
// disk. Called automatically by the Capturer after each auto-snapshot.
func (r *Repo) applyRetention() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if len(r.dag.Nodes) <= r.keepRecent+1 {
		return // nothing to thin yet
	}
	if dropped := r.retainLocked(r.keepRecent); len(dropped) > 0 {
		_, _ = r.gcLocked()
	}
}
