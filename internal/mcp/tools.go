package mcp

import (
	"context"
	"encoding/json"

	"github.com/adamorad/airlock/internal/store"
)

// defaultLockTTLSeconds is the lock TTL applied when a caller supplies neither
// ttl_seconds nor ttl_minutes (15 minutes — long enough to cover a typical
// guarded operation, short enough that a crashed holder frees the lock).
const defaultLockTTLSeconds = 900

// defaultPresenceTTLSeconds is the heartbeat TTL applied to register_agent when
// ttl_seconds is omitted.
const defaultPresenceTTLSeconds = 60

// defaultEventWaitSeconds is the blocking window applied to wait_for_event when
// wait_seconds is omitted.
const defaultEventWaitSeconds = 25

// defaultTaskLeaseSeconds is the lease applied to claim_next_task when
// lease_seconds is omitted.
const defaultTaskLeaseSeconds = 120

// toolHandler dispatches tool calls to the store managers. It is constructed by
// NewToolHandler and implements the dispatch half of the MCP Handler; the
// JSON-RPC method layer lives in handler.go.
type toolHandler struct {
	locks    *store.LockManager
	presence *store.PresenceManager
	events   *store.EventManager
	tasks    *store.TaskManager
	store    *store.Store
}

// toolDef is a single MCP tool definition as returned by tools/list. The schema
// is hand-built (not reflected) so descriptions and required-field semantics are
// exactly what agents read.
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// obj is a tiny constructor for an inputSchema with object type, the given
// properties, and the given required field names.
func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// prop builds a single JSON-schema property of the given type with a
// description.
func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

// toolDefs is the immutable, ordered list of every tool the handler exposes.
// tools/list returns these verbatim; toolCall dispatches by Name. Keep this in
// sync with the dispatch switch in callTool.
func toolDefs() []toolDef {
	return []toolDef{
		// --- LOCKS ---
		{
			Name: "lock_resource",
			Description: "Acquire an exclusive lock on a named resource so other agents do not touch it concurrently. " +
				"Pass wait_seconds (up to 50) to BLOCK until the lock is acquired — this is the coordination-by-default path; " +
				"with the default wait_seconds=0 a contended lock returns locked=false immediately. " +
				"Use a descriptive name like 'file:/path/to/file', 'npm-install', or 'agent:some-id'. " +
				"On success you get a lock_token; pass it to renew_lock/unlock_resource. " +
				"If blocking times out you get a wake_token + queue_position — call again with that wake_token to keep your place in line.",
			InputSchema: obj(map[string]any{
				"name":         prop("string", "Resource name to lock, e.g. 'file:/path', 'npm-install', 'agent:id'."),
				"agent_id":     prop("string", "Stable identifier of the calling agent."),
				"ttl_seconds":  prop("integer", "Lock lifetime in seconds (default 900). The lock auto-expires after this if not renewed."),
				"ttl_minutes":  prop("integer", "Deprecated alias for ttl_seconds expressed in minutes; ttl_seconds wins if both are given."),
				"wait_seconds": prop("integer", "Block up to this many seconds (0-50) waiting to acquire. 0 = return immediately if held."),
				"wake_token":   prop("string", "Re-poll token from a prior timed-out call; pass it to keep your FIFO place in line."),
			}, "name", "agent_id"),
		},
		{
			Name:        "unlock_resource",
			Description: "Release a lock you hold. Prefer passing the lock_token returned by lock_resource; agent_id is accepted for compatibility. No-op if not held or not yours.",
			InputSchema: obj(map[string]any{
				"name":       prop("string", "Resource name to unlock."),
				"agent_id":   prop("string", "Calling agent id (used if lock_token is omitted)."),
				"lock_token": prop("string", "The lock_token returned by lock_resource (preferred)."),
			}, "name"),
		},
		{
			Name:        "renew_lock",
			Description: "Extend a lock you hold before it expires. Prefer passing lock_token; agent_id is accepted for compatibility (renews only if the live holder matches).",
			InputSchema: obj(map[string]any{
				"name":        prop("string", "Resource name to renew."),
				"lock_token":  prop("string", "The lock_token returned by lock_resource (preferred)."),
				"agent_id":    prop("string", "Calling agent id (used if lock_token is omitted)."),
				"ttl_seconds": prop("integer", "New lifetime in seconds from now (default 900)."),
				"ttl_minutes": prop("integer", "Deprecated alias for ttl_seconds in minutes."),
			}, "name"),
		},
		{
			Name:        "list_locks",
			Description: "List all currently held locks with their holder agent and remaining seconds.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "lock_resources",
			Description: "Atomically acquire several locks at once (all-or-nothing). If any name is held, none are acquired. Returns a token per name on success.",
			InputSchema: obj(map[string]any{
				"names":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Resource names to lock together."},
				"agent_id":    prop("string", "Calling agent id."),
				"ttl_seconds": prop("integer", "Lock lifetime in seconds (default 900)."),
				"ttl_minutes": prop("integer", "Deprecated alias for ttl_seconds in minutes."),
			}, "names", "agent_id"),
		},

		// --- NOTES / STATE ---
		{
			Name:        "set_note",
			Description: "Store a shared key/value note visible to all agents. Optionally set an expiry via ttl_seconds; default is no expiry.",
			InputSchema: obj(map[string]any{
				"key":         prop("string", "Note key."),
				"value":       prop("string", "Note value."),
				"author":      prop("string", "Optional provenance — who wrote it."),
				"ttl_seconds": prop("integer", "Optional expiry in seconds; omit for no expiry."),
				"ttl_minutes": prop("integer", "Deprecated alias for ttl_seconds in minutes."),
			}, "key", "value"),
		},
		{
			Name:        "get_note",
			Description: "Read a shared note by key. Returns found=false if absent or expired.",
			InputSchema: obj(map[string]any{
				"key": prop("string", "Note key to read."),
			}, "key"),
		},
		{
			Name:        "list_notes",
			Description: "List all non-expired shared notes.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "delete_note",
			Description: "Delete a shared note by key. Returns deleted=false if it did not exist.",
			InputSchema: obj(map[string]any{
				"key": prop("string", "Note key to delete."),
			}, "key"),
		},
		{
			Name:        "set_note_if",
			Description: "Atomic compare-and-swap on a note: only writes new_value if the current value equals expected_value (an absent/expired note counts as \"\"). Use for lock-free coordination on shared state.",
			InputSchema: obj(map[string]any{
				"key":            prop("string", "Note key."),
				"expected_value": prop("string", "Value the note must currently have for the swap to happen (use \"\" for create-if-absent)."),
				"new_value":      prop("string", "Value to write if expected_value matches."),
				"author":         prop("string", "Optional provenance."),
				"ttl_seconds":    prop("integer", "Optional expiry in seconds."),
				"ttl_minutes":    prop("integer", "Deprecated alias for ttl_seconds in minutes."),
			}, "key", "expected_value", "new_value"),
		},
		{
			Name:        "increment_counter",
			Description: "Atomically add to a named shared counter (creating it if absent) and return the new value.",
			InputSchema: obj(map[string]any{
				"name": prop("string", "Counter name."),
				"by":   prop("integer", "Amount to add (default 1; may be negative)."),
			}, "name"),
		},

		// --- PRESENCE ---
		{
			Name:        "register_agent",
			Description: "Register/heartbeat the calling agent with a TTL so others can see it is alive. Call periodically to stay registered; when it lapses the agent's locks are auto-released.",
			InputSchema: obj(map[string]any{
				"agent_id":    prop("string", "Stable agent id to register."),
				"ttl_seconds": prop("integer", "Heartbeat lifetime in seconds (default 60)."),
			}, "agent_id"),
		},
		{
			Name:        "unregister_agent",
			Description: "Gracefully unregister the calling agent, releasing all locks it holds.",
			InputSchema: obj(map[string]any{
				"agent_id": prop("string", "Agent id to unregister."),
			}, "agent_id"),
		},
		{
			Name:        "list_agents",
			Description: "List all currently registered (non-expired) agents with remaining seconds.",
			InputSchema: obj(map[string]any{}),
		},

		// --- EVENTS ---
		{
			Name:        "signal_event",
			Description: "Fire a named event, bumping its generation counter and waking all waiters. Returns the new generation.",
			InputSchema: obj(map[string]any{
				"name": prop("string", "Event name to signal."),
			}, "name"),
		},
		{
			Name:        "wait_for_event",
			Description: "Block until a named event's generation advances beyond last_seen_generation, up to wait_seconds (0-50, default 25). Pass back the generation you last saw to avoid missing signals.",
			InputSchema: obj(map[string]any{
				"name":                 prop("string", "Event name to wait on."),
				"last_seen_generation": prop("integer", "Generation you last observed (default 0); returns immediately if the current generation is higher."),
				"wait_seconds":         prop("integer", "Max seconds to block (0-50, default 25)."),
			}, "name"),
		},
		{
			Name:        "clear_event",
			Description: "Reset a named event's generation to absent (0).",
			InputSchema: obj(map[string]any{
				"name": prop("string", "Event name to clear."),
			}, "name"),
		},

		// --- TASKS ---
		{
			Name:        "push_task",
			Description: "Enqueue a task onto a named work queue. Higher priority and older tasks are claimed first. Returns the task id.",
			InputSchema: obj(map[string]any{
				"queue":    prop("string", "Queue name."),
				"payload":  prop("string", "Opaque task payload (e.g. JSON)."),
				"author":   prop("string", "Optional provenance."),
				"priority": prop("integer", "Priority; higher is claimed first (default 0)."),
			}, "queue", "payload"),
		},
		{
			Name:        "claim_next_task",
			Description: "Atomically lease the next pending task from a queue. The lease auto-requeues if you don't complete/renew within lease_seconds or your agent goes away. Returns a lease_token for complete_task/fail_task.",
			InputSchema: obj(map[string]any{
				"queue":         prop("string", "Queue to claim from."),
				"agent_id":      prop("string", "Claiming agent id."),
				"lease_seconds": prop("integer", "Lease duration in seconds (default 120)."),
			}, "queue", "agent_id"),
		},
		{
			Name:        "complete_task",
			Description: "Mark a leased task done. Requires the lease_token from claim_next_task.",
			InputSchema: obj(map[string]any{
				"id":          prop("integer", "Task id to complete."),
				"lease_token": prop("string", "Lease token from claim_next_task."),
			}, "id", "lease_token"),
		},
		{
			Name:        "fail_task",
			Description: "Release a leased task. By default (requeue=true) it returns to pending for another consumer; requeue=false gives up and marks it done.",
			InputSchema: obj(map[string]any{
				"id":          prop("integer", "Task id to fail."),
				"lease_token": prop("string", "Lease token from claim_next_task."),
				"requeue":     prop("boolean", "Return to pending (true, default) or give up (false)."),
			}, "id", "lease_token"),
		},
		{
			Name:        "list_tasks",
			Description: "List all tasks in a queue with their state, priority, and (for claimed tasks) the holding agent and remaining lease.",
			InputSchema: obj(map[string]any{
				"queue": prop("string", "Queue name to list."),
			}, "queue"),
		},
	}
}

// callTool dispatches a single tool invocation to the appropriate store call
// and returns the v1-compatible result. Most tools return a result map; the
// list_* tools return a bare JSON array (a []map[string]any) to match the v1
// wire shape. An unknown tool name returns an {"error": ...} result map (NOT a
// JSON-RPC error), matching v1.
func (h *toolHandler) callTool(ctx context.Context, name string, args map[string]any) any {
	switch name {
	// --- LOCKS ---
	case "lock_resource":
		return h.lockResource(ctx, args)
	case "unlock_resource":
		return h.unlockResource(args)
	case "renew_lock":
		return h.renewLock(args)
	case "list_locks":
		return h.listLocks()
	case "lock_resources":
		return h.lockResources(ctx, args)

	// --- NOTES / STATE ---
	case "set_note":
		return h.setNote(args)
	case "get_note":
		return h.getNote(args)
	case "list_notes":
		return h.listNotes()
	case "delete_note":
		return h.deleteNote(args)
	case "set_note_if":
		return h.setNoteIf(args)
	case "increment_counter":
		return h.incrementCounter(args)

	// --- PRESENCE ---
	case "register_agent":
		return h.registerAgent(args)
	case "unregister_agent":
		return h.unregisterAgent(args)
	case "list_agents":
		return h.listAgents()

	// --- EVENTS ---
	case "signal_event":
		return h.signalEvent(args)
	case "wait_for_event":
		return h.waitForEvent(ctx, args)
	case "clear_event":
		return h.clearEvent(args)

	// --- TASKS ---
	case "push_task":
		return h.pushTask(args)
	case "claim_next_task":
		return h.claimNextTask(args)
	case "complete_task":
		return h.completeTask(args)
	case "fail_task":
		return h.failTask(args)
	case "list_tasks":
		return h.listTasks(args)

	default:
		return map[string]any{"error": "unknown tool: " + name}
	}
}

// --- LOCKS ---

func (h *toolHandler) lockResource(ctx context.Context, args map[string]any) map[string]any {
	name := argStr(args, "name")
	if name == "" {
		return errMissing("name")
	}
	agentID := argStr(args, "agent_id")
	if agentID == "" {
		return errMissing("agent_id")
	}
	ttl := ttlSeconds(args, defaultLockTTLSeconds)
	wait := argInt(args, "wait_seconds", 0)
	wakeToken := argStr(args, "wake_token")

	res, err := h.locks.Lock(ctx, name, agentID, ttl, wait, wakeToken)
	if err != nil {
		return errResult(err)
	}
	if res.Locked {
		return map[string]any{
			"locked":             true,
			"lock_token":         res.LockToken,
			"expires_in_seconds": res.ExpiresInSeconds,
		}
	}
	out := map[string]any{
		"locked":             false,
		"held_by":            res.HeldBy,
		"expires_in_seconds": res.ExpiresInSeconds,
	}
	if res.QueuePosition >= 1 {
		out["wake_token"] = res.WakeToken
		out["queue_position"] = res.QueuePosition
		out["retry_with"] = res.RetryWith
	}
	return out
}

func (h *toolHandler) unlockResource(args map[string]any) map[string]any {
	name := argStr(args, "name")
	if name == "" {
		return errMissing("name")
	}
	released, err := h.locks.Unlock(name, argStr(args, "lock_token"), argStr(args, "agent_id"))
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"released": released}
}

func (h *toolHandler) renewLock(args map[string]any) map[string]any {
	name := argStr(args, "name")
	if name == "" {
		return errMissing("name")
	}
	ttl := ttlSeconds(args, defaultLockTTLSeconds)
	lockToken := argStr(args, "lock_token")

	var (
		res store.LockResult
		err error
	)
	if lockToken != "" {
		res, err = h.locks.Renew(name, lockToken, ttl)
	} else {
		agentID := argStr(args, "agent_id")
		if agentID == "" {
			return errMissing("lock_token or agent_id")
		}
		res, err = h.locks.RenewByAgent(name, agentID, ttl)
	}
	if err != nil {
		return map[string]any{"renewed": false, "error": err.Error()}
	}
	return map[string]any{"renewed": true, "expires_in_seconds": res.ExpiresInSeconds}
}

// listLocks returns a bare JSON array of held locks ([{name, agent_id,
// expires_in_seconds}, ...]) to match the v1 wire shape. On error it returns an
// {"error": ...} map instead (both marshal cleanly into content[0].text).
func (h *toolHandler) listLocks() any {
	locks, err := h.locks.ListLocks()
	if err != nil {
		return errResult(err)
	}
	out := make([]map[string]any, 0, len(locks))
	for _, l := range locks {
		out = append(out, map[string]any{
			"name":               l.Name,
			"agent_id":           l.AgentID,
			"expires_in_seconds": l.ExpiresInSeconds,
		})
	}
	return out
}

func (h *toolHandler) lockResources(ctx context.Context, args map[string]any) map[string]any {
	names := argStrSlice(args, "names")
	if len(names) == 0 {
		return errMissing("names")
	}
	agentID := argStr(args, "agent_id")
	if agentID == "" {
		return errMissing("agent_id")
	}
	ttl := ttlSeconds(args, defaultLockTTLSeconds)

	acquired, tokens, heldBy, err := h.locks.LockMany(ctx, names, agentID, ttl, 0)
	if err != nil {
		return errResult(err)
	}
	if !acquired {
		return map[string]any{"locked": false, "held_by": heldBy}
	}
	tok := make(map[string]any, len(tokens))
	for k, v := range tokens {
		tok[k] = v
	}
	return map[string]any{"locked": true, "tokens": tok}
}

// --- NOTES / STATE ---

func (h *toolHandler) setNote(args map[string]any) map[string]any {
	key := argStr(args, "key")
	if key == "" {
		return errMissing("key")
	}
	if _, ok := args["value"]; !ok {
		return errMissing("value")
	}
	ttl := ttlSeconds(args, 0)
	if err := h.store.SetNote(key, argStr(args, "value"), argStr(args, "author"), ttl); err != nil {
		return errResult(err)
	}
	return map[string]any{"saved": true}
}

func (h *toolHandler) getNote(args map[string]any) map[string]any {
	key := argStr(args, "key")
	if key == "" {
		return errMissing("key")
	}
	note, found, err := h.store.GetNote(key)
	if err != nil {
		return errResult(err)
	}
	if !found {
		return map[string]any{"found": false}
	}
	return noteMap(note)
}

// listNotes returns a bare JSON array of active notes ([{key, value, author?,
// expires_in_seconds?}, ...]) to match the v1 wire shape; author/expiry are
// omitted when absent (see noteMap). On error it returns an {"error": ...} map.
func (h *toolHandler) listNotes() any {
	notes, err := h.store.ListNotes()
	if err != nil {
		return errResult(err)
	}
	out := make([]map[string]any, 0, len(notes))
	for _, n := range notes {
		out = append(out, noteMap(n))
	}
	return out
}

func (h *toolHandler) deleteNote(args map[string]any) map[string]any {
	key := argStr(args, "key")
	if key == "" {
		return errMissing("key")
	}
	deleted, err := h.store.DeleteNote(key)
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"deleted": deleted}
}

func (h *toolHandler) setNoteIf(args map[string]any) map[string]any {
	key := argStr(args, "key")
	if key == "" {
		return errMissing("key")
	}
	if _, ok := args["expected_value"]; !ok {
		return errMissing("expected_value")
	}
	if _, ok := args["new_value"]; !ok {
		return errMissing("new_value")
	}
	ttl := ttlSeconds(args, 0)
	swapped, err := h.store.SetNoteIf(
		key, argStr(args, "expected_value"), argStr(args, "new_value"), argStr(args, "author"), ttl,
	)
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"swapped": swapped}
}

func (h *toolHandler) incrementCounter(args map[string]any) map[string]any {
	name := argStr(args, "name")
	if name == "" {
		return errMissing("name")
	}
	by := int64(argInt(args, "by", 1))
	value, err := h.store.IncrementCounter(name, by)
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"value": value}
}

// --- PRESENCE ---

func (h *toolHandler) registerAgent(args map[string]any) map[string]any {
	agentID := argStr(args, "agent_id")
	if agentID == "" {
		return errMissing("agent_id")
	}
	ttl := argInt(args, "ttl_seconds", defaultPresenceTTLSeconds)
	if err := h.presence.Register(agentID, ttl); err != nil {
		return errResult(err)
	}
	return map[string]any{"registered": true, "expires_in_seconds": ttl}
}

func (h *toolHandler) unregisterAgent(args map[string]any) map[string]any {
	agentID := argStr(args, "agent_id")
	if agentID == "" {
		return errMissing("agent_id")
	}
	if err := h.presence.Unregister(agentID); err != nil {
		return errResult(err)
	}
	return map[string]any{"unregistered": true}
}

// listAgents returns a bare JSON array of registered agents ([{agent_id,
// expires_in_seconds}, ...]), matching the list-tool array convention. On error
// it returns an {"error": ...} map.
func (h *toolHandler) listAgents() any {
	agents, err := h.presence.ListAgents()
	if err != nil {
		return errResult(err)
	}
	out := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		out = append(out, map[string]any{
			"agent_id":           a.AgentID,
			"expires_in_seconds": a.ExpiresInSeconds,
		})
	}
	return out
}

// --- EVENTS ---

func (h *toolHandler) signalEvent(args map[string]any) map[string]any {
	name := argStr(args, "name")
	if name == "" {
		return errMissing("name")
	}
	gen, err := h.events.Signal(name)
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"generation": gen}
}

func (h *toolHandler) waitForEvent(ctx context.Context, args map[string]any) map[string]any {
	name := argStr(args, "name")
	if name == "" {
		return errMissing("name")
	}
	lastSeen := int64(argInt(args, "last_seen_generation", 0))
	wait := argInt(args, "wait_seconds", defaultEventWaitSeconds)
	gen, fired, err := h.events.Wait(ctx, name, lastSeen, wait)
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"generation": gen, "fired": fired}
}

func (h *toolHandler) clearEvent(args map[string]any) map[string]any {
	name := argStr(args, "name")
	if name == "" {
		return errMissing("name")
	}
	if err := h.events.Clear(name); err != nil {
		return errResult(err)
	}
	return map[string]any{"cleared": true}
}

// --- TASKS ---

func (h *toolHandler) pushTask(args map[string]any) map[string]any {
	queue := argStr(args, "queue")
	if queue == "" {
		return errMissing("queue")
	}
	if _, ok := args["payload"]; !ok {
		return errMissing("payload")
	}
	priority := argInt(args, "priority", 0)
	id, err := h.tasks.Push(queue, argStr(args, "payload"), argStr(args, "author"), priority)
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"id": id}
}

func (h *toolHandler) claimNextTask(args map[string]any) map[string]any {
	queue := argStr(args, "queue")
	if queue == "" {
		return errMissing("queue")
	}
	agentID := argStr(args, "agent_id")
	if agentID == "" {
		return errMissing("agent_id")
	}
	lease := argInt(args, "lease_seconds", defaultTaskLeaseSeconds)
	task, token, claimed, err := h.tasks.ClaimNext(queue, agentID, lease)
	if err != nil {
		return errResult(err)
	}
	if !claimed {
		return map[string]any{"claimed": false}
	}
	return map[string]any{
		"claimed":     true,
		"id":          task.ID,
		"payload":     task.Payload,
		"lease_token": token,
	}
}

func (h *toolHandler) completeTask(args map[string]any) map[string]any {
	id, ok := argID(args)
	if !ok {
		return errMissing("id")
	}
	token := argStr(args, "lease_token")
	if token == "" {
		return errMissing("lease_token")
	}
	completed, err := h.tasks.Complete(id, token)
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"completed": completed}
}

func (h *toolHandler) failTask(args map[string]any) map[string]any {
	id, ok := argID(args)
	if !ok {
		return errMissing("id")
	}
	token := argStr(args, "lease_token")
	if token == "" {
		return errMissing("lease_token")
	}
	requeue := argBool(args, "requeue", true)
	failed, err := h.tasks.Fail(id, token, requeue)
	if err != nil {
		return errResult(err)
	}
	return map[string]any{"failed": failed}
}

// listTasks returns a bare JSON array of tasks in a queue, matching the
// list-tool array convention. A missing queue arg or store error returns an
// {"error": ...} map instead.
func (h *toolHandler) listTasks(args map[string]any) any {
	queue := argStr(args, "queue")
	if queue == "" {
		return errMissing("queue")
	}
	tasks, err := h.tasks.List(queue)
	if err != nil {
		return errResult(err)
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		m := map[string]any{
			"id":       t.ID,
			"queue":    t.Queue,
			"payload":  t.Payload,
			"priority": t.Priority,
			"state":    t.State,
		}
		if t.Author != "" {
			m["author"] = t.Author
		}
		if t.State == "claimed" {
			m["lease_agent"] = t.LeaseAgent
			m["lease_expires_in_seconds"] = t.LeaseExpiresInSeconds
		}
		out = append(out, m)
	}
	return out
}

// --- shared result helpers ---

// errMissing builds the standard {"error":"missing <field>"} tool result.
func errMissing(field string) map[string]any {
	return map[string]any{"error": "missing " + field}
}

// errResult wraps a store error as a tool-result map. Store errors are
// operational (I/O, invalid ttl); they ride in the result so the agent sees a
// structured message rather than a JSON-RPC fault.
func errResult(err error) map[string]any {
	return map[string]any{"error": err.Error()}
}

// noteMap renders a store.Note as a result map, omitting author/expiry when
// absent (matching v1: optional fields are only present when set).
func noteMap(n store.Note) map[string]any {
	m := map[string]any{"key": n.Key, "value": n.Value}
	if n.Author != "" {
		m["author"] = n.Author
	}
	if n.HasExpiry {
		m["expires_in_seconds"] = n.ExpiresInSeconds
	}
	return m
}

// --- argument parsing helpers ---

// argStr returns args[key] as a string, or "" if absent or not a string.
func argStr(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// argInt returns args[key] coerced to int, accepting float64 (the JSON default),
// int, and json.Number. Falls back to def when absent or uncoercible.
func argInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return def
}

// argBool returns args[key] as a bool, or def when absent or not a bool.
func argBool(args map[string]any, key string, def bool) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// argStrSlice returns args[key] as a []string. It accepts a JSON array of
// strings (the common case) and skips non-string elements.
func argStrSlice(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// argID extracts a task id (int64) from args["id"], accepting the numeric forms
// JSON may carry. The bool is false when absent.
func argID(args map[string]any) (int64, bool) {
	v, ok := args["id"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// ttlSeconds resolves a TTL in seconds with v1/v2 compatibility: ttl_seconds
// wins if present; else ttl_minutes (×60) if present; else def. A present-but-
// zero ttl_seconds is honoured (returns 0), letting callers explicitly request
// "no expiry" semantics where the store supports it.
func ttlSeconds(args map[string]any, def int) int {
	if _, ok := args["ttl_seconds"]; ok {
		return argInt(args, "ttl_seconds", def)
	}
	if _, ok := args["ttl_minutes"]; ok {
		return argInt(args, "ttl_minutes", 0) * 60
	}
	return def
}
