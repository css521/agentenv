//go:build linux

// HTTP transport for the agentenv API — a REST surface alongside the existing
// newline-JSON unix socket. Built on huma v2, which derives OpenAPI 3.1 from
// the typed Go handler signatures, so the spec at /openapi.json and the
// interactive docs at /docs are always in sync with the running code.
//
// Why both transports? The unix socket stays as the zero-config, local-only,
// no-auth-needed channel that MCP and the bare CLI auto-route through. HTTP
// (off by default) is the one to enable when a non-Go client wants curl /
// Postman / a generated SDK / Swagger UI — i.e. all the "I expected this to
// be normal" expectations a Web/AI developer brings.
//
// Streaming `exec` is NOT yet exposed over HTTP (Server-Sent Events port is
// follow-up); use the socket for streaming today.

package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/css521/agentenv/internal/protocol"
	"github.com/css521/agentenv/internal/repo"
)

// HTTPVersion is reported in the OpenAPI spec's info block. Bumped manually
// alongside backwards-incompatible HTTP-layer changes (independent of the
// agentenv release tag, which can grow without changing this surface).
const HTTPVersion = "0.3.0"

// ServeHTTP runs the HTTP API on addr (e.g. "127.0.0.1:8911"). Blocks until ctx
// is cancelled. When token != "", every request must carry
// `Authorization: Bearer <token>` — non-loopback addresses without a token are
// rejected at startup as a safety guard. Compatible with the socket dispatch:
// both paths call the same r.* and the same repo handles the locking.
func ServeHTTP(ctx context.Context, r *repo.Repo, _ *repo.Capturer, addr, token string) error {
	if token == "" && !isLoopback(addr) {
		return fmt.Errorf("refusing to bind HTTP on non-loopback %s without AGENTENV_HTTP_TOKEN — set a token or bind 127.0.0.1", addr)
	}

	mux := http.NewServeMux()
	cfg := huma.DefaultConfig("agentenv", HTTPVersion)
	cfg.Info.Description = "REST surface for the agentenv daemon. " +
		"Same operations as the unix-socket protocol, plus an OpenAPI 3.1 spec " +
		"at /openapi.json and interactive docs at /docs. " +
		"Streaming `exec` is socket-only; everything else is here."
	if token != "" {
		cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
			"bearer": {Type: "http", Scheme: "bearer"},
		}
		cfg.Security = []map[string][]string{{"bearer": {}}}
	}
	api := humago.New(mux, cfg)
	registerRoutes(api, r)

	// Token middleware — applied only when configured.
	var handler http.Handler = mux
	if token != "" {
		handler = bearerAuth(mux, token)
	}

	srv := &http.Server{Addr: addr, Handler: handler}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "agentenv HTTP listening on http://%s (docs http://%s/docs)\n", ln.Addr(), ln.Addr())
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// isLoopback reports whether addr binds only to a loopback interface. A bare
// ":PORT" (host empty) in Go's net.Listen binds ALL interfaces, so we treat
// that as non-loopback — the safer default for the no-token guard.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func bearerAuth(h http.Handler, token string) http.Handler {
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// /openapi.json and /docs stay open so users can discover the API before
		// they realize they need a token. The 401 from /v1/* will say what to do.
		if strings.HasPrefix(req.URL.Path, "/v1/") && req.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentenv"`)
			http.Error(w, "missing or wrong Authorization: Bearer token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, req)
	})
}

// --- typed I/O for huma ----------------------------------------------------

// All response bodies use the explicit `Body` field huma expects, so the
// returned struct doubles as the HTTP envelope (status code defaulted to 200).

type headOut struct {
	Body struct {
		Head string `json:"head" doc:"the current HEAD node id"`
	}
}

type logOut struct {
	Body struct {
		Head  string           `json:"head"`
		Nodes []protocol.Node  `json:"nodes"`
	}
}

type nodeIDIn struct {
	ID string `path:"id" doc:"node id, ID prefix, or tag name"`
}

type showOut struct {
	Body struct {
		Parent  string             `json:"parent"`
		Changes []protocol.Change  `json:"changes"`
	}
}

type diffIn struct {
	A string `query:"a" required:"true" doc:"first ref"`
	B string `query:"b" required:"true" doc:"second ref"`
}

type changesOut struct {
	Body struct {
		Changes []protocol.Change `json:"changes"`
	}
}

type commitIn struct {
	Body struct {
		Message string `json:"message" doc:"commit message"`
	}
}

type nodeRefOut struct {
	Body struct {
		Node string `json:"node,omitempty"`
		Head string `json:"head"`
	}
}

type checkoutIn struct {
	Body struct {
		Node string `json:"node" required:"true" doc:"node id, ID prefix, or tag name"`
	}
}

type headOnlyOut struct {
	Body struct {
		Head string `json:"head"`
	}
}

type tagSetIn struct {
	Body struct {
		Name string `json:"name" required:"true"`
		Ref  string `json:"ref"  required:"true" doc:"target node id, prefix, or another tag"`
	}
}

type emptyOut struct{ Body struct{} }

type tagsListOut struct {
	Body struct {
		Tags map[string]string `json:"tags"`
	}
}

type tournamentIn struct {
	Body struct {
		Base       string                          `json:"base" doc:"base ref (default HEAD)"`
		Test       string                          `json:"test" required:"true" doc:"test command run inside each candidate"`
		Candidates []protocol.TournamentCandidate  `json:"candidates" required:"true" minItems:"1"`
	}
}

type tournamentOut struct {
	Body struct {
		Head       string                       `json:"head"`
		Winner     string                       `json:"winner,omitempty"`
		WinnerNode string                       `json:"winner_node,omitempty"`
		Branches   []protocol.TournamentBranch  `json:"branches"`
	}
}

type gcOut struct {
	Body struct {
		Removed []string `json:"removed"`
	}
}

// --- routes ----------------------------------------------------------------
//
// Every route goes through huma.Register (not Get/Post helpers) so each gets a
// hand-written Summary, Description, and Tag. The Tags group the operations in
// the /docs UI (and any generated SDK): "History" / "Mutation" / "Tags" /
// "Branching". The Summary is what shows up as the operation title in redoc /
// Swagger; it must read as English, not "Get v1 head".

const (
	tagHistory   = "History"
	tagMutation  = "Mutation"
	tagTags      = "Tags"
	tagBranching = "Branching"
)

func registerRoutes(api huma.API, r *repo.Repo) {
	huma.Register(api, huma.Operation{
		OperationID: "get-head", Method: http.MethodGet, Path: "/v1/head",
		Summary:     "Get the current HEAD node id",
		Description: "Returns the id of the node the working rootfs is currently derived from.",
		Tags:        []string{tagHistory},
	}, func(_ context.Context, _ *struct{}) (*headOut, error) {
		var o headOut
		o.Body.Head = r.Head()
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-log", Method: http.MethodGet, Path: "/v1/log",
		Summary:     "List every node in the commit DAG",
		Description: "Oldest first. Each entry includes parent id, message, and whether it is HEAD or a leaf.",
		Tags:        []string{tagHistory},
	}, func(_ context.Context, _ *struct{}) (*logOut, error) {
		var o logOut
		head := r.Head()
		o.Body.Head = head
		for _, n := range r.Nodes() {
			o.Body.Nodes = append(o.Body.Nodes, protocol.Node{
				ID: n.ID, Parent: n.Parent, Message: n.Message,
				Head: n.ID == head, Leaf: len(n.Children) == 0,
			})
		}
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-branches", Method: http.MethodGet, Path: "/v1/branches",
		Summary:     "List branch tips (distinct explored end-states)",
		Description: "Returns the leaf nodes of the DAG — each is one fully-explored path.",
		Tags:        []string{tagBranching},
	}, func(_ context.Context, _ *struct{}) (*logOut, error) {
		var o logOut
		head := r.Head()
		o.Body.Head = head
		for _, n := range r.Leaves() {
			o.Body.Nodes = append(o.Body.Nodes, protocol.Node{
				ID: n.ID, Parent: n.Parent, Message: n.Message,
				Head: n.ID == head, Leaf: true,
			})
		}
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "show-node", Method: http.MethodGet, Path: "/v1/nodes/{id}",
		Summary:     "Show files a node changed vs its parent",
		Description: "Like `git show --stat`: which files this commit added, removed, or modified.",
		Tags:        []string{tagHistory},
	}, func(_ context.Context, in *nodeIDIn) (*showOut, error) {
		ch, parent, err := r.Show(in.ID)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		var o showOut
		o.Body.Parent = parent
		o.Body.Changes = changes(ch)
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "diff-nodes", Method: http.MethodGet, Path: "/v1/diff",
		Summary:     "Diff two nodes",
		Description: "Returns the files that differ between the two refs. Useful for comparing branch tips.",
		Tags:        []string{tagHistory},
	}, func(_ context.Context, in *diffIn) (*changesOut, error) {
		ch, err := r.Diff(in.A, in.B)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		var o changesOut
		o.Body.Changes = changes(ch)
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "commit", Method: http.MethodPost, Path: "/v1/commit",
		Summary:     "Snapshot the current working rootfs",
		Description: "Freezes the working rootfs as a new immutable node, child of HEAD, and advances HEAD.",
		Tags:        []string{tagMutation},
	}, func(_ context.Context, in *commitIn) (*nodeRefOut, error) {
		n, err := r.Commit(in.Body.Message, "")
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		var o nodeRefOut
		o.Body.Node, o.Body.Head = n.ID, r.Head()
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "checkout", Method: http.MethodPost, Path: "/v1/checkout",
		Summary:     "Roll the whole environment back to a node",
		Description: "Kills processes in the inner env, rebuilds work/current from the target node, and points HEAD there. Accepts a full node id, an ID prefix, or a tag name.",
		Tags:        []string{tagMutation},
	}, func(_ context.Context, in *checkoutIn) (*headOnlyOut, error) {
		if err := r.Checkout(in.Body.Node); err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		var o headOnlyOut
		o.Body.Head = r.Head()
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-node", Method: http.MethodDelete, Path: "/v1/nodes/{id}",
		Summary:     "Delete a node from the DAG",
		Description: "Splices the node out: its children re-parent to its parent so descendants survive. Refuses to delete the current HEAD or the only remaining node.",
		Tags:        []string{tagMutation},
	}, func(_ context.Context, in *nodeIDIn) (*headOnlyOut, error) {
		if err := r.Delete(in.ID); err != nil {
			// repo.Delete returns "is HEAD" / "only node" as plain errors; 409
			// communicates "you can't delete this right now" (a 404 would
			// mis-imply the node doesn't exist).
			return nil, huma.Error409Conflict(err.Error())
		}
		var o headOnlyOut
		o.Body.Head = r.Head()
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-tags", Method: http.MethodGet, Path: "/v1/tags",
		Summary:     "List all tags",
		Description: "Returns a map of tag name to node id.",
		Tags:        []string{tagTags},
	}, func(_ context.Context, _ *struct{}) (*tagsListOut, error) {
		var o tagsListOut
		o.Body.Tags = r.Tags()
		if o.Body.Tags == nil {
			o.Body.Tags = map[string]string{}
		}
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-tag", Method: http.MethodPost, Path: "/v1/tags",
		Summary:     "Set (or update) a tag",
		Description: "Points the named tag at a node. The ref can be a full id, an ID prefix, another tag, or `HEAD`.",
		Tags:        []string{tagTags},
	}, func(_ context.Context, in *tagSetIn) (*emptyOut, error) {
		if err := r.SetTag(in.Body.Name, in.Body.Ref); err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		return &emptyOut{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "tournament", Method: http.MethodPost, Path: "/v1/tournament",
		Summary:     "Run N candidates in parallel, keep the winner",
		Description: "Forks a fresh workspace from `base` for each candidate, runs the candidate's command, then runs `test` inside it. The first branch whose test exits 0 wins; HEAD moves to that branch. All branches are committed regardless of outcome — losers stay in the DAG and can be `DELETE`d later if desired.",
		Tags:        []string{tagBranching},
	}, func(_ context.Context, in *tournamentIn) (*tournamentOut, error) {
		pairs := make([]struct{ Name, Cmd string }, len(in.Body.Candidates))
		for i, c := range in.Body.Candidates {
			pairs[i] = struct{ Name, Cmd string }{c.Name, c.Cmd}
		}
		base := in.Body.Base
		if base == "" {
			base = "HEAD"
		}
		res, err := r.Tournament(base, in.Body.Test, pairs, true)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		var o tournamentOut
		o.Body.Head = r.Head()
		o.Body.Winner = res.Winner
		o.Body.WinnerNode = res.WinnerNode
		for _, c := range res.Candidates {
			o.Body.Branches = append(o.Body.Branches, protocol.TournamentBranch{
				Name: c.Name, Cmd: c.Cmd, Node: c.Node, TestExit: c.TestExit,
			})
		}
		return &o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "gc", Method: http.MethodPost, Path: "/v1/gc",
		Summary:     "Reclaim disk by removing orphan snapshots",
		Description: "Deletes on-disk snapshots that no node references (e.g. from interrupted operations, or from `DELETE /v1/nodes/{id}` calls that didn't reach the snapshot store). Returns the list of removed node ids.",
		Tags:        []string{tagMutation},
	}, func(_ context.Context, _ *struct{}) (*gcOut, error) {
		removed, err := r.GC()
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		var o gcOut
		o.Body.Removed = removed
		return &o, nil
	})
}
