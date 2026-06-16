//go:build linux

package repo

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/css521/agentenv/internal/dag"
)

// truncate returns s clipped to max with a trailing ellipsis when truncation
// happens. Used to keep CLI/tree output scannable for long shell commands.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// runExit runs cmd in workspace and returns its exit code (or -1 if the runner
// itself failed before the command exited). Mirrors the (code, err) shape of
// repo.Exec for callers that only care about the exit status.
func (r *Repo) runExit(workspace string, shellCmd string, dump *bytes.Buffer) (int, error) {
	err := r.be.Runner.Run(workspace, []string{"bash", "-lc", shellCmd}, sandboxEnv(), nil, dump, dump)
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil // command ran, returned non-zero — not a runner error
	}
	return -1, err
}

// CandidateResult is what one branch of a tournament produced.
type CandidateResult struct {
	Name     string // human label ("+", "B", "v1.2", ...)
	Cmd      string // shell command run inside the env to produce this branch
	Node     string // resulting branch tip (empty on build failure)
	TestExit int    // exit code of the test command on this branch (-1 if not run)
	Err      string // non-empty if the branch failed to build or test
}

// TournamentResult is the outcome of one tournament call.
type TournamentResult struct {
	Base       string            // resolved base node id
	Candidates []CandidateResult // one per candidate, in input order
	Winner     string            // candidate name whose test exit==0 (first such by input order)
	WinnerNode string            // the winning node id, "" if none
}

// Tournament forks the environment N times from base, runs each candidate
// concurrently in its own isolated workspace, then runs test concurrently in
// each, and picks the first candidate (by input order, for determinism) whose
// test exits 0. The main work/current is NOT touched during the run; only the
// final HEAD reposition (always done at the end) affects it.
//
//   - keep==true && there is a winner → HEAD lands on the winner's node.
//   - otherwise → HEAD lands on the base.
//
// Build and test phases are parallel; the only serialization point is a brief
// opMu hold per branch around assigning a node ID and adding to the DAG. The
// backend workspace primitives are concurrent-safe (each touches a distinct
// path), and the sandbox runner does too (each invocation forks its own child
// in its own namespaces).
func (r *Repo) Tournament(baseRef, test string, candidates []struct{ Name, Cmd string }, keep bool) (*TournamentResult, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("at least one candidate is required")
	}
	// Resolve baseRef + read HEAD under one opMu hold so we never see a base
	// that's been deleted by retention/gc between resolve and use.
	r.opMu.Lock()
	if baseRef == "" {
		baseRef = r.dag.Head
	}
	base := r.resolveRefLocked(baseRef)
	r.opMu.Unlock()
	if base == "" {
		return nil, fmt.Errorf("unknown base ref: %s", baseRef)
	}

	// Make the capturer ignore the run — its watcher only sees work/current
	// (untouched here), but we want to be doubly sure new auto-snapshots don't
	// race with the parallel commits we add to the DAG.
	if r.cap != nil {
		r.cap.pause()
		defer r.cap.resumeBaseline()
	}

	res := &TournamentResult{Base: base, Candidates: make([]CandidateResult, len(candidates))}

	type built struct {
		ws       string // workspace path (kept alive through the test phase)
		nodeID   string
		buildErr error
	}
	builds := make([]built, len(candidates))

	// --- Phase 1: build each candidate in parallel ---
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		go func(i int, c struct{ Name, Cmd string }) {
			defer wg.Done()
			ws, err := r.be.Snapshotter.NewWorkspace(base)
			if err != nil {
				builds[i].buildErr = fmt.Errorf("workspace: %w", err)
				return
			}
			builds[i].ws = ws
			var dump bytes.Buffer
			code, err := r.runExit(ws, c.Cmd, &dump)
			if err != nil {
				builds[i].buildErr = fmt.Errorf("exec: %w (%s)", err, dump.String())
				return
			}
			// A build command that ran but exited non-zero is a failed candidate
			// — the previous code dropped this on the floor and treated such a
			// candidate as a successful build with the SAME content as base, so
			// it would frequently "win" the tournament by accident.
			if code != 0 {
				builds[i].buildErr = fmt.Errorf("build exited %d: %s", code, strings.TrimSpace(dump.String()))
				return
			}
			// Freeze under opMu (we need a unique DAG-assigned ID and an atomic
			// DAG mutation). The freeze itself is O(diff) on copy w/ reflink and
			// O(1) on btrfs, so contention is brief.
			r.opMu.Lock()
			nodeID := r.newID()
			if err := r.be.Snapshotter.FreezeFrom(ws, nodeID, base); err != nil {
				r.opMu.Unlock()
				builds[i].buildErr = fmt.Errorf("freeze: %w", err)
				return
			}
			r.dag.Add(&dag.Node{
				ID:     nodeID,
				Parent: base,
				// Trim Message so the agentenv log tree stays scannable when
				// candidates are long shell pipelines; the full text is still
				// available in Command for diff/show.
				Message:   truncate("tournament "+c.Name+": "+c.Cmd, 60),
				Command:   c.Cmd,
				CreatedAt: time.Now(),
			})
			r.opMu.Unlock()
			builds[i].nodeID = nodeID
		}(i, c)
	}
	wg.Wait()

	// Persist DAG once after the build phase (cheaper than saving per branch).
	r.opMu.Lock()
	if err := r.dag.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "agentenv: WARN tournament could not persist meta.json: %v\n", err)
	}
	r.opMu.Unlock()

	// --- Phase 2: run the test in each successful build, in parallel ---
	for i, c := range candidates {
		res.Candidates[i] = CandidateResult{Name: c.Name, Cmd: c.Cmd, Node: builds[i].nodeID, TestExit: -1}
		if builds[i].buildErr != nil {
			res.Candidates[i].Err = builds[i].buildErr.Error()
		}
	}
	wg = sync.WaitGroup{}
	for i := range builds {
		if builds[i].buildErr != nil || builds[i].ws == "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var dump bytes.Buffer
			code, err := r.runExit(builds[i].ws, test, &dump)
			if err != nil {
				res.Candidates[i].Err = fmt.Sprintf("test: %v (%s)", err, dump.String())
				return
			}
			res.Candidates[i].TestExit = code
		}(i)
	}
	wg.Wait()

	// Pick first passing candidate (input order = stable winner choice).
	for _, c := range res.Candidates {
		if c.TestExit == 0 && c.Err == "" {
			res.Winner = c.Name
			res.WinnerNode = c.Node
			break
		}
	}

	// --- Phase 3: cleanup workspaces; reposition HEAD ---
	for i := range builds {
		if builds[i].ws != "" {
			_ = r.be.Snapshotter.DeleteWorkspace(builds[i].ws)
		}
	}
	dest := base
	if keep && res.WinnerNode != "" {
		dest = res.WinnerNode
	}
	if err := r.parkHead(dest); err != nil {
		return res, fmt.Errorf("park HEAD on %s: %w", dest, err)
	}
	return res, nil
}

// parkHead repositions HEAD without re-entering Checkout's pause/resume of the
// Capturer (we already paused at the top of Tournament).
func (r *Repo) parkHead(id string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.checkoutCount++
	r.killAll()
	if err := r.resetWorking(id); err != nil {
		return err
	}
	r.dag.Head = id
	return r.dag.Save()
}
