// Entry point + dispatcher. Mirrors v6 app.ts routing exactly: the store is
// created once, the process listens on 127.0.0.1:0, prints "PORT <n>" as its
// first stdout line, then serves forever. One global mutex guards the whole
// dispatch, so requests are processed sequentially exactly like the
// single-threaded TS server; the transport reads headers and bodies without
// holding the lock.
package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
)

const defaultTrialNow = "2026-01-01T00:00:00Z"

type server struct {
	mu      sync.Mutex // guards the whole dispatch, including the store
	store   *Store
	token   string
	counter int // request counter; IDs are "req-N"
}

func main() {
	trialNow := os.Getenv("MAKEFUN_TRIAL_NOW")
	if trialNow == "" {
		trialNow = defaultTrialNow
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")

	srv := &server{
		store: NewStore(trialNow),
		token: token,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// PORT <n> must be the FIRST stdout line; the Node shim waits for it.
	fmt.Printf("PORT %d\n", port)
	_ = os.Stdout.Sync()

	serveListener(ln, srv.dispatch)
}

// dispatch mints the request ID once, then routes exactly as v6 app.ts
// does. A handler panic becomes a 500 with the same three headers, never a
// dropped connection.
func (s *server) dispatch(req *Request) (resp *Response) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counter++
	req.ID = "req-" + itoa(s.counter)
	id := req.ID

	defer func() {
		if r := recover(); r != nil {
			resp = SendRaw(500, marshalJSON(map[string]string{"message": "Internal Server Error"}), id)
			fmt.Fprintf(os.Stderr, "[app] unhandled: %v\n", r)
		}
	}()

	rawPath := req.Path
	method := req.Method

	// Health probe: no auth.
	if method == "GET" && rawPath == "/healthz" {
		return SendJSON(200, map[string]bool{"ok": true}, id)
	}
	// MCP endpoint.
	if rawPath == "/mcp" {
		return HandleMcp(req, s.store, s.token)
	}
	// REST v3 surface. NOTE: this check comes before the GraphQL check,
	// exactly as in app.ts — so /api/v3/graphql is routed to REST, not to
	// GraphQL (the GraphQL branch below is unreachable for that path).
	if rawPath == "/api/v3" || strings.HasPrefix(rawPath, "/api/v3/") {
		sub := ""
		if rawPath != "/api/v3" {
			sub = rawPath[len("/api/v3"):]
		}
		if len(sub) > 1 && strings.HasSuffix(sub, "/") {
			sub = sub[:len(sub)-1]
		}
		restReq := *req
		restReq.Path = sub
		return HandleRest(&restReq, s.store, s.token)
	}
	// GraphQL endpoint.
	if rawPath == "/api/graphql" || rawPath == "/api/v3/graphql" {
		return HandleGraphQL(req, s.store, s.token)
	}
	// Git smart HTTP: /{owner}/{repo}.git/info/refs, git-upload-pack,
	// git-receive-pack.
	if handled, gitResp := HandleGit(req, s.store, s.token); handled {
		if gitResp == nil {
			return SendRaw(500, marshalJSON(map[string]string{"message": "Internal Server Error"}), id)
		}
		return gitResp
	}
	// Everything else, every method: GitHub's 404 body.
	return ErrJSON(404, "Not Found", id)
}
