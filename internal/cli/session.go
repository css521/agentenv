//go:build linux

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/css521/agentenv/internal/api"
	"github.com/css521/agentenv/internal/repo"
)

// cmdSupervise runs an unmodified agent INSIDE the managed environment, with
// auto-capture recording everything it does (any command, any language, any path)
// and a control socket for out-of-band rollback. On a rollback the agent process
// is killed and re-launched from the restored environment. This is the
// zero-intrusion injection: the agent calls no API and is unaware of agentenv.
func cmdSupervise(r *repo.Repo, args []string) error {
	agentArgs := after(args, "--")
	if len(agentArgs) == 0 {
		return fmt.Errorf("usage: agentenv supervise [--socket path] -- <agent command...>")
	}
	cap := r.StartCapturer()
	defer cap.Stop()

	// --self-rollback: let the supervised agent roll BACK ITSELF via MCP/ctl and
	// keep running. Two things change: (1) the control socket lives INSIDE the
	// sandbox (work/current/.agentenv/control.sock — an always-ignored dir), so
	// the agent can reach it; (2) Checkout reverts in place without killing the
	// agent. The agent calls agentenv__checkout, the env reverts around it, and
	// it continues. (Default mode kills + relaunches the agent on rollback —
	// right when an EXTERNAL operator drives the rollback.)
	selfRollback := hasFlag(args, "--self-rollback")
	sock := flagValue(args, "--socket")
	if sock == "" {
		if selfRollback {
			dir := filepath.Join(r.Backend().Snapshotter.WorkRoot(), ".agentenv")
			_ = os.MkdirAll(dir, 0o700)
			sock = filepath.Join(dir, "control.sock")
		} else {
			sock = filepath.Join(rootDir(), "agentenv.sock")
		}
	}
	if selfRollback {
		r.SetPreserveProcs(true)
	}
	// signal.NotifyContext gives us a context that is cancelled on SIGINT/SIGTERM
	// — replaces the old chan/goroutine plumbing and means everything downstream
	// (api.Serve, the run loop, tailFile) can stop on the same signal.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go api.Serve(ctx, r, cap, sock)
	defer os.Remove(sock)

	// Two modes, chosen by whether stdin is a terminal:
	//   - interactive (TTY): run the agent on a PTY in the foreground so a REPL
	//     like Claude Code works. Stay SILENT — no banner, no snapshot notices —
	//     so the agent's terminal isn't polluted. (Inspect from another terminal
	//     with `agentenv log` / `agentenv ctl log`.)
	//   - headless (no TTY): print a startup banner + tail the agent's log — for
	//     autonomous/long-running agents where this is useful operational output.
	// In self-rollback mode a checkout doesn't kill the agent, so the run loops'
	// "killed by rollback → relaunch" branch simply never fires.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return superviseInteractive(ctx, r, agentArgs, selfRollback)
	}
	fmt.Printf("agentenv supervise: backend=%s control-socket=%s self-rollback=%v (uid=%d)\n", r.Backend().Name, sock, selfRollback, os.Getuid())
	fmt.Printf("running agent inside the env: %s\n", strings.Join(agentArgs, " "))
	cap.SetOnSnapshot(printSnapshot)
	return superviseHeadless(ctx, r, agentArgs, selfRollback)
}

// hasFlag reports whether flag appears in argv (before any "--").
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == "--" {
			return false
		}
		if a == flag {
			return true
		}
	}
	return false
}

// superviseInteractive runs the agent on the controlling terminal. In default
// mode a rollback kills the agent and we relaunch it from the restored env. In
// self-rollback mode the agent is never killed by a checkout (it rolls itself
// back and keeps running), so any exit is genuine — we don't relaunch.
// Snapshot notices are NOT printed (they'd corrupt the agent's TUI — inspect
// history from another terminal with `agentenv ctl log`).
func superviseInteractive(ctx context.Context, r *repo.Repo, agentArgs []string, selfRollback bool) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		before := r.CheckoutCount()
		_, err := r.RunInteractive(agentArgs)
		if ctx.Err() != nil {
			return nil
		}
		if !selfRollback && r.CheckoutCount() > before {
			fmt.Println("\nagentenv supervise: rolled back — relaunching agent from the restored env")
			continue
		}
		if err != nil {
			return err
		}
		fmt.Println("\nagentenv supervise: agent exited; stopping")
		return nil
	}
}

// superviseHeadless backgrounds the agent and tails its log (autonomous agents).
// In self-rollback mode a checkout never kills the agent, so we don't treat an
// exit as a rollback-relaunch — the agent rolled itself back and then exited
// for real.
func superviseHeadless(ctx context.Context, r *repo.Repo, agentArgs []string, selfRollback bool) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		before := r.CheckoutCount()
		p, err := r.Spawn(agentArgs)
		if err != nil {
			return err
		}
		fmt.Printf("  agent pid %d (log %s)\n", p.PID, p.Log)
		go tailFile(p.Log, os.Stdout, ctx.Done())

		for r.ProcAlive(p.PID) {
			select {
			case <-ctx.Done():
				_ = r.Kill(p.PID)
				return nil
			case <-time.After(200 * time.Millisecond):
			}
		}
		if !selfRollback && r.CheckoutCount() > before {
			fmt.Println("  agent killed by rollback — restarting from the restored env")
			continue
		}
		fmt.Println("agentenv supervise: agent exited; stopping")
		return nil
	}
}

// tailFile streams a growing file to w until stop is closed (surfaces agent logs).
func tailFile(path string, w io.Writer, stop <-chan struct{}) {
	var off int64
	for {
		select {
		case <-stop:
			return
		default:
		}
		if f, err := os.Open(path); err == nil {
			f.Seek(off, io.SeekStart)
			n, _ := io.Copy(w, f)
			off += n
			f.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// cmdDaemon serves the JSON-over-unix-socket API so agent harnesses can drive the
// environment programmatically. Auto-capture runs alongside, so each command the
// agent runs returns the snapshot node it produced (for later checkout/branch).
func cmdDaemon(r *repo.Repo, args []string) error {
	sock := flagValue(args, "--socket")
	if sock == "" {
		if v := os.Getenv("AGENTENV_SOCKET"); v != "" {
			sock = v
		} else {
			sock = filepath.Join(rootDir(), "agentenv.sock")
		}
	}
	cap := r.StartCapturer()
	defer cap.Stop()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// uid in the banner surfaces the cross-uid trap (daemon=root, agent=non-root,
	// socket=0600) right away — pairs with daemonclient.diagnoseConnect.
	fmt.Printf("agentenv daemon: backend=%s listening on %s (uid=%d, mode=0600)\n", r.Backend().Name, sock, os.Getuid())
	defer os.Remove(sock)
	return api.Serve(ctx, r, cap, sock)
}

// cmdShell runs an interactive program inside the environment on a PTY with
// auto-capture running alongside: every change it makes is auto-snapshotted,
// and you rewind later with checkout. With no args it's a login shell; with a
// command after `--` it runs that instead — e.g. `agentenv shell -- claude`
// gives an interactive Claude Code session whose whole environment is
// transparently versioned.
func cmdShell(r *repo.Repo, args []string) error {
	cmd := after(args, "--")
	if len(cmd) == 0 {
		cmd = []string{"bash", "-l"}
	}
	cap := r.StartCapturer()
	defer cap.Stop()
	cap.SetOnSnapshot(printSnapshot)
	fmt.Printf("agentenv shell: backend=%s — running %q inside the env, auto-snapshot on change.\n", r.Backend().Name, strings.Join(cmd, " "))
	code, err := r.Shell(cmd)
	if err != nil {
		return err
	}
	cap.Flush("end of shell session") // capture the final state before stopping
	fmt.Printf("\nagentenv shell: %q exited (code %d); snapshots captured.\n", strings.Join(cmd, " "), code)
	return nil
}

func printSnapshot(id, label string) {
	fmt.Printf("  ✓ snapshot %s  %q\n", id, label)
}
