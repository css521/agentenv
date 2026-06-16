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
	cap.SetOnSnapshot(printSnapshot)

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

// cmdShell drops you into an interactive shell inside the environment with
// auto-capture running alongside: run it once, work normally (every change is
// auto-snapshotted), exit when done. Rewind later with checkout.
func cmdShell(r *repo.Repo, _ []string) error {
	cap := r.StartCapturer()
	defer cap.Stop()
	cap.SetOnSnapshot(printSnapshot)
	fmt.Printf("agentenv shell: backend=%s — interactive env, auto-snapshot on change. Type 'exit' to leave.\n", r.Backend().Name)
	code, err := r.Shell([]string{"bash", "-l"})
	if err != nil {
		return err
	}
	cap.Flush("end of shell session") // capture the final state before stopping
	fmt.Printf("\nagentenv shell: exited (code %d); snapshots captured.\n", code)
	return nil
}

func printSnapshot(id, label string) {
	fmt.Printf("  ✓ snapshot %s  %q\n", id, label)
}
