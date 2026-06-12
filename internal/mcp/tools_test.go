package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/adamorad/airlock/internal/store"
)

// newTestHandler opens a temp-DB store, builds the managers, starts their
// reapers (cancelled at test end), and returns a ready ToolHandler.
func newTestHandler(t *testing.T) (*ToolHandler, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	s, err := store.OpenDB(dbPath)
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

	return NewToolHandler(lm, pm, em, tm, s), s
}

// callToolJSON invokes a tool through the full Handle path (tools/call) and
// returns the decoded tool-result map that rides inside content[0].text.
func callToolJSON(t *testing.T, h *ToolHandler, name string, args map[string]any) map[string]any {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	res, rpcErr := h.Handle(context.Background(), "tools/call", params)
	if rpcErr != nil {
		t.Fatalf("tools/call %s: rpc error %+v", name, rpcErr)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("tools/call %s: result not a map: %T", name, res)
	}
	content, ok := m["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tools/call %s: missing content: %v", name, m)
	}
	text, ok := content[0]["text"].(string)
	if !ok {
		t.Fatalf("tools/call %s: content[0].text not a string", name)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("tools/call %s: unmarshal text: %v", name, err)
	}
	return out
}

func TestInitialize(t *testing.T) {
	h, _ := newTestHandler(t)
	res, rpcErr := h.Handle(context.Background(), "initialize", nil)
	if rpcErr != nil {
		t.Fatalf("initialize: %+v", rpcErr)
	}
	m := res.(map[string]any)
	if m["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", m["protocolVersion"])
	}
	info := m["serverInfo"].(map[string]any)
	if info["name"] != "airlock" {
		t.Errorf("serverInfo.name = %v, want airlock", info["name"])
	}
	if _, ok := m["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities.tools missing")
	}
}

func TestToolsList(t *testing.T) {
	h, _ := newTestHandler(t)
	res, rpcErr := h.Handle(context.Background(), "tools/list", nil)
	if rpcErr != nil {
		t.Fatalf("tools/list: %+v", rpcErr)
	}
	defs := res.(map[string]any)["tools"].([]toolDef)
	// 5 locks + 6 notes/state + 3 presence + 3 events + 5 tasks = 22.
	const want = 22
	if len(defs) != want {
		t.Fatalf("tools/list returned %d tools, want %d", len(defs), want)
	}
	// Each def must carry a name, description, and an object inputSchema.
	for _, d := range defs {
		if d.Name == "" || d.Description == "" {
			t.Errorf("tool %q missing name/description", d.Name)
		}
		if d.InputSchema["type"] != "object" {
			t.Errorf("tool %q inputSchema type = %v", d.Name, d.InputSchema["type"])
		}
		if _, ok := d.InputSchema["required"]; !ok {
			t.Errorf("tool %q inputSchema missing required", d.Name)
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	h, _ := newTestHandler(t)
	_, rpcErr := h.Handle(context.Background(), "bogus/method", nil)
	if rpcErr == nil || rpcErr.Code != -32601 {
		t.Fatalf("expected -32601, got %+v", rpcErr)
	}
}

func TestUnknownTool(t *testing.T) {
	h, _ := newTestHandler(t)
	out := callToolJSON(t, h, "no_such_tool", map[string]any{})
	if out["error"] != "unknown tool: no_such_tool" {
		t.Fatalf("unknown tool error = %v", out["error"])
	}
}

func TestMissingRequiredArg(t *testing.T) {
	h, _ := newTestHandler(t)
	out := callToolJSON(t, h, "lock_resource", map[string]any{"agent_id": "A"})
	if out["error"] != "missing name" {
		t.Fatalf("expected missing name, got %v", out["error"])
	}
}

func TestLockResource_AcquireThenContended(t *testing.T) {
	h, _ := newTestHandler(t)

	got := callToolJSON(t, h, "lock_resource", map[string]any{"name": "f", "agent_id": "A"})
	if got["locked"] != true {
		t.Fatalf("A should lock f: %v", got)
	}
	if got["lock_token"] == "" || got["lock_token"] == nil {
		t.Fatalf("expected lock_token: %v", got)
	}
	token := got["lock_token"].(string)

	// Contended non-blocking call returns locked:false immediately (v1 behavior).
	got2 := callToolJSON(t, h, "lock_resource", map[string]any{"name": "f", "agent_id": "B"})
	if got2["locked"] != false {
		t.Fatalf("B should NOT lock f: %v", got2)
	}
	if got2["held_by"] != "A" {
		t.Fatalf("held_by = %v, want A", got2["held_by"])
	}

	// Unlock by token releases it.
	rel := callToolJSON(t, h, "unlock_resource", map[string]any{"name": "f", "lock_token": token})
	if rel["released"] != true {
		t.Fatalf("unlock by token should release: %v", rel)
	}

	// Now B can take it.
	got3 := callToolJSON(t, h, "lock_resource", map[string]any{"name": "f", "agent_id": "B"})
	if got3["locked"] != true {
		t.Fatalf("B should lock f after release: %v", got3)
	}
}

func TestRenewLock_ByTokenAndByAgent(t *testing.T) {
	h, _ := newTestHandler(t)

	got := callToolJSON(t, h, "lock_resource", map[string]any{"name": "r", "agent_id": "A", "ttl_seconds": float64(30)})
	token := got["lock_token"].(string)

	byTok := callToolJSON(t, h, "renew_lock", map[string]any{"name": "r", "lock_token": token, "ttl_seconds": float64(60)})
	if byTok["renewed"] != true {
		t.Fatalf("renew by token: %v", byTok)
	}

	byAgent := callToolJSON(t, h, "renew_lock", map[string]any{"name": "r", "agent_id": "A", "ttl_seconds": float64(90)})
	if byAgent["renewed"] != true {
		t.Fatalf("renew by agent: %v", byAgent)
	}

	// Wrong agent cannot renew.
	bad := callToolJSON(t, h, "renew_lock", map[string]any{"name": "r", "agent_id": "Z"})
	if bad["renewed"] != false {
		t.Fatalf("renew by wrong agent should fail: %v", bad)
	}
}

func TestListAndLockMany(t *testing.T) {
	h, _ := newTestHandler(t)

	many := callToolJSON(t, h, "lock_resources", map[string]any{
		"names": []any{"x", "y"}, "agent_id": "A",
	})
	if many["locked"] != true {
		t.Fatalf("lock_resources should acquire: %v", many)
	}
	toks := many["tokens"].(map[string]any)
	if toks["x"] == nil || toks["y"] == nil {
		t.Fatalf("expected tokens for x and y: %v", toks)
	}

	list := callToolJSON(t, h, "list_locks", map[string]any{})
	locks := list["locks"].([]any)
	if len(locks) != 2 {
		t.Fatalf("expected 2 locks, got %d: %v", len(locks), locks)
	}

	// Contended batch returns locked:false.
	clash := callToolJSON(t, h, "lock_resources", map[string]any{
		"names": []any{"y", "z"}, "agent_id": "B",
	})
	if clash["locked"] != false || clash["held_by"] != "A" {
		t.Fatalf("contended batch: %v", clash)
	}
}

func TestNotes(t *testing.T) {
	h, _ := newTestHandler(t)

	if got := callToolJSON(t, h, "set_note", map[string]any{"key": "k", "value": "v", "author": "me"}); got["saved"] != true {
		t.Fatalf("set_note: %v", got)
	}
	got := callToolJSON(t, h, "get_note", map[string]any{"key": "k"})
	if got["value"] != "v" || got["author"] != "me" {
		t.Fatalf("get_note: %v", got)
	}

	miss := callToolJSON(t, h, "get_note", map[string]any{"key": "nope"})
	if miss["found"] != false {
		t.Fatalf("get_note absent: %v", miss)
	}

	list := callToolJSON(t, h, "list_notes", map[string]any{})
	if len(list["notes"].([]any)) != 1 {
		t.Fatalf("list_notes: %v", list)
	}

	// CAS: wrong expected does not swap; correct expected swaps.
	noSwap := callToolJSON(t, h, "set_note_if", map[string]any{"key": "k", "expected_value": "wrong", "new_value": "v2"})
	if noSwap["swapped"] != false {
		t.Fatalf("set_note_if mismatch should not swap: %v", noSwap)
	}
	swap := callToolJSON(t, h, "set_note_if", map[string]any{"key": "k", "expected_value": "v", "new_value": "v2"})
	if swap["swapped"] != true {
		t.Fatalf("set_note_if match should swap: %v", swap)
	}

	del := callToolJSON(t, h, "delete_note", map[string]any{"key": "k"})
	if del["deleted"] != true {
		t.Fatalf("delete_note: %v", del)
	}
}

func TestIncrementCounter(t *testing.T) {
	h, _ := newTestHandler(t)
	first := callToolJSON(t, h, "increment_counter", map[string]any{"name": "c"})
	if first["value"].(float64) != 1 {
		t.Fatalf("first increment = %v, want 1", first["value"])
	}
	second := callToolJSON(t, h, "increment_counter", map[string]any{"name": "c", "by": float64(5)})
	if second["value"].(float64) != 6 {
		t.Fatalf("second increment = %v, want 6", second["value"])
	}
}

func TestPresence(t *testing.T) {
	h, _ := newTestHandler(t)
	reg := callToolJSON(t, h, "register_agent", map[string]any{"agent_id": "A", "ttl_seconds": float64(60)})
	if reg["registered"] != true || reg["expires_in_seconds"].(float64) != 60 {
		t.Fatalf("register_agent: %v", reg)
	}
	list := callToolJSON(t, h, "list_agents", map[string]any{})
	if len(list["agents"].([]any)) != 1 {
		t.Fatalf("list_agents: %v", list)
	}
	un := callToolJSON(t, h, "unregister_agent", map[string]any{"agent_id": "A"})
	if un["unregistered"] != true {
		t.Fatalf("unregister_agent: %v", un)
	}
	list2 := callToolJSON(t, h, "list_agents", map[string]any{})
	if len(list2["agents"].([]any)) != 0 {
		t.Fatalf("list_agents after unregister: %v", list2)
	}
}

func TestEvents(t *testing.T) {
	h, _ := newTestHandler(t)

	sig := callToolJSON(t, h, "signal_event", map[string]any{"name": "e"})
	if sig["generation"].(float64) != 1 {
		t.Fatalf("signal_event gen = %v, want 1", sig["generation"])
	}

	// Waiting with last_seen=0 fires immediately because current generation (1) > 0.
	wait := callToolJSON(t, h, "wait_for_event", map[string]any{
		"name": "e", "last_seen_generation": float64(0), "wait_seconds": float64(0),
	})
	if wait["fired"] != true || wait["generation"].(float64) != 1 {
		t.Fatalf("wait_for_event should fire immediately: %v", wait)
	}

	// Already caught up: does not fire (non-blocking).
	caught := callToolJSON(t, h, "wait_for_event", map[string]any{
		"name": "e", "last_seen_generation": float64(1), "wait_seconds": float64(0),
	})
	if caught["fired"] != false {
		t.Fatalf("wait_for_event already caught up should not fire: %v", caught)
	}

	clr := callToolJSON(t, h, "clear_event", map[string]any{"name": "e"})
	if clr["cleared"] != true {
		t.Fatalf("clear_event: %v", clr)
	}
}

func TestTasks(t *testing.T) {
	h, _ := newTestHandler(t)

	push := callToolJSON(t, h, "push_task", map[string]any{"queue": "q", "payload": "job1", "author": "me"})
	id := push["id"].(float64)
	if id <= 0 {
		t.Fatalf("push_task id = %v", id)
	}

	claim := callToolJSON(t, h, "claim_next_task", map[string]any{"queue": "q", "agent_id": "A"})
	if claim["claimed"] != true {
		t.Fatalf("claim_next_task: %v", claim)
	}
	if claim["payload"] != "job1" {
		t.Fatalf("claimed payload = %v", claim["payload"])
	}
	leaseToken := claim["lease_token"].(string)

	// list_tasks shows the claimed task with its lease agent.
	list := callToolJSON(t, h, "list_tasks", map[string]any{"queue": "q"})
	tasks := list["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("list_tasks: %v", tasks)
	}
	tm := tasks[0].(map[string]any)
	if tm["state"] != "claimed" || tm["lease_agent"] != "A" {
		t.Fatalf("list_tasks claimed entry: %v", tm)
	}

	done := callToolJSON(t, h, "complete_task", map[string]any{"id": id, "lease_token": leaseToken})
	if done["completed"] != true {
		t.Fatalf("complete_task: %v", done)
	}

	// Empty queue claim returns claimed:false.
	empty := callToolJSON(t, h, "claim_next_task", map[string]any{"queue": "q", "agent_id": "A"})
	if empty["claimed"] != false {
		t.Fatalf("claim from drained queue: %v", empty)
	}
}

func TestFailTaskRequeue(t *testing.T) {
	h, _ := newTestHandler(t)
	callToolJSON(t, h, "push_task", map[string]any{"queue": "q", "payload": "j"})
	claim := callToolJSON(t, h, "claim_next_task", map[string]any{"queue": "q", "agent_id": "A"})
	lt := claim["lease_token"].(string)
	id := claim["id"].(float64)

	failed := callToolJSON(t, h, "fail_task", map[string]any{"id": id, "lease_token": lt, "requeue": true})
	if failed["failed"] != true {
		t.Fatalf("fail_task: %v", failed)
	}
	// Requeued task is claimable again.
	again := callToolJSON(t, h, "claim_next_task", map[string]any{"queue": "q", "agent_id": "B"})
	if again["claimed"] != true {
		t.Fatalf("requeued task should be claimable: %v", again)
	}
}

// TestTTLCompat verifies ttl_minutes is honoured and ttl_seconds takes precedence.
func TestTTLCompat(t *testing.T) {
	h, _ := newTestHandler(t)

	// ttl_minutes alone -> *60.
	got := callToolJSON(t, h, "lock_resource", map[string]any{"name": "m", "agent_id": "A", "ttl_minutes": float64(2)})
	if got["expires_in_seconds"].(float64) != 120 {
		t.Fatalf("ttl_minutes=2 -> %v, want 120", got["expires_in_seconds"])
	}

	// ttl_seconds wins over ttl_minutes.
	got2 := callToolJSON(t, h, "lock_resource", map[string]any{
		"name": "m2", "agent_id": "A", "ttl_minutes": float64(2), "ttl_seconds": float64(30),
	})
	if got2["expires_in_seconds"].(float64) != 30 {
		t.Fatalf("ttl_seconds should win -> %v, want 30", got2["expires_in_seconds"])
	}
}
