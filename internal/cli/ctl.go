//go:build linux

package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/css521/agentenv/internal/daemonclient"
	"github.com/css521/agentenv/internal/protocol"
)

// cmdCtl is an out-of-band client for a running `daemon`/`supervise`: it connects
// to the control socket and issues one JSON op, printing a human-readable result.
// It does NOT open the repo or take the lock, so it works while a supervise/daemon
// process holds it (e.g. `agentenv ctl checkout <id>` to roll a live agent back).
func cmdCtl(args []string) error {
	sock := daemonclient.SocketPath(flagValue(args, "--socket"))
	args = stripFlagVal(args, "--socket")
	if len(args) == 0 {
		return fmt.Errorf("usage: agentenv ctl [--socket path] <checkout|log|head|branches|exec|spawn|show|diff|commit|gc|ps|kill> [args]")
	}

	op, a := args[0], args[1:]
	req := map[string]any{"op": op}
	switch op {
	case "log", "head", "branches", "gc", "ps":
	case "checkout", "show":
		if len(a) < 1 {
			return fmt.Errorf("usage: agentenv ctl %s <node>", op)
		}
		req["node"] = a[0]
	case "diff":
		if len(a) < 2 {
			return fmt.Errorf("usage: agentenv ctl diff <node-a> <node-b>")
		}
		req["a"], req["b"] = a[0], a[1]
	case "exec", "spawn":
		if len(a) == 0 {
			return fmt.Errorf("usage: agentenv ctl %s <command...>", op)
		}
		req["cmd"] = strings.Join(a, " ")
	case "commit":
		req["message"] = strings.Join(a, " ")
	case "kill":
		if len(a) < 1 {
			return fmt.Errorf("usage: agentenv ctl kill <pid>")
		}
		n, err := strconv.Atoi(a[0])
		if err != nil {
			return fmt.Errorf("bad pid %q", a[0])
		}
		req["pid"] = n
	case "tag":
		// 3 modes: list (no args), get (name only), set (name + ref).
		if len(a) >= 1 {
			req["name"] = a[0]
		}
		if len(a) >= 2 {
			req["ref"] = a[1]
		}
	case "tournament":
		// usage: agentenv ctl tournament --base <ref> --test "<cmd>" -- "cand1" "cand2" ...
		base := flagValue(a, "--base")
		test := flagValue(a, "--test")
		cands := after(a, "--")
		if test == "" || len(cands) == 0 {
			return fmt.Errorf("usage: agentenv ctl tournament [--base <ref>] --test \"<cmd>\" -- \"cand1\" \"cand2\" ...")
		}
		req["base"], req["test"] = base, test
		var list []map[string]string
		for i, c := range cands {
			list = append(list, map[string]string{"name": fmt.Sprintf("%c", 'A'+i), "cmd": c})
		}
		req["candidates"] = list
	default:
		return fmt.Errorf("unknown ctl op %q", op)
	}

	// exec streams (stdout/stderr arrive in real time as multiple JSON frames),
	// so it has its own client path. Everything else is one request → one response.
	if op == "exec" {
		return ctlStreamExec(sock, req)
	}
	resp, err := daemonclient.Roundtrip(sock, req)
	if err != nil {
		return err
	}
	return ctlPrint(op, resp)
}

// ctlStreamExec sends an exec request and prints frames as they arrive until
// the terminal frame (OK or Error). Decoding into the typed protocol.Response
// means no map[string]any reflection and no JSON-number-as-float64 quirks.
func ctlStreamExec(sock string, req map[string]any) error {
	conn, err := daemonclient.Dial(sock, req)
	if err != nil {
		return err
	}
	defer conn.Close()
	for {
		f, err := conn.Recv()
		if err != nil {
			return err
		}
		if f.Stdout != "" {
			fmt.Print(f.Stdout)
		}
		if f.Stderr != "" {
			fmt.Fprint(os.Stderr, f.Stderr)
		}
		if f.Error != "" {
			return fmt.Errorf("%s", f.Error)
		}
		if f.OK {
			if f.Node != "" {
				fmt.Printf("node %s\n", f.Node)
			}
			// Propagate the inner command's exit code as a typed error so
			// cli.Run translates it into the process's exit status. Without
			// this, scripts like `agentenv ctl exec test -f X && ...` are
			// silently always-true — the non-zero exit only reached stderr
			// as "[exit N]" and the process itself exit'd 0. Same pattern as
			// the `agentenv exec` path in cli/commands.go.
			if f.Exit != nil && *f.Exit != 0 {
				return ExitError(*f.Exit)
			}
			return nil
		}
	}
}

func ctlPrint(op string, r *protocol.Response) error {
	if !r.OK {
		return fmt.Errorf("%s", r.Error)
	}
	switch op {
	case "checkout", "commit", "head":
		if r.Node != "" {
			fmt.Printf("node %s\n", r.Node)
		}
		if r.Head != "" {
			fmt.Printf("HEAD %s\n", r.Head)
		}
	case "log", "branches":
		for _, n := range r.Nodes {
			mark := ""
			if n.Head {
				mark = "  <- HEAD"
			}
			fmt.Printf("%s  %q%s\n", n.ID, n.Message, mark)
		}
	case "show", "diff":
		for _, c := range r.Changes {
			fmt.Printf("%s %s\n", c.Kind, c.Path)
		}
		fmt.Printf("%d changed\n", len(r.Changes))
	case "ps":
		for _, p := range r.Procs {
			fmt.Printf("%d\t%s\n", p.PID, p.Args)
		}
	case "tag":
		switch {
		case len(r.Tags) > 0:
			for name, id := range r.Tags {
				fmt.Printf("%-20s %s\n", name, id)
			}
		case r.Node != "":
			fmt.Println(r.Node)
		default:
			fmt.Println("ok")
		}
	case "tournament":
		for _, b := range r.Branches {
			verdict := fmt.Sprintf("exit %d", b.TestExit)
			if b.TestExit == 0 {
				verdict = "PASS"
			}
			cmd := b.Cmd
			if len(cmd) > 50 {
				cmd = cmd[:49] + "…"
			}
			fmt.Printf("  branch %s  →  %s  (%s)  [%s]\n", b.Name, b.Node, verdict, cmd)
		}
		if r.Winner != "" {
			fmt.Printf("winner: branch %s  (node %s)\n", r.Winner, r.WinnerNode)
		} else {
			fmt.Println("no candidate passed the test")
		}
		if r.Head != "" {
			fmt.Printf("HEAD %s\n", r.Head)
		}
	default:
		fmt.Println("ok")
	}
	return nil
}

// stripFlagVal removes a flag and its value from args, in either form:
//
//	--flag value   (two argv elements)
//	--flag=value   (one argv element)
func stripFlagVal(args []string, flag string) []string {
	prefix := flag + "="
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++ // skip the value too
			continue
		}
		if strings.HasPrefix(args[i], prefix) {
			continue // drop "--flag=value" as one element
		}
		out = append(out, args[i])
	}
	return out
}
