//go:build linux

package sandbox

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// RunPTY runs an interactive command inside rootfs on a pseudo-terminal that's
// wired to the caller's terminal (raw mode + window-size propagation). rootless
// adds a user namespace so a non-root caller can do it. Returns the exit code.
//
// When stdin is not a terminal (e.g. piped), the call falls back to a plain
// non-PTY run with the same plumbing — the caller still gets stdout/stderr.
//
// Previously this file hand-rolled /dev/ptmx / TIOCSPTLCK / TIOCGPTN / /dev/pts
// path-joining / termios raw-mode bit flips / SIGWINCH plumbing — about 130 lines
// of brittle ioctl wrangling. creack/pty + golang.org/x/term shrink it to ~30
// well-tested lines (pure-Go, no cgo, no extra runtime dependencies).
func RunPTY(rootfs string, args, env []string, rootless bool) (int, error) {
	if len(args) == 0 {
		args = []string{"bash", "-l"}
	}
	// Piped stdin (e.g. tests) → no PTY to set up; run streamed.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		err := runStreamed(rootfs, args, env, rootless)
		return exitCode(err), err
	}

	cmd := buildCmd(rootfs, args, env)
	if rootless {
		addUserns(cmd)
	}
	// pty.Start allocates a master/slave pair, wires the slave as the child's
	// stdin/stdout/stderr, sets Setsid + Setctty in SysProcAttr (preserving our
	// existing Cloneflags / UidMappings), and starts the process.
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return -1, err
	}
	defer ptmx.Close()

	// Put the caller's terminal into raw mode; restore on exit.
	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}

	// Propagate window size now and on every SIGWINCH.
	_ = pty.InheritSize(os.Stdin, ptmx)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()

	go func() { _, _ = io.Copy(ptmx, os.Stdin) }() // keystrokes → pty
	_, _ = io.Copy(os.Stdout, ptmx)                // pty → screen (ends when child exits)

	return exitCode(cmd.Wait()), nil
}

// runStreamed runs without a PTY (used when stdin is not a terminal).
func runStreamed(rootfs string, args, env []string, rootless bool) error {
	if rootless {
		return RunRootless(rootfs, args, env, os.Stdin, os.Stdout, os.Stderr)
	}
	return Run(rootfs, args, env, os.Stdin, os.Stdout, os.Stderr)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return -1
}
