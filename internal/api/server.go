//go:build linux

// Package api serves a small newline-delimited JSON protocol over a unix socket,
// so an agent harness can drive the environment programmatically (run commands,
// branch, roll back) and get structured results — including the snapshot node id
// produced by each command, which it can later checkout or branch from.
//
// One JSON request per line; one JSON response per line. Example:
//
//	{"op":"exec","cmd":"apt-get install -y jq"}
//	-> {"ok":true,"exit":0,"stdout":"...","node":"ab12cd34ef56","head":"ab12cd34ef56"}
package api

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"

	"github.com/css521/agentenv/internal/protocol"
	"github.com/css521/agentenv/internal/repo"
)

// All wire types live in internal/protocol so the client (internal/cli ctl) can
// decode straight into the same struct shapes — no map[string]any reflection.
type (
	request  = protocol.Request
	response = protocol.Response
)

// Serve listens on sockPath and serves requests until ctx is cancelled.
//
// Security: the socket is created with default fs perms (~0755 after umask),
// which lets ANY local user connect and invoke `exec` — i.e. run arbitrary
// commands as whoever runs agentenv (often root). We immediately tighten it to
// 0600 so only the owning uid can talk to the daemon. Callers that need to
// share the socket across UIDs should chmod/chown it themselves AFTER Serve
// has started (with full awareness of the implications).
func Serve(ctx context.Context, r *repo.Repo, c *repo.Capturer, sockPath string) error {
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		return err
	}
	// The listening/uid banner is left to the caller: `daemon` and headless
	// `supervise` print it (useful operational logging + the cross-uid trap
	// hint), while interactive `supervise` stays silent so it doesn't pollute
	// the agent's terminal.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	// RWMutex (was a plain Mutex) so read-only ops (log, head, branches, show,
	// diff, ps, status…) can run concurrently with each other. Without this, a
	// long-running exec held the global lock and starved every other client —
	// e.g. an agent's `make` blocked an operator's `agentenv ctl log`.
	var mu sync.RWMutex
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // listener closed via stop
		}
		go handle(r, c, conn, &mu)
	}
}

// readOnlyOps don't mutate the repo and are safe to run under a read lock —
// many of them at once, in parallel with one writer.
var readOnlyOps = map[string]bool{
	"head": true, "log": true, "branches": true,
	"show": true, "diff": true, "ps": true,
}

func handle(r *repo.Repo, c *repo.Capturer, conn net.Conn, mu *sync.RWMutex) {
	defer conn.Close()
	// json.Decoder streams over the connection: no max-line ceiling (bufio.Scanner
	// had a 16MB cap, after which it silently failed), and EOF / parse errors
	// surface cleanly as io.EOF / *json.SyntaxError. The protocol is still
	// newline-delimited JSON on the wire — json.Decoder accepts it transparently.
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			return // EOF or malformed: drop the connection
		}
		// Read-only ops use the read lock so concurrent log/diff/show calls
		// don't serialize behind a long-running exec. Mutating ops (and exec,
		// which mutates via auto-snapshot) take the writer lock.
		if readOnlyOps[req.Op] {
			mu.RLock()
			err := enc.Encode(dispatch(r, c, req))
			mu.RUnlock()
			if err != nil {
				return
			}
			continue
		}
		mu.Lock()
		if req.Op == "exec" {
			dispatchExecStream(r, c, enc, req)
		} else if err := enc.Encode(dispatch(r, c, req)); err != nil {
			mu.Unlock()
			return
		}
		mu.Unlock()
	}
}

// streamWriter emits one JSON frame per Write to the encoder, tagged as stdout
// or stderr. The shared mutex serializes both streams onto the same connection.
type streamWriter struct {
	mu     *sync.Mutex
	enc    *json.Encoder
	stream string // "out" or "err"
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := response{}
	if w.stream == "out" {
		f.Stdout = string(p)
	} else {
		f.Stderr = string(p)
	}
	if err := w.enc.Encode(f); err != nil {
		return 0, err
	}
	return len(p), nil
}

// dispatchExecStream runs an exec and emits NDJSON frames:
//
//	{"stdout":"line 1\n"} ... {"stdout":"line 2\n"} ... {"stderr":"oops\n"}
//	{"ok":true,"exit":0,"node":"<id>","head":"<id>"}    ← terminal frame
//
// Clients keep reading frames until they see one with "ok":true (success) or
// "error":"..." (failure). The terminal frame is the only one that contains
// "ok" or "error" — that's how the client knows the stream is over.
func dispatchExecStream(r *repo.Repo, c *repo.Capturer, enc *json.Encoder, req request) {
	c.SetLabel("$ " + req.Cmd)
	var encMu sync.Mutex // serialize stdout vs stderr frames onto the connection
	out := &streamWriter{mu: &encMu, enc: enc, stream: "out"}
	errw := &streamWriter{mu: &encMu, enc: enc, stream: "err"}
	code, err := r.Exec([]string{"bash", "-lc", req.Cmd}, nil, out, errw)
	encMu.Lock()
	defer encMu.Unlock()
	if err != nil {
		_ = enc.Encode(response{Error: err.Error()})
		return
	}
	c.Flush("$ " + req.Cmd)
	_ = enc.Encode(response{OK: true, Exit: &code, Node: r.Head(), Head: r.Head()})
}

func dispatch(r *repo.Repo, c *repo.Capturer, req request) response {
	// Note: "exec" is handled by dispatchExecStream() in handle() because it streams
	// stdout/stderr as multiple frames. All other ops return a single response.
	switch req.Op {
	case "spawn":
		p, err := r.Spawn([]string{"bash", "-lc", req.Cmd})
		if err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Pid: p.PID, Log: p.Log}

	case "checkout", "fork":
		if err := r.Checkout(req.Node); err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Head: r.Head()}

	case "delete":
		if err := r.Delete(req.Node); err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Head: r.Head()}

	case "commit":
		n, err := r.Commit(req.Message, "")
		if err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Node: n.ID, Head: r.Head()}

	case "head":
		return response{OK: true, Head: r.Head()}

	case "log":
		head := r.Head()
		var ns []protocol.Node
		for _, n := range r.Nodes() {
			ns = append(ns, protocol.Node{ID: n.ID, Parent: n.Parent, Message: n.Message, Head: n.ID == head, Leaf: len(n.Children) == 0})
		}
		return response{OK: true, Nodes: ns, Head: head}

	case "branches":
		head := r.Head()
		var ns []protocol.Node
		for _, n := range r.Leaves() {
			ns = append(ns, protocol.Node{ID: n.ID, Parent: n.Parent, Message: n.Message, Head: n.ID == head, Leaf: true})
		}
		return response{OK: true, Nodes: ns, Head: head}

	case "diff":
		ch, err := r.Diff(req.A, req.B)
		if err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Changes: changes(ch)}

	case "show":
		ch, parent, err := r.Show(req.Node)
		if err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Parent: parent, Changes: changes(ch)}

	case "ps":
		var ps []protocol.Proc
		for _, p := range r.Procs() {
			args := ""
			for i, a := range p.Args {
				if i > 0 {
					args += " "
				}
				args += a
			}
			ps = append(ps, protocol.Proc{PID: p.PID, Args: args, Log: p.Log})
		}
		return response{OK: true, Procs: ps}

	case "kill":
		if err := r.Kill(req.Pid); err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true}

	case "gc":
		removed, err := r.GC()
		if err != nil {
			return response{Error: err.Error()}
		}
		return response{OK: true, Removed: removed}

	case "retain":
		keep := req.Keep
		if keep <= 0 {
			keep = 30
		}
		return response{OK: true, Dropped: r.Retain(keep)}

	case "tag":
		// 3 modes: list (no name), set (name + ref), get (name only).
		switch {
		case req.Name == "":
			return response{OK: true, Tags: r.Tags()}
		case req.Ref != "":
			if err := r.SetTag(req.Name, req.Ref); err != nil {
				return response{Error: err.Error()}
			}
			return response{OK: true}
		default:
			id := r.ResolveRef(req.Name)
			if id == "" {
				return response{Error: "unknown ref: " + req.Name}
			}
			return response{OK: true, Node: id}
		}

	case "tournament":
		pairs := make([]struct{ Name, Cmd string }, len(req.Candidates))
		for i, c := range req.Candidates {
			pairs[i] = struct{ Name, Cmd string }{c.Name, c.Cmd}
		}
		// Always keep the winner via API — programmatic callers can checkout away
		// afterwards if they prefer.
		res, err := r.Tournament(req.Base, req.Test, pairs, true)
		if err != nil {
			return response{Error: err.Error()}
		}
		out := response{OK: true, Head: r.Head(), Winner: res.Winner, WinnerNode: res.WinnerNode}
		for _, c := range res.Candidates {
			out.Branches = append(out.Branches, protocol.TournamentBranch{Name: c.Name, Cmd: c.Cmd, Node: c.Node, TestExit: c.TestExit})
		}
		return out

	default:
		return response{Error: "unknown op: " + req.Op}
	}
}

func changes(ch []repo.Change) []protocol.Change {
	out := make([]protocol.Change, 0, len(ch))
	for _, c := range ch {
		out = append(out, protocol.Change{Kind: string(c.Kind), Path: c.Path})
	}
	return out
}
