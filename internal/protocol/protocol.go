// Package protocol defines the wire types for the agentenv daemon's
// newline-delimited JSON API. Server (internal/api) and client (internal/cli's
// ctl) BOTH import this — having a single typed shape on both sides means the
// client can `json.NewDecoder(conn).Decode(&Response)` directly into a struct,
// instead of unmarshalling into map[string]any and doing fragile type
// assertions on every field.
//
// Protocol shape:
//
//   - 1 line of JSON in = 1 Request from the client.
//   - For all ops except "exec": exactly 1 line of JSON out = 1 Response.
//   - For "exec": multiple Response frames stream back — output frames carry
//     Stdout/Stderr only; the terminal frame carries OK=true (and Exit/Node/
//     Head) or Error="...". Clients keep reading frames until OK or Error.
package protocol

// Request is one operation the client wants to perform.
type Request struct {
	Op         string                `json:"op"`
	Cmd        string                `json:"cmd,omitempty"`
	Node       string                `json:"node,omitempty"`
	Message    string                `json:"message,omitempty"`
	A          string                `json:"a,omitempty"`
	B          string                `json:"b,omitempty"`
	Pid        int                   `json:"pid,omitempty"`
	Keep       int                   `json:"keep,omitempty"`
	Name       string                `json:"name,omitempty"`       // tag: name to set/get
	Ref        string                `json:"ref,omitempty"`        // tag: target ref
	Base       string                `json:"base,omitempty"`       // tournament: base ref
	Test       string                `json:"test,omitempty"`       // tournament: test command
	Candidates []TournamentCandidate `json:"candidates,omitempty"` // tournament: candidates
}

// TournamentCandidate is one branch to try in a tournament.
type TournamentCandidate struct {
	Name string `json:"name"`
	Cmd  string `json:"cmd"`
}

// TournamentBranch is one tournament result entry.
type TournamentBranch struct {
	Name     string `json:"name"`
	Cmd      string `json:"cmd"`
	Node     string `json:"node"`
	TestExit int    `json:"test_exit"`
}

// Node is one DAG node summary (log/branches output).
type Node struct {
	ID      string `json:"id"`
	Parent  string `json:"parent,omitempty"`
	Message string `json:"message"`
	Head    bool   `json:"head,omitempty"`
	Leaf    bool   `json:"leaf,omitempty"`
}

// Change is one entry in a diff/show response.
type Change struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// Proc is one tracked background process (ps output).
type Proc struct {
	PID  int    `json:"pid"`
	Args string `json:"args"`
	Log  string `json:"log"`
}

// Response is one frame the server emits. For non-exec ops it's the only frame.
// For exec it can be either a streaming output frame (only Stdout/Stderr set) or
// the terminal frame (OK or Error set; for OK, Exit/Node/Head also).
type Response struct {
	OK         bool               `json:"ok,omitempty"`
	Error      string             `json:"error,omitempty"`
	Tags       map[string]string  `json:"tags,omitempty"`
	Branches   []TournamentBranch `json:"branches,omitempty"`
	Winner     string             `json:"winner,omitempty"`
	WinnerNode string             `json:"winner_node,omitempty"`
	Exit       *int               `json:"exit,omitempty"`
	Stdout     string             `json:"stdout,omitempty"`
	Stderr     string             `json:"stderr,omitempty"`
	Node       string             `json:"node,omitempty"`
	Head       string             `json:"head,omitempty"`
	Parent     string             `json:"parent,omitempty"`
	Pid        int                `json:"pid,omitempty"`
	Log        string             `json:"log,omitempty"`
	Nodes      []Node             `json:"nodes,omitempty"`
	Changes    []Change           `json:"changes,omitempty"`
	Procs      []Proc             `json:"procs,omitempty"`
	Removed    []string           `json:"removed,omitempty"`
	Dropped    []string           `json:"dropped,omitempty"`
}

// Streaming returns true if this exec frame is a streaming output frame (i.e.
// the server is still sending — the client should keep reading until Terminal).
func (r *Response) Streaming() bool { return !r.OK && r.Error == "" }

// Terminal returns true if this frame ends the exec stream (OK or Error).
func (r *Response) Terminal() bool { return r.OK || r.Error != "" }
