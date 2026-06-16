package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/css521/agentenv/internal/protocol"
)

// shortTempSock returns a unix-socket path short enough for Darwin (which
// caps sun_path at 104 bytes). t.TempDir() on macOS embeds the full test
// name in /var/folders/.../ and easily blows past that limit.
func shortTempSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ae")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// fakeDaemon stands in for a real `agentenv daemon`: it listens on a unix
// socket, decodes one request line per connection, looks up a canned response
// in a table, and writes it back as one frame. That's enough to exercise the
// full MCP-server → daemonclient → socket round-trip end-to-end without
// needing the Linux-only repo/sandbox/backend stack.
type fakeDaemon struct {
	t      *testing.T
	ln     net.Listener
	mu     sync.Mutex
	got    []protocol.Request  // every request the daemon received, in order
	replyF func(req protocol.Request) protocol.Response
}

func newFakeDaemon(t *testing.T, reply func(protocol.Request) protocol.Response) *fakeDaemon {
	t.Helper()
	sock := shortTempSock(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &fakeDaemon{t: t, ln: ln, replyF: reply}
	go d.serve()
	t.Cleanup(func() { ln.Close() })
	return d
}

func (d *fakeDaemon) sock() string { return d.ln.Addr().String() }

func (d *fakeDaemon) serve() {
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go d.handle(c)
	}
}

func (d *fakeDaemon) handle(c net.Conn) {
	defer c.Close()
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req protocol.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	d.mu.Lock()
	d.got = append(d.got, req)
	d.mu.Unlock()
	resp := d.replyF(req)
	b, _ := json.Marshal(resp)
	c.Write(append(b, '\n'))
}

func (d *fakeDaemon) requests() []protocol.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]protocol.Request, len(d.got))
	copy(out, d.got)
	return out
}

// startMCP wires the MCP server (with `sock` as the daemon socket) into an
// in-memory transport pair, returns the connected client session. Mirrors how
// Claude Code would drive us, minus the stdio framing.
func startMCP(t *testing.T, sock string) *mcpsdk.ClientSession {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "agentenv", Version: serverVersion}, nil)
	register(server, sock)

	t1, t2 := mcpsdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	sess, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// call invokes one MCP tool and returns the joined text content + isError.
func call(t *testing.T, sess *mcpsdk.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func TestToolsList(t *testing.T) {
	// No daemon needed for tools/list — it's served entirely by the SDK from
	// the static AddTool registrations.
	sess := startMCP(t, "/nowhere.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"agentenv__head":      false,
		"agentenv__log":       false,
		"agentenv__branches":  false,
		"agentenv__show":      false,
		"agentenv__diff":      false,
		"agentenv__checkout":  false,
	}
	for _, tl := range res.Tools {
		if _, ok := want[tl.Name]; ok {
			want[tl.Name] = true
		}
		if tl.Description == "" {
			t.Errorf("tool %s has empty Description", tl.Name)
		}
		if tl.InputSchema == nil {
			t.Errorf("tool %s has nil InputSchema", tl.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %s not registered", name)
		}
	}
}

func TestHeadAndCheckout(t *testing.T) {
	daemon := newFakeDaemon(t, func(req protocol.Request) protocol.Response {
		switch req.Op {
		case "head":
			return protocol.Response{OK: true, Head: "abc123def456"}
		case "checkout":
			return protocol.Response{OK: true, Head: req.Node}
		}
		return protocol.Response{Error: "unexpected op " + req.Op}
	})
	sess := startMCP(t, daemon.sock())

	// head — no args, should ship op="head" and surface the daemon's Head.
	out, isErr := call(t, sess, "agentenv__head", nil)
	if isErr {
		t.Fatalf("head returned isError: %s", out)
	}
	if !strings.Contains(out, "abc123def456") {
		t.Errorf("head: want substring abc123def456, got %q", out)
	}

	// checkout — must forward the `node` arg verbatim and surface the new HEAD.
	out, isErr = call(t, sess, "agentenv__checkout", map[string]any{"node": "winner"})
	if isErr {
		t.Fatalf("checkout returned isError: %s", out)
	}
	if !strings.Contains(out, "winner") {
		t.Errorf("checkout: want substring winner, got %q", out)
	}

	reqs := daemon.requests()
	if len(reqs) != 2 {
		t.Fatalf("daemon got %d reqs, want 2: %+v", len(reqs), reqs)
	}
	if reqs[0].Op != "head" {
		t.Errorf("req[0].Op = %q, want head", reqs[0].Op)
	}
	if reqs[1].Op != "checkout" || reqs[1].Node != "winner" {
		t.Errorf("req[1] = %+v, want {Op:checkout Node:winner}", reqs[1])
	}
}

func TestLogAndBranches(t *testing.T) {
	nodes := []protocol.Node{
		{ID: "n1", Message: "root"},
		{ID: "n2", Parent: "n1", Message: "edit", Head: true},
		{ID: "n3", Parent: "n2", Message: "branch", Leaf: true},
	}
	daemon := newFakeDaemon(t, func(req protocol.Request) protocol.Response {
		return protocol.Response{OK: true, Nodes: nodes}
	})
	sess := startMCP(t, daemon.sock())

	for _, tool := range []string{"agentenv__log", "agentenv__branches"} {
		out, isErr := call(t, sess, tool, nil)
		if isErr {
			t.Fatalf("%s returned isError: %s", tool, out)
		}
		for _, n := range nodes {
			if !strings.Contains(out, n.ID) {
				t.Errorf("%s: missing node %s in output:\n%s", tool, n.ID, out)
			}
		}
		if !strings.Contains(out, "<- HEAD") {
			t.Errorf("%s: missing HEAD marker:\n%s", tool, out)
		}
		if !strings.Contains(out, "parent=n1") {
			t.Errorf("%s: missing parent annotation:\n%s", tool, out)
		}
	}
}

func TestShowAndDiff(t *testing.T) {
	daemon := newFakeDaemon(t, func(req protocol.Request) protocol.Response {
		switch req.Op {
		case "show":
			if req.Node != "n2" {
				return protocol.Response{Error: fmt.Sprintf("want node=n2, got %q", req.Node)}
			}
			return protocol.Response{OK: true, Changes: []protocol.Change{
				{Kind: "+", Path: "newfile"},
				{Kind: "M", Path: "edited"},
			}}
		case "diff":
			if req.A != "a1" || req.B != "b2" {
				return protocol.Response{Error: fmt.Sprintf("want a=a1 b=b2, got a=%q b=%q", req.A, req.B)}
			}
			return protocol.Response{OK: true, Changes: []protocol.Change{
				{Kind: "-", Path: "removed"},
			}}
		}
		return protocol.Response{Error: "unexpected op " + req.Op}
	})
	sess := startMCP(t, daemon.sock())

	out, isErr := call(t, sess, "agentenv__show", map[string]any{"node": "n2"})
	if isErr {
		t.Fatalf("show isError: %s", out)
	}
	if !strings.Contains(out, "newfile") || !strings.Contains(out, "edited") || !strings.Contains(out, "2 changed") {
		t.Errorf("show output missing expected fields:\n%s", out)
	}

	out, isErr = call(t, sess, "agentenv__diff", map[string]any{"a": "a1", "b": "b2"})
	if isErr {
		t.Fatalf("diff isError: %s", out)
	}
	if !strings.Contains(out, "removed") || !strings.Contains(out, "1 changed") {
		t.Errorf("diff output missing expected fields:\n%s", out)
	}
}

func TestDaemonErrorBecomesIsError(t *testing.T) {
	daemon := newFakeDaemon(t, func(req protocol.Request) protocol.Response {
		return protocol.Response{Error: "unknown ref: bogus"}
	})
	sess := startMCP(t, daemon.sock())

	out, isErr := call(t, sess, "agentenv__checkout", map[string]any{"node": "bogus"})
	if !isErr {
		t.Errorf("want isError=true, got false (out=%q)", out)
	}
	if !strings.Contains(out, "unknown ref") {
		t.Errorf("want underlying error message in content, got %q", out)
	}
	// Anti-hallucination guard: every error must carry the prefix that tells
	// the model "the call did not execute, do not fabricate a result".
	if !strings.Contains(out, "TOOL CALL FAILED") {
		t.Errorf("error content missing anti-hallucination prefix: %q", out)
	}
}

func TestSocketUnreachableBecomesIsError(t *testing.T) {
	// Point the server at a path nothing listens on. A connect failure must
	// surface as an MCP tool error, not a hard protocol error — Claude Code
	// should see the message and be able to recover.
	sess := startMCP(t, shortTempSock(t))

	out, isErr := call(t, sess, "agentenv__head", nil)
	if !isErr {
		t.Errorf("want isError=true for missing socket, got false (out=%q)", out)
	}
	if !strings.Contains(out, "connect") {
		t.Errorf("want connect error in content, got %q", out)
	}
	// The diagnoseConnect path should kick in for ENOENT: socket path doesn't
	// exist, so the user should be told to start the daemon, not just see
	// "no such file or directory" naked.
	if !strings.Contains(out, "daemon") {
		t.Errorf("want diagnostic mentioning daemon, got %q", out)
	}
}

func TestSchemaRejectsMissingRequiredArg(t *testing.T) {
	// The SDK validates inputs against the inferred JSON schema before the
	// handler runs. A `show` call with no `node` should be rejected without
	// ever reaching the daemon.
	daemon := newFakeDaemon(t, func(req protocol.Request) protocol.Response {
		t.Errorf("daemon should not be reached: got %+v", req)
		return protocol.Response{OK: true}
	})
	sess := startMCP(t, daemon.sock())

	// Empty arguments → schema validation should fail. CallTool surfaces this
	// either as an isError result or as a hard error; either is acceptable as
	// long as the daemon wasn't called.
	out, isErr := call(t, sess, "agentenv__show", map[string]any{})
	if !isErr {
		t.Errorf("want isError or hard error for missing 'node', got success: %q", out)
	}
	if got := daemon.requests(); len(got) != 0 {
		t.Errorf("daemon should not have been called, got %+v", got)
	}
}
