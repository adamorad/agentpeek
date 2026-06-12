// Package mcp implements the airlock MCP (Model Context Protocol) server that
// exposes port-management tools to agents over the local loopback interface.
package mcp

import "github.com/adamorad/airlock/internal/store"

// Server is the airlock MCP server. It will host the JSON-RPC/MCP transport,
// register tool handlers, and mediate access to persistent state via the store.
type Server struct {
	addr  string
	store *store.Store
}

// New constructs a Server bound to addr (e.g. "127.0.0.1:27183") and backed by
// the given store. The returned Server is inert until Start is called.
func New(addr string, st *store.Store) *Server {
	return &Server{addr: addr, store: st}
}

// Start runs the MCP server, blocking until the server stops or ctx-equivalent
// shutdown is signalled.
//
// Placeholder: this stub returns nil immediately. A future task will:
//   - bind a listener on s.addr (loopback only),
//   - register the MCP tool set (get_available_port, reserve_port, etc.),
//   - serve requests until shutdown,
//   - and return any fatal serving error.
func (s *Server) Start() error {
	return nil
}
