package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/adamorad/airlock/v2/internal/store"
)

// newE2EHandler builds a real ToolHandler over a temp store for the HTTP
// end-to-end test.
func newE2EHandler(t *testing.T) *ToolHandler {
	t.Helper()
	s, err := store.OpenDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	lm := store.NewLockManager(s)
	pm := store.NewPresenceManager(s, lm)
	em := store.NewEventManager(s)
	tm := store.NewTaskManager(s)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lm.Start(ctx)
	pm.Start(ctx)
	tm.Start(ctx)

	return NewToolHandler(lm, pm, em, tm, s)
}

// TestHTTPEndToEnd drives a real Server: it POSTs an initialize and a
// lock_resource tools/call with proper Host/Content-Type and asserts the
// JSON-RPC envelope plus that the tool result rides inside content[0].text.
func TestHTTPEndToEnd(t *testing.T) {
	h := newE2EHandler(t)
	s, teardown := startServer(t, h, Options{})
	defer teardown()

	// 1. initialize.
	status, body := doPOST(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	if status != http.StatusOK {
		t.Fatalf("initialize status = %d; body=%s", status, body)
	}
	var initEnv struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &initEnv); err != nil {
		t.Fatalf("unmarshal initialize: %v; body=%s", err, body)
	}
	if initEnv.JSONRPC != "2.0" || string(initEnv.ID) != "1" {
		t.Fatalf("bad envelope: %s", body)
	}
	if initEnv.Result.ProtocolVersion != "2024-11-05" || initEnv.Result.ServerInfo.Name != "airlock" {
		t.Fatalf("bad initialize result: %s", body)
	}

	// 2. lock_resource via tools/call.
	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` +
		`{"name":"lock_resource","arguments":{"name":"f","agent_id":"A"}}}`
	status, body = doPOST(t, s, callBody, nil)
	if status != http.StatusOK {
		t.Fatalf("tools/call status = %d; body=%s", status, body)
	}
	var callEnv struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &callEnv); err != nil {
		t.Fatalf("unmarshal tools/call: %v; body=%s", err, body)
	}
	if string(callEnv.ID) != "2" {
		t.Fatalf("tools/call id = %s, want 2", callEnv.ID)
	}
	if len(callEnv.Result.Content) == 0 || callEnv.Result.Content[0].Type != "text" {
		t.Fatalf("missing text content: %s", body)
	}
	// The tool result rides inside content[0].text as a JSON string.
	var toolRes map[string]any
	if err := json.Unmarshal([]byte(callEnv.Result.Content[0].Text), &toolRes); err != nil {
		t.Fatalf("unmarshal tool result text: %v; text=%s", err, callEnv.Result.Content[0].Text)
	}
	if toolRes["locked"] != true {
		t.Fatalf("expected locked=true in tool result: %v", toolRes)
	}
	if toolRes["lock_token"] == nil || toolRes["lock_token"] == "" {
		t.Fatalf("expected lock_token in tool result: %v", toolRes)
	}
}
