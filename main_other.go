//go:build !linux

// Non-Linux stub. agentenv's runtime is Linux-only (Linux user namespaces +
// pivot_root, btrfs subvolumes, copy-backend sandbox), so this file exists only
// so that `go build .` on macOS/Windows produces a useful error binary instead
// of a cryptic "no Go files in <dir>" message — and points the user at how to
// actually try it (Docker on the host, or the developer-mode docs).
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintf(os.Stderr, `agentenv: the runtime is Linux-only — it uses Linux user namespaces +
pivot_root, which don't exist on %s.

To try it without a Linux box, run the demo via Docker (rootless, no
--privileged, mirrors a restricted Kubernetes pod):

  docker run --rm --user 1001:1001 --security-opt seccomp=unconfined \
    -v "$PWD":/src:ro -e CGO_ENABLED=0 -e GOCACHE=/tmp/gocache -e HOME=/tmp \
    -w /src golang:1.26 bash /src/examples/demo/killer-demo.sh

For development on macOS (Cursor / VS Code with full Go intel-sense), see the
"Developing on macOS / Windows" section of the README.
`, runtime.GOOS)
	os.Exit(1)
}
