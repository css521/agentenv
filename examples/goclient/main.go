//go:build ignore

// A Go client for agentenv that drives the high-level `tournament` op — the
// daemon forks N parallel branches from a base, runs each candidate plus a test
// command, and tells you which branch passed first. Compared to the manual
// loop in ../branch_explore.py (head → checkout → exec → check, repeat), it's
// one round-trip and the parallelism happens inside agentenv (true parallel
// workspaces, not serial like a hand-rolled loop).
//
// Run: go run examples/goclient/main.go /agentfs/agentenv.sock
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
)

type branch struct {
	Name     string `json:"name"`
	Cmd      string `json:"cmd"`
	Node     string `json:"node"`
	TestExit int    `json:"test_exit"`
}

type resp struct {
	OK         bool     `json:"ok"`
	Error      string   `json:"error"`
	Head       string   `json:"head"`
	Branches   []branch `json:"branches"`
	Winner     string   `json:"winner"`
	WinnerNode string   `json:"winner_node"`
}

// call sends one request and returns the terminal frame. The tournament op is
// single-frame; streaming ops (exec) would need draining first.
func call(c net.Conn, r *bufio.Reader, req map[string]any) resp {
	b, _ := json.Marshal(req)
	c.Write(append(b, '\n'))
	line, _ := r.ReadBytes('\n')
	var out resp
	json.Unmarshal(line, &out)
	if out.Error != "" {
		panic("api error: " + out.Error)
	}
	return out
}

func main() {
	sock := "/agentfs/agentenv.sock"
	if len(os.Args) > 1 {
		sock = os.Args[1]
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		panic(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)

	apt := "apt-get -o APT::Sandbox::User=root install -y -qq"
	req := map[string]any{
		"op":   "tournament",
		"base": "HEAD",
		"test": "command -v jq",
		"candidates": []map[string]string{
			{"name": "A", "cmd": apt + " tree"},
			{"name": "B", "cmd": apt + " jq"},
			{"name": "C", "cmd": apt + " figlet"},
		},
	}
	out := call(c, r, req)

	fmt.Println("tournament results:")
	for _, b := range out.Branches {
		verdict := fmt.Sprintf("exit %d", b.TestExit)
		if b.TestExit == 0 {
			verdict = "PASS"
		}
		fmt.Printf("  %s  %s  %s\n", b.Name, b.Node, verdict)
	}
	if out.Winner == "" {
		fmt.Println("no candidate passed the test")
		os.Exit(1)
	}
	fmt.Printf("winner: %s (node %s) — HEAD now %s\n", out.Winner, out.WinnerNode, out.Head)

	// Optional clean-up: prune the losing branches with the v0.2.0 `delete` op.
	// Children re-parent to the deleted node's parent; harmless here (losers
	// are leaves) and keeps the DAG free of dead-end snapshots.
	for _, b := range out.Branches {
		if b.Name != out.Winner {
			call(c, r, map[string]any{"op": "delete", "node": b.Node})
			fmt.Printf("  pruned %s (node %s)\n", b.Name, b.Node)
		}
	}
}
