package mcp

import (
	"context"
	"encoding/json"

	"github.com/adamorad/airlock/internal/store"
	"github.com/adamorad/airlock/internal/version"
)

// protocolVersion is the MCP protocol revision airlock implements. It is echoed
// in the initialize result and must match what the client negotiated.
const protocolVersion = "2024-11-05"

// ToolHandler wires the MCP JSON-RPC method surface (initialize, tools/list,
// tools/call, notifications/initialized) to the underlying store managers via an
// embedded toolHandler. It implements the mcp.Handler interface consumed by the
// transport in server.go.
type ToolHandler struct {
	*toolHandler
}

// NewToolHandler constructs the MCP handler over the given store managers. The
// returned value implements mcp.Handler and can be passed to mcp.New.
func NewToolHandler(
	locks *store.LockManager,
	presence *store.PresenceManager,
	events *store.EventManager,
	tasks *store.TaskManager,
	s *store.Store,
) *ToolHandler {
	return &ToolHandler{
		toolHandler: &toolHandler{
			locks:    locks,
			presence: presence,
			events:   events,
			tasks:    tasks,
			store:    s,
		},
	}
}

// toolsCallParams is the params object for a tools/call request: the tool name
// and its free-form arguments map.
type toolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Handle dispatches a JSON-RPC method to the matching MCP handler. It implements
// mcp.Handler: a non-nil *RPCError signals a JSON-RPC fault; otherwise the
// returned value is marshalled into the result envelope by the transport.
func (h *ToolHandler) Handle(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case "initialize":
		return h.initialize(), nil
	case "tools/list":
		return map[string]any{"tools": toolDefs()}, nil
	case "tools/call":
		return h.toolsCall(ctx, params)
	case "notifications/initialized":
		// The transport 202s these before reaching Handle; if one does arrive
		// here (e.g. a client that sends it with an id), it is a no-op.
		return nil, nil
	default:
		return nil, &RPCError{Code: -32601, Message: "method not found: " + method}
	}
}

// initialize builds the MCP initialize result advertising our protocol version,
// (empty) tools capability, and server identity.
func (h *ToolHandler) initialize() map[string]any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    version.Name,
			"version": version.Number,
		},
	}
}

// toolsCall handles a tools/call request: it parses {name, arguments}, dispatches
// to the tool, JSON-marshals the tool's result map to a string, and returns it
// inside content[0].text (matching v1 — the structured payload rides in text).
func (h *ToolHandler) toolsCall(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p toolsCallParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: codeParseError, Message: "invalid tools/call params"}
		}
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}

	result := h.callTool(ctx, p.Name, p.Arguments)

	text, err := json.Marshal(result)
	if err != nil {
		// A tool result is always a plain map of JSON-serializable values; a
		// marshal failure would be a programming error, surfaced as a fault.
		return nil, &RPCError{Code: codeParseError, Message: "tool result not serializable"}
	}

	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(text)},
		},
	}, nil
}
