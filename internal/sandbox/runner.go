//go:build linux

// Package sandbox runs a real binary inside a given rootfs using Linux
// namespaces + pivot_root, implemented directly on golang.org/x/sys/unix
// (no libcontainer / runc dependency).
//
// Topology recap: the agent itself lives in the control-root and is NOT affected
// by rollback. Each command the agent runs becomes a child here, with the
// inner-env subvolume as its root. The child is PID 1 of a fresh PID namespace,
// so killing it (see repo.killAll on checkout) tears down the whole inner-env
// process tree.
//
// Networking: we deliberately do NOT create a new network namespace, so the
// child shares the host network and `apt install` can reach the internet. The
// host's /etc/resolv.conf is bind-mounted in for DNS.
package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// childArg is the re-exec marker. main() must detect it and call Child().
const childArg = "__child"

// buildCmd prepares (but does not start) a re-exec of the current binary that
// will start in fresh namespaces and pivot_root into rootfs (see Child).
func buildCmd(rootfs string, args, env []string) *exec.Cmd {
	cmd := exec.Command("/proc/self/exe", append([]string{childArg, rootfs}, args...)...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// New mount/PID/UTS/IPC namespaces; NO new net namespace (share host net
		// so `apt install` can reach the internet). Mounts are made private inside
		// the child (see setupAndExec), so nothing propagates back to the host.
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC,
	}
	return cmd
}

// Run executes args inside rootfs and blocks until it exits (foreground).
func Run(rootfs string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := buildCmd(rootfs, args, env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// RunRootless is like Run but creates a new USER namespace too, mapping container
// root (uid/gid 0) to the caller's real uid/gid. This lets an unprivileged,
// non-root process (e.g. a restricted K8s pod running as uid 1001) gain namespaced
// CAP_SYS_ADMIN and thus do mount/pivot_root — without any host privilege.
func RunRootless(rootfs string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := buildCmd(rootfs, args, env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	addUserns(cmd) // user namespace on top of the namespaces buildCmd already requested
	return cmd.Run()
}

// Start launches args inside rootfs detached and returns immediately. The caller
// owns the returned *exec.Cmd: it must Wait() on it (to reap) and may Kill() it.
// Killing the process kills the whole inner-env PID namespace it leads.
func Start(rootfs string, args, env []string, out io.Writer) (*exec.Cmd, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	cmd := buildCmd(rootfs, args, env)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// addUserns adds a user namespace + uid/gid maps so an unprivileged process can
// act as root inside the sandbox (see RunRootless).
//
// As a non-root caller only a single uid is mappable (container-root -> caller),
// which is fine because an unprivileged extraction can't chown files to other uids
// anyway. As real root we identity-map the full range, so files owned by other
// uids (e.g. _apt) remain accessible inside the namespace.
func addUserns(cmd *exec.Cmd) {
	uid, gid := os.Getuid(), os.Getgid()
	size := 1
	if uid == 0 {
		size = 1 << 16 // identity-map all standard uids/gids
	}
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: uid, Size: size}}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: gid, Size: size}}
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
}

// StartRootless is the rootless (user-namespace) variant of Start.
func StartRootless(rootfs string, args, env []string, out io.Writer) (*exec.Cmd, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	cmd := buildCmd(rootfs, args, env)
	cmd.Stdout, cmd.Stderr = out, out
	addUserns(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// IsChild reports whether the current process was re-executed as the namespaced
// child (i.e. os.Args == [exe, "__child", rootfs, cmd...]).
func IsChild() bool {
	return len(os.Args) > 2 && os.Args[1] == childArg
}

// Child is the entry point for the re-executed process. It sets up the inner-env
// root and execs the target command. It never returns on success.
func Child() {
	rootfs := os.Args[2]
	cmd := os.Args[3:]
	if err := setupAndExec(rootfs, cmd); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox child:", err)
		os.Exit(127)
	}
}

func setupAndExec(rootfs string, cmd []string) error {
	// Make all mounts in this namespace private so nothing propagates back to host.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make-rprivate: %w", err)
	}
	// pivot_root requires the new root to be a mount point: bind rootfs onto itself.
	if err := unix.Mount(rootfs, rootfs, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs: %w", err)
	}

	// Bind host /dev, /sys, and /etc/resolv.conf before switching root, while the
	// host paths are still reachable. /proc is mounted fresh after pivot so it
	// reflects the new PID namespace.
	// Bind host /dev and /sys best-effort: in a rootless user namespace some
	// sub-mounts may be unbindable, which must not abort the whole setup. apt etc.
	// mainly need /dev/null, which an rbind of /dev provides when it succeeds.
	for _, b := range []struct{ src, dst string }{
		{"/dev", filepath.Join(rootfs, "dev")},
		{"/sys", filepath.Join(rootfs, "sys")},
	} {
		_ = os.MkdirAll(b.dst, 0o755)
		_ = unix.Mount(b.src, b.dst, "", unix.MS_BIND|unix.MS_REC, "")
	}
	resolv := filepath.Join(rootfs, "etc/resolv.conf")
	_ = os.MkdirAll(filepath.Dir(resolv), 0o755)
	if f, err := os.OpenFile(resolv, os.O_CREATE, 0o644); err == nil {
		f.Close()
		_ = unix.Mount("/etc/resolv.conf", resolv, "", unix.MS_BIND, "")
	}

	// pivot_root using the "." trick: chdir into the new root, then pivot_root(".", old).
	if err := os.Chdir(rootfs); err != nil {
		return err
	}
	old := ".pivot_old"
	if err := os.MkdirAll(old, 0o700); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", old); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}

	oldRoot := "/" + old
	// Fresh /proc for the new PID namespace. In a rootless user namespace with a
	// masked /proc (Docker/K8s), a fresh procfs mount can be refused
	// (mount_too_revealing); fall back to rbinding the existing /proc, which still
	// provides /proc/self, /proc/mounts, etc. (carrying the masks along).
	_ = os.MkdirAll("/proc", 0o755)
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		if err2 := unix.Mount(oldRoot+"/proc", "/proc", "", unix.MS_BIND|unix.MS_REC, ""); err2 != nil {
			return fmt.Errorf("mount proc: %w (rbind fallback: %v)", err, err2)
		}
	}
	// Detach the old root. We intentionally do NOT remove the (hidden) .pivot_old
	// mount point: creating+removing it on every exec would churn the rootfs root
	// directory's mtime and trigger spurious auto-snapshots for read-only commands.
	// Leaving the empty hidden dir means only the first command ever touches it.
	if err := unix.Unmount(oldRoot, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}

	_ = unix.Sethostname([]byte("agentenv"))

	// Resolve the command against PATH inside the new root and exec it.
	path, err := exec.LookPath(cmd[0])
	if err != nil {
		return fmt.Errorf("lookup %q: %w", cmd[0], err)
	}
	return unix.Exec(path, cmd, os.Environ())
}
