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

	sock := flagValue(args, "--socket")
	if sock == "" {
		sock = filepath.Join(rootDir(), "agentenv.sock")
	}
	// signal.NotifyContext gives us a context that is cancelled on SIGINT/SIGTERM
	// — replaces the old chan/goroutine plumbing and means everything downstream
	// (api.Serve, the run loop, tailFile) can stop on the same signal.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go api.Serve(ctx, r, cap, sock)
	defer os.Remove(sock)

	fmt.Printf("agentenv supervise: backend=%s control-socket=%s\n", r.Backend().Name, sock)
	fmt.Printf("running agent inside the env: %s\n", strings.Join(agentArgs, " "))

	// Two modes, chosen by whether stdin is a terminal:
	//   - interactive (TTY): run the agent on a PTY in the foreground so a REPL
	//     like Claude Code works. Rollback (via the socket, from another
	//     terminal) kills the agent and we relaunch it from the restored env.
	//   - headless (no TTY): background the agent, tail its log. The original
	//     behavior, right for autonomous/long-running agents.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return superviseInteractive(ctx, r, agentArgs)
	}
	cap.SetOnSnapshot(printSnapshot)
	return superviseHeadless(ctx, r, agentArgs)
}

// superviseInteractive runs the agent on the controlling terminal, relaunching
// it after each rollback. Snapshot notices are NOT printed (they'd corrupt the
// agent's TUI — inspect history from another terminal with `agentenv ctl log`).
func superviseInteractive(ctx context.Context, r *repo.Repo, agentArgs []string) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		before := r.CheckoutCount()
		_, err := r.RunInteractive(agentArgs)
		if ctx.Err() != nil {
			return nil
		}
		if r.CheckoutCount() > before {
			fmt.Println("\nagentenv supervise: rolled back — relaunching agent from the restored env")
			continue
		}
		if err != nil {
			return err
		}
		fmt.Println("\nagentenv supervise: agent exited on its own; stopping")
		return nil
	}
}

// superviseHeadless backgrounds the agent and tails its log (autonomous agents).
func superviseHeadless(ctx context.Context, r *repo.Repo, agentArgs []string) error {
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
		if r.CheckoutCount() > before {
			fmt.Println("  agent killed by rollback — restarting from the restored env")
			continue
		}
		fmt.Println("agentenv supervise: agent exited on its own; stopping")
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

	fmt.Printf("agentenv daemon: backend=%s listening on %s\n", r.Backend().Name, sock)
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
