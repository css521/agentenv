// Package mcp implements an MCP (Model Context Protocol) server that bridges
// Claude Code (and any other MCP host) to a running `agentenv daemon`.
//
// Wire/protocol plumbing — JSON-RPC 2.0 framing, the `initialize` handshake,
// `tools/list`, `tools/call` dispatch, schema inference from Go struct tags —
// is delegated to the official Go SDK (github.com/modelcontextprotocol/go-sdk).
// We only declare typed tool handlers; the SDK validates inputs against the
// inferred schema and packs the result.
//
// Exposed tools (read/inspect only — mutations belong in the daemon, not in
// the agent's MCP action space):
//
//	agentenv__head        — current HEAD node id
//	agentenv__log         — full commit DAG
//	agentenv__branches    — branch tips (distinct explored end-states)
//	agentenv__show        — files a node changed vs its parent
//	agentenv__diff        — files differing between two nodes
//	agentenv__checkout    — roll the whole environment back to any node
//
// Every tool call opens a fresh unix-socket round-trip to the daemon and
// returns when it sees a terminal frame — no socket state is held between
// calls, so the daemon owns concurrency (RWMutex).
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/css521/agentenv/internal/daemonclient"
	"github.com/css521/agentenv/internal/protocol"
)

// emptyArgs is the input type for the zero-arg tools (head, log, branches).
// The SDK still infers a JSON Schema object for it; empty struct → "no
// properties, no required" — exactly what we want.
type emptyArgs struct{}

type nodeArgs struct {
	Node string `json:"node" jsonschema:"node id, ID prefix, or tag name"`
}

type diffArgs struct {
	A string `json:"a" jsonschema:"first ref (node id, prefix, or tag)"`
	B string `json:"b" jsonschema:"second ref (node id, prefix, or tag)"`
}

// Serve runs the MCP server on stdin/stdout (the transport Claude Code uses
// when it spawns this binary), bridging tool calls to the agentenv daemon's
// unix socket. The `version` is reported to clients in the initialize
// handshake — pass the agentenv binary's resolved release tag (main.go's
// resolveVersion) so MCP hosts can log which build they connected to.
// Returns when stdin EOFs or ctx is cancelled.
func Serve(ctx context.Context, sock, version string) error {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "agentenv",
		Version: version,
	}, nil)
	register(server, sock)
	err := server.Run(ctx, &mcp.StdioTransport{})
	// Treat clean shutdown signals as success. Claude Code drops stdin when
	// it disconnects; the SDK surfaces that as io.EOF or as an internal
	// jsonrpc2 "client/server is closing" error (the latter is in an
	// internal package so we can't errors.Is against it, hence the string
	// check). Either way it's a healthy exit — surfacing it as an error
	// makes every MCP session look failed in shell wrappers.
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	if msg := err.Error(); strings.Contains(msg, "client is closing") || strings.Contains(msg, "server is closing") {
		return nil
	}
	return err
}

// register wires every agentenv tool. Each handler is a closure that pins the
// socket path; the SDK takes care of decoding the typed input from the
// CallTool params and validating it against the inferred schema.
func register(s *mcp.Server, sock string) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "agentenv__head",
		Description: "Return the current HEAD node id of the agentenv environment. Use this to know where the time-travel cursor is right now.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		return relay(sock, "head", nil, formatHead)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "agentenv__log",
		Description: "List every node in the agentenv commit DAG (id, parent id, message, whether it is HEAD, whether it is a branch tip). Use this to find a past state to roll back to.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		return relay(sock, "log", nil, formatNodes)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "agentenv__branches",
		Description: "List branch tips (distinct explored end-states). Use this when you've explored multiple approaches and want to compare or pick one.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		return relay(sock, "branches", nil, formatNodes)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "agentenv__show",
		Description: "List the files a given node changed vs its parent (like `git show --stat`). Helps identify which past commit introduced a regression.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in nodeArgs) (*mcp.CallToolResult, any, error) {
		return relay(sock, "show", map[string]any{"node": in.Node}, formatChanges)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "agentenv__diff",
		Description: "List the files that differ between two nodes. Useful for comparing alternative approaches.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in diffArgs) (*mcp.CallToolResult, any, error) {
		return relay(sock, "diff", map[string]any{"a": in.A, "b": in.B}, formatChanges)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "agentenv__checkout",
		Description: "Roll the whole environment back to a node (accepts node id, ID prefix, or tag). The agent's process will be killed and restarted from the restored environment if running under `agentenv supervise`.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in nodeArgs) (*mcp.CallToolResult, any, error) {
		return relay(sock, "checkout", map[string]any{"node": in.Node}, formatHead)
	})
}

// relay issues a single daemon round-trip and packages the response. Errors
// from the daemon are translated to an isError tool result rather than a hard
// protocol error — Claude Code surfaces the text to the model and lets it
// recover, which is what we want for "daemon not running" / "unknown ref".
//
// Drain (vs Roundtrip) is defensive: none of our tools map to `exec`, but if
// an operator ever routes a streaming op through here we'll still terminate
// on the OK/Error frame instead of misreading a stdout frame as the verdict.
func relay(sock, op string, args map[string]any, fmtResp func(*protocol.Response) string) (*mcp.CallToolResult, any, error) {
	if args == nil {
		args = map[string]any{}
	}
	args["op"] = op
	resp, err := daemonclient.Drain(sock, args)
	if err != nil {
		return toolError(err.Error()), nil, nil
	}
	if resp.Error != "" {
		return toolError(resp.Error), nil, nil
	}
	return toolText(fmtResp(resp)), nil, nil
}

func formatHead(r *protocol.Response) string {
	if r.Head == "" {
		return "(no head)"
	}
	return "HEAD is " + r.Head
}

func formatNodes(r *protocol.Response) string {
	if len(r.Nodes) == 0 {
		return "(no nodes)"
	}
	out := ""
	for _, n := range r.Nodes {
		head := ""
		if n.Head {
			head = "  <- HEAD"
		}
		parent := ""
		if n.Parent != "" {
			parent = "  parent=" + n.Parent
		}
		out += fmt.Sprintf("%s  %q%s%s\n", n.ID, n.Message, parent, head)
	}
	return out
}

func formatChanges(r *protocol.Response) string {
	out := ""
	for _, c := range r.Changes {
		out += fmt.Sprintf("%s %s\n", c.Kind, c.Path)
	}
	out += fmt.Sprintf("%d changed\n", len(r.Changes))
	return out
}

// toolText / toolError build the MCP CallToolResult payload.
func toolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// errPrefix is prepended to every error result. In testing we caught Claude
// inventing a plausible-looking node id after a `connect: permission denied`
// — the model rationalized away the isError flag and made up an answer. The
// prefix is deliberately blunt and addresses the model directly, because
// polite error text reads to an LLM as "context, not constraint".
const errPrefix = "AGENTENV MCP TOOL CALL FAILED — the tool DID NOT EXECUTE. " +
	"Do not invent or guess a result. Report this error verbatim to the user.\n\n"

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: errPrefix + msg}},
	}
}
