//go:build ignore

// A Go client for the agentenv JSON socket API — same branch-exploration as the
// Python example, showing the protocol is language-neutral (stdlib only).
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

type resp struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Exit   *int   `json:"exit"`
	Stdout string `json:"stdout"`
	Head   string `json:"head"`
	Node   string `json:"node"`
}

type client struct {
	conn net.Conn
	r    *bufio.Reader
}

func dial(sock string) *client {
	c, err := net.Dial("unix", sock)
	if err != nil {
		panic(err)
	}
	return &client{conn: c, r: bufio.NewReader(c)}
}

// call sends one request and returns the TERMINAL frame. For "exec", agentenv
// streams stdout/stderr frames first; we drain them until we see a frame with
// "ok":true or "error". Other ops return one frame, so the loop exits after one
// iteration.
func (c *client) call(req map[string]any) resp {
	b, _ := json.Marshal(req)
	c.conn.Write(append(b, '\n'))
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			panic(err)
		}
		var out resp
		json.Unmarshal(line, &out)
		if out.Error != "" {
			panic("api error: " + out.Error)
		}
		if out.OK {
			return out
		}
		// streaming output frame: drop on the floor in this minimal example
		// (a real client would print or buffer it).
	}
}

func main() {
	sock := "/agentfs/agentenv.sock"
	if len(os.Args) > 1 {
		sock = os.Args[1]
	}
	c := dial(sock)

	base := c.call(map[string]any{"op": "head"}).Head
	fmt.Println("base:", base)

	apt := "apt-get -o APT::Sandbox::User=root install -y -qq "
	tips := map[string]string{}
	for _, pkg := range []string{"tree", "jq", "figlet"} {
		c.call(map[string]any{"op": "checkout", "node": base})
		c.call(map[string]any{"op": "exec", "cmd": apt + pkg})
		tips[pkg] = c.call(map[string]any{"op": "head"}).Head
		fmt.Printf("  explored %s -> %s\n", pkg, tips[pkg])
	}

	winner := ""
	for pkg, tip := range tips {
		c.call(map[string]any{"op": "checkout", "node": tip})
		if *c.call(map[string]any{"op": "exec", "cmd": "command -v jq >/dev/null"}).Exit == 0 {
			winner = pkg
		}
	}
	fmt.Println("winner:", winner)
	c.call(map[string]any{"op": "checkout", "node": tips[winner]})
}
