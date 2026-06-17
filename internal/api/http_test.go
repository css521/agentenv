//go:build linux

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	// The lock-down rule: any bind that is NOT clearly loopback requires a token.
	// The bare ":PORT" form is the common foot-gun — Go binds it to ALL
	// interfaces, so we MUST treat it as non-loopback or we'd accidentally
	// expose the daemon on a LAN-reachable port with no auth.
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8911", true},
		{"localhost:8911", true},
		{"[::1]:8911", true},
		{":8911", false},        // bare port = all interfaces
		{"0.0.0.0:8911", false}, // explicit all-interfaces
		{"[::]:8911", false},
		{"10.0.0.1:8911", false},
		{"example.com:80", false},
	}
	for _, c := range cases {
		if got := isLoopback(c.addr); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestBearerAuth(t *testing.T) {
	// /v1/* must demand the token; /openapi.json and /docs stay open so users
	// can discover the API before they realize a token is needed.
	inner := http.NewServeMux()
	inner.HandleFunc("/v1/head", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	inner.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	inner.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := bearerAuth(inner, "secret")

	check := func(name, method, path, auth string, wantStatus int) {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != wantStatus {
			t.Errorf("%s: got %d, want %d", name, rr.Code, wantStatus)
		}
	}
	check("no-token /v1/head",      "GET", "/v1/head",      "",                401)
	check("wrong-token /v1/head",   "GET", "/v1/head",      "Bearer nope",     401)
	check("right-token /v1/head",   "GET", "/v1/head",      "Bearer secret",   204)
	check("no-token /openapi.json", "GET", "/openapi.json", "",                200)
	check("no-token /docs",         "GET", "/docs",         "",                200)
}
