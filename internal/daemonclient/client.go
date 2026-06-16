// Package daemonclient is the in-process client for the agentenv daemon's
// newline-delimited JSON API (see internal/protocol). It owns all the
// "dial unix socket, write one request line, decode protocol.Response frames"
// plumbing so that callers — internal/cli's `ctl` subcommand AND the MCP
// server bridge (internal/mcp) — share one implementation.
//
// Protocol shape (recap of internal/protocol):
//
//   - 1 request line in → 1 or more Response frames out.
//   - Most ops emit exactly 1 frame; `exec` streams stdout/stderr frames and
//     then a terminal frame (OK or Error).
//
// Three call shapes are exposed:
//
//   - Dial + Recv + Close — full control; use when you need to react to each
//     frame (ctl's streaming exec prints stdout/stderr as it arrives).
//   - Roundtrip — read exactly one frame; correct for non-streaming ops.
//   - Drain — read until a Terminal() frame; correct when you want the final
//     verdict but don't care about intermediate streaming frames (the MCP
//     bridge uses this defensively in case someone routes `exec` through it).
package daemonclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/css521/agentenv/internal/protocol"
)

// Conn is one open daemon connection with the request already sent. Recv()
// repeatedly until you get a Terminal() frame (or an error), then Close().
type Conn struct {
	c   net.Conn
	dec *json.Decoder
}

// Dial connects to the daemon's unix socket and sends one JSON request line.
// The returned Conn is ready for Recv(); the caller closes it.
func Dial(sock string, req map[string]any) (*Conn, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w (%s)", sock, err, diagnoseConnect(sock, err))
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
		return nil, err
	}
	return &Conn{c: conn, dec: json.NewDecoder(conn)}, nil
}

// diagnoseConnect turns a raw net.Dial error into a hint a user can act on.
// The two most common failures, ordered by how often we've seen them bite:
//
//  1. "permission denied" — the socket exists but is owned by another uid.
//     This is the cross-uid trap (daemon started by root, agent runs as
//     non-root, socket is 0600). We stat the socket and report both uids so
//     the user immediately sees the mismatch.
//  2. "no such file" — daemon isn't running, or the user pointed us at the
//     wrong socket path.
func diagnoseConnect(sock string, dialErr error) string {
	if errors.Is(dialErr, fs.ErrPermission) {
		if st, err := os.Stat(sock); err == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				return fmt.Sprintf("socket owned by uid=%d, this process is uid=%d — "+
					"start `agentenv daemon` and the client under the SAME user", sys.Uid, os.Getuid())
			}
		}
		return "socket exists but you don't have permission to connect — uid mismatch?"
	}
	if errors.Is(dialErr, fs.ErrNotExist) {
		return fmt.Sprintf("no socket at %s — is `agentenv daemon`/`supervise` running? "+
			"check AGENTENV_SOCKET / AGENTENV_ROOT", sock)
	}
	return "is `agentenv daemon`/`supervise` running?"
}

// Recv decodes the next response frame.
func (c *Conn) Recv() (*protocol.Response, error) {
	var r protocol.Response
	if err := c.dec.Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Close closes the underlying socket.
func (c *Conn) Close() error { return c.c.Close() }

// Roundtrip sends one request and reads exactly one frame back. Use this for
// non-streaming ops (everything except `exec`).
func Roundtrip(sock string, req map[string]any) (*protocol.Response, error) {
	conn, err := Dial(sock, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.Recv()
}

// Drain sends one request and reads frames until a Terminal() one arrives,
// discarding any streaming frames along the way. Use this when you only care
// about the final verdict.
func Drain(sock string, req map[string]any) (*protocol.Response, error) {
	conn, err := Dial(sock, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	for {
		r, err := conn.Recv()
		if err != nil {
			return nil, err
		}
		if r.Terminal() {
			return r, nil
		}
	}
}

// SocketPath resolves the daemon socket path, in order of precedence:
//
//  1. explicit override (--socket flag value; pass "" to skip)
//  2. AGENTENV_SOCKET env var
//  3. AGENTENV_ROOT/agentenv.sock (root defaults to /agentfs)
//
// Centralizing this means `ctl`, the MCP server, and any future client all
// agree on where the daemon listens.
func SocketPath(override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("AGENTENV_SOCKET"); v != "" {
		return v
	}
	root := os.Getenv("AGENTENV_ROOT")
	if root == "" {
		root = "/agentfs"
	}
	return filepath.Join(root, "agentenv.sock")
}
