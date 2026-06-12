// Package mcp implements the airlock MCP (Model Context Protocol) server that
// exposes port-management tools to agents over the local loopback interface.
//
// This file provides the transport and security layer: a loopback-only HTTP
// server that speaks JSON-RPC 2.0 over POST, guarded by a defence-in-depth set
// of middleware checks (method/path, Host allowlist, Origin rejection,
// Content-Type, body size limit, and optional bearer-token auth). The actual
// tool dispatch is delegated to a pluggable Handler so the tool logic can be
// developed independently.
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// JSON-RPC 2.0 error codes used by the transport layer.
const (
	// codeParseError signals invalid JSON or a malformed JSON-RPC envelope.
	codeParseError = -32700
)

// defaultAddr is the loopback address the server binds when Options.Addr is
// empty. It is intentionally loopback-only: airlock never listens on a routable
// interface.
const defaultAddr = "127.0.0.1:27183"

// defaultMaxBodyBytes caps the size of a request body (1 MiB) when
// Options.MaxBodyBytes is not set. Requests larger than this are rejected with
// 413 before reaching the handler.
const defaultMaxBodyBytes int64 = 1 << 20

// RPCRequest is a parsed JSON-RPC 2.0 request. ID is kept as a json.RawMessage
// so it can be echoed back byte-for-byte (number, string, or null) without
// lossy round-tripping through a concrete Go type.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// RPCError is a JSON-RPC 2.0 error object returned to the client when a method
// invocation fails.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Handler turns a method+params into a result or an RPCError. The transport
// layer marshals whatever result is returned into the JSON-RPC envelope. A
// non-nil rpcErr takes precedence over result. Implemented by the tool layer.
type Handler interface {
	Handle(ctx context.Context, method string, params json.RawMessage) (result any, rpcErr *RPCError)
}

// Options configures a Server.
type Options struct {
	// Addr is the loopback address to bind. Defaults to "127.0.0.1:27183".
	// Use "127.0.0.1:0" to bind an ephemeral port (see Addr).
	Addr string
	// Token, when non-empty, requires every JSON-RPC request to present
	// "Authorization: Bearer <token>". When empty, no auth is enforced and the
	// caller is trusted (the daemon decides per-platform).
	Token string
	// MaxBodyBytes caps the request body size. Defaults to 1 MiB.
	MaxBodyBytes int64
}

// Server is the airlock MCP HTTP server. It hosts the JSON-RPC transport,
// applies the security middleware, and dispatches to a pluggable Handler.
type Server struct {
	addr         string
	token        string
	maxBodyBytes int64
	handler      Handler

	httpServer *http.Server

	mu       sync.Mutex
	listener net.Listener
}

// New constructs a Server that will dispatch JSON-RPC calls to h, configured by
// opts. The returned Server is inert until Start is called.
func New(h Handler, opts Options) *Server {
	addr := opts.Addr
	if addr == "" {
		addr = defaultAddr
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	return &Server{
		addr:         addr,
		token:        opts.Token,
		maxBodyBytes: maxBody,
		handler:      h,
	}
}

// Start binds the loopback listener and serves until ctx is cancelled, at which
// point it performs a graceful shutdown. It blocks for the lifetime of the
// server. A returned error is the first fatal serving error, or nil on a clean
// ctx-triggered shutdown.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("mcp: listen on %s: %w", s.addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", s.handleHealthz)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.mu.Lock()
	s.listener = ln
	s.httpServer = srv
	s.mu.Unlock()

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return <-serveErr
	case err := <-serveErr:
		return err
	}
}

// Addr returns the actual bound address. After Start has bound the listener
// (e.g. when the configured port was 0), this reflects the real ephemeral
// address; before Start it returns the configured address.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// handleHealthz serves the unauthenticated readiness probe. It still requires a
// loopback Host (DNS-rebinding defence) but bypasses auth and Content-Type.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !loopbackHost(r.Host) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleRoot is the JSON-RPC endpoint. It applies the security middleware in
// order, then parses, dispatches, and frames the JSON-RPC response.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// 1. Method/path: only POST to "/".
	if r.URL.Path != "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Host header allowlist (DNS-rebinding defence).
	if !loopbackHost(r.Host) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 3. Origin header: a legitimate local MCP client does not send one.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 4. Content-Type must be application/json (charset suffix allowed).
	if !hasJSONContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	// 6. Token auth (before reading the body). Empty token disables auth.
	if s.token != "" && !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 5. Body size limit. MaxBytesReader makes the body return an error on
	// overflow, which we surface as 413.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)

	var req RPCRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeParseError(w)
		return
	}
	if req.JSONRPC != "2.0" {
		writeParseError(w)
		return
	}

	// A notification (no id) named notifications/initialized gets a 202 with no
	// JSON-RPC body, matching MCP norms. The handler is not invoked.
	if req.Method == "notifications/initialized" && len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	id := normalizeID(req.ID)
	result, rpcErr := s.handler.Handle(r.Context(), req.Method, req.Params)
	if rpcErr != nil {
		writeRPCError(w, id, rpcErr)
		return
	}
	writeRPCResult(w, id, result)
}

// authorized reports whether r presents the configured bearer token. The
// comparison is constant-time to avoid leaking the token via timing.
func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// loopbackHost reports whether the Host header names a loopback host. The port
// (if any) is ignored: the port is ours because we bound it on loopback. Bare
// hosts without a port are also accepted.
func loopbackHost(host string) bool {
	if host == "" {
		return false
	}
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.TrimPrefix(hostname, "[")
	hostname = strings.TrimSuffix(hostname, "]")
	switch hostname {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// hasJSONContentType reports whether ct begins with application/json (an
// optional "; charset=..." suffix is allowed).
func hasJSONContentType(ct string) bool {
	ct = strings.TrimSpace(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return strings.EqualFold(ct, "application/json")
}

// normalizeID returns a JSON-RPC id suitable for echoing: the request's raw id
// if present, or a literal null otherwise.
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// rpcResponse is the JSON-RPC 2.0 response envelope. Exactly one of Result or
// Error is populated; the other is omitted.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// writeRPCResult writes a successful JSON-RPC response echoing id.
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// writeRPCError writes a JSON-RPC error response echoing id.
func writeRPCError(w http.ResponseWriter, id json.RawMessage, rpcErr *RPCError) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Error: rpcErr})
}

// writeParseError writes a JSON-RPC parse error (-32700) with a null id, used
// for malformed JSON or a bad envelope.
func writeParseError(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage("null"),
		Error:   &RPCError{Code: codeParseError, Message: "parse error"},
	})
}

// writeJSON marshals v and writes it with the given status. A marshal failure
// is surfaced as a 500; in practice v is always a serializable envelope.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// EnsureTokenFile reads the bearer token from path (expected mode 0600). If the
// file is missing and create is true, it mints a 32-byte crypto/rand hex token,
// writes it 0600 (creating the parent dir 0700), and returns it. If the file
// exists, its trimmed contents are returned. When create is false and the file
// is absent, it returns "" with a nil error.
func EnsureTokenFile(path string, create bool) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("mcp: read token file: %w", err)
	}
	if !create {
		return "", nil
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("mcp: create token dir: %w", err)
		}
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mcp: generate token: %w", err)
	}
	token := hex.EncodeToString(buf)

	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("mcp: write token file: %w", err)
	}
	return token, nil
}
