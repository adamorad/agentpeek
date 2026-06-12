# Airlock v2 — The Coordination Layer for AI Agents

> **Status (2026-06-12): v2.0.0 shipped.** The core (locks with blocking waits, atomic state, presence, events) and the task queue are implemented in Go and cross-platform. This document is the original design spec.

> Status: public roadmap. v2 is design-only right now — this document is the roadmap and the design rationale. v1 (this repo) is shipped and supported; v2 is the next-generation design described below.
> Last updated: 2026-06-12.

---

## §1 Vision

**Run five agents on one repo without them stomping on each other.**

Airlock is the always-on coordination daemon that outlives every session — the missing syscall layer (mutexes, signals, shared state) for AI agents. Operating systems gave processes mutexes, semaphores, signals, and shared memory so that concurrent work could be correct. AI agents have none of that: each session is an island that spins up, acts on a shared filesystem, and dies. Airlock is the small, boring, always-running piece that gives a fleet of agents the primitives an OS gives a fleet of processes.

Picture the north star concretely. Two Claude Code sessions, one Cursor agent, and one CI script are all working on the same repository at the same time. All four need to run database migrations, but migrations must not run concurrently — so each one calls `lock_resource("migrations")` and the daemon serializes them: the first holder runs, the rest block on lock waits and wake the instant it releases, no busy-polling and no agent-authored retry loops. Each agent that needs a scratch file calls `increment_counter("scratch-seq")` and gets a unique number back, so filenames never collide even under a dead heat. When the first agent finishes the schema work it calls `signal_event("migrations-done")`; Agent B, which has been parked in `wait_for_event("migrations-done")`, wakes the instant the signal fires and begins the dependent work — not a second of polling latency. And the developer, watching all of it happen live, runs `airlock status` in a terminal and sees exactly which agent holds which lock, who is queued behind it, and what each agent last signaled. Coordination that used to live in fragile prompt instructions ("please wait until migrations are done") becomes a real, observable, race-free system primitive.

---

## §2 Goals

| ID | Goal | Definition of done |
|----|------|--------------------|
| **G1** | Coordination-by-default | A contended lock is acquired via a wake-token re-poll with **zero agent-authored retry logic** — the tool result itself carries the `retry_with` instruction the agent follows verbatim. |
| **G2** | Correct under concurrency | 100 concurrent `increment_counter` calls produce 100 unique values; `set_note_if` never lies (a compare-and-swap that reports success only when it actually swapped). |
| **G3** | Cross-platform | A single smoke script passes unchanged on macOS arm64 and Ubuntu x86_64. |
| **G4** | Zero-dep install | A fresh machine reaches a working daemon in ≤2 commands, with no language runtime to install. |
| **G5** | Backward compatible | Every v1 call works unchanged on v2 — same port (27183), same JSON request and response shapes. |
| **G6** | Visible | "What are my agents doing?" is answered in ≤2s via `airlock status`. |
| **G7** | Secure by construction | `release`/`renew` of a lock is **impossible** without the capability token returned by `acquire`; provenance is not authorization. |

---

## §3 Landscape

How Airlock v2 compares to adjacent tools, **as of June 2026**. Columns: locks · blocking waits · task queue · atomic state · always-on daemon · zero deps · cross-platform · auth.

| Tool | locks | blocking waits | task queue | atomic state | always-on daemon | zero deps | cross-platform | auth |
|------|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|
| [agent-orchestration](https://github.com/madebyaris/agent-orchestration) — MCP locks/queue/memory, Node, session-scoped | ✅ | ❌ | ✅ | partial | ❌ | ❌ (Node) | ✅ | ❌ |
| [Swarm Tools](https://swarmtools.ai) — file reservations + agent mail, coupled to its orchestrator | ✅ | ❌ | partial | ❌ | ❌ | ❌ | ❓ | ❌ |
| [ComposioHQ agent-orchestrator](https://github.com/ComposioHQ/agent-orchestrator) — git-worktree isolation (different problem) | n/a | n/a | n/a | n/a | ❌ | ❌ | ✅ | ❌ |
| [A2A protocol v1.0](https://a2a-protocol.org) (April 2026) — messaging standard, not infrastructure | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a |
| [mem0](https://github.com/mem0ai/mem0) / [engram](https://github.com/) / [mcp-server-memory](https://github.com/) — memory ≠ real-time coordination | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ |
| Redis + scripts — capable, but no MCP surface, real setup burden | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ |
| `flock` / lockfiles — no TTL, crash-deadlocks, invisible to agents | ✅ | partial | ❌ | ❌ | ❌ | ✅ | partial | ❌ |
| **Airlock v2** | ✅ | ✅ | ✅ (v2.1) | ✅ | ✅ | ✅ | ✅ | ✅ |

A2A is a messaging standard for how agents *talk to each other*; it is not coordination infrastructure, and Airlock sits **below** it — A2A is the conversation, Airlock is the shared arbiter the conversing agents both lean on.

**The unoccupied quadrant.** No existing tool combines *always-on* + *blocking waits* + *zero-dep* + *MCP-native* + *token-secured* coordination. Redis has the semantics but no MCP surface and a real install. The MCP-native tools are session-scoped — they die with the session that spawned them, which is exactly wrong for a coordination layer whose entire job is to outlive any single agent and hold the shared state every agent depends on. Airlock v2 takes that quadrant.

---

## §4 What v1 got wrong

Airlock v1 shipped and proved the concept. Six things it got wrong, each of which drives a requirement in §5:

1. **Polling-only locks.** A contended `lock_resource` returns `{locked:false, held_by}` immediately and the agent has to author its own retry loop in the prompt. That is fragile, latency-heavy, and pushes coordination logic into every agent. → **R1**.
2. **UserDefaults is non-transactional.** State lives in macOS `UserDefaults`, which has no transactions and no atomic read-modify-write. Concurrent writers can lose updates. → **R2, R3**.
3. **No compare-and-swap / no atomic counters.** There is no way to do "set this only if it still equals X" or "give me the next unique number." Our own website's marketing example — a migration counter incremented via read-then-write — is itself **racy**. → **R3**.
4. **macOS-only.** Swift + the Apple `Network` framework + a LaunchAgent means Linux and CI machines are excluded, which is where a lot of agent fleets actually run. → **R4**.
5. **No visibility.** There is no way to ask "what are my agents doing?" — no `status`, no `watch`, no dashboard. The coordination is invisible. → **R8**.
6. **Trusts self-declared `agent_id`.** Any caller can pass any `agent_id`, so a prompt-injected or buggy agent can call `unlock_resource` with someone *else's* `agent_id` and steal or drop their lock. Identity is not authorization. → **R6, R9**.

---

## §5 Requirements

### R1 — Wait mechanics (→ G1)

`lock_resource` (and `wait_for_event`, `lock_resources`) take an optional **`wait_seconds`**, default **25**, hard cap **50**.

The cap is empirical, not arbitrary. Claude Code's default per-tool-call timeout for HTTP MCP servers is **60 seconds**: a server-side block of 55s completes and returns; a 120s block is aborted by the client at ~60s with `MCP server "<name>" tool "<tool>" timed out after Ns`. `MCP_TOOL_TIMEOUT` moves that threshold both directions. So `wait_seconds` defaults to 25 and caps at **50** — comfortably under the 60s default. Users who have raised `MCP_TOOL_TIMEOUT` may pass higher, but **the server must never rely on the client's timeout being above default**; 50 is the contract.

Behavior:

- **Acquired within the window** → `{locked: true, lock_token, expires_in_seconds}`. The `lock_token` is a capability (R6) — required to `unlock`/`renew`.
- **Window expired without acquiring** → `{locked: false, wake_token, queue_position, retry_with: "<exact re-poll instruction>"}`. The `retry_with` string is a literal instruction the agent follows with no logic of its own, e.g. *"Call lock_resource again with wake_token=<...> to keep your place in line and block for the next slot."*
- The **`wake_token` holds the agent's FIFO slot** in the wait queue. Re-polling with the wake_token is **idempotent** — it never double-enqueues the same waiter; it resumes the existing slot.
- **Abandoned `wake_token`s expire after 2× the lock TTL**, so an agent that gives up and never re-polls cannot wedge the queue forever.

### R2 — Durable, transactional store (→ G2)

State lives in **SQLite in WAL mode** at `~/.airlock/state.db`. WAL gives concurrent readers alongside a single writer and crash-safe durability. Replaces UserDefaults entirely.

### R3 — Atomic state operations (→ G2)

- `increment_counter(name, by=1)` → returns the post-increment value; 100 concurrent calls yield 100 distinct values.
- `set_note_if(key, expected_value, new_value)` → compare-and-swap; returns `{swapped: true, value}` only if the stored value equaled `expected_value`, else `{swapped: false, current_value}`.
- All writes run inside a SQLite transaction on the single write connection — no lost updates.

### R4 — Go (→ G3, G4)

The daemon is written in **Go**. The real reasons, in order:

1. **The concurrency model fits the problem exactly.** A parked waiter is a goroutine blocked on a channel with a `context` deadline. Wake = send on the channel. Timeout = context cancellation. Crash-of-holder wake = reaper closes the channel. This is *the* textbook Go shape; in another language it is async ceremony bolted onto a worse fit.
2. **`modernc.org/sqlite` is pure Go (no cgo).** That keeps `GOOS=linux GOARCH=amd64 go build` a trivial cross-compile with no C toolchain, which is what makes G3/G4 cheap.

Explicitly **not** chosen "because an SDK exists" — we hand-roll the HTTP/MCP layer anyway (it is small and we want full control of the streaming and fallback behavior), so SDK availability is not a factor.

| | Go | Rust | Swift |
|---|---|---|---|
| Parked-waiter model | goroutine + channel + `context` — exact fit | async runtime + `tokio::select!` — works, more ceremony | `Task` + `Network` framework — Apple-shaped |
| Pure-Go/no-cgo SQLite | ✅ `modernc.org/sqlite` | ✅ via `rusqlite`/bundled, but C build | ✅ but Apple-leaning ecosystem |
| Cross-compile to Linux | trivial (`GOOS=linux`) | doable, heavier toolchain | weak Linux story |
| Distribution / single static binary | excellent | excellent | Apple-only is the practical reality |
| Net outcome | **chosen** | same outcome, more async ceremony | Apple-only `Network` framework + weak Linux distribution |

### R5 — Presence / heartbeats (→ G6, G1)

- `register_agent(agent_id, ttl_seconds=60)` registers a heartbeat-backed presence lease; the agent re-calls to stay alive.
- When an agent's presence **expires**, the daemon **immediately** releases all locks and leases that agent held and **wakes the waiters** queued behind them. A crashed agent must not cost its successors their full wait window.

### R6 — Capability tokens (→ G7)

- `lock_resource` returns a `lock_token`; `unlock_resource` and `renew_lock` **require** it. No token → no release/renew, full stop.
- `claim_next_task` returns a `task_token`; `complete_task` and `fail_task` **require** it.
- `agent_id` becomes **provenance** (who did this), not **authorization** (who is allowed to). This directly closes the v1 hole where any caller could unlock anyone's lock.

### R7 — Events (→ G1)

Events are **generation-counted**, not latched:

- `signal_event(name)` bumps that event's **generation** counter.
- `wait_for_event(name, since_generation, wait_seconds)` blocks until the generation exceeds `since_generation`, then returns the new generation. Passing the last-seen generation makes wakeups exactly-once and missed-signal-safe (a signal that fired between calls is still observed, because the generation moved).
- `clear_event(name)` resets the event. There is **no permanent latch** — events model "something happened N times," not a one-way boolean that stays true forever.

### R8 — Observability (→ G6)

- `airlock status` — one-shot snapshot: locks (holder, queue depth), notes, counters, present agents, recent events. ≤2s.
- `airlock watch` — live-updating TUI of the same.
- Optional `airlock --dashboard` serving a read-only web view at `/dashboard`.

### R9 — Hardened local security (→ G7)

Keeps the entire v1 posture and adds to it:

- **v1 posture retained:** loopback-only bind (`127.0.0.1`), `Host` header allowlist, reject any `Origin` header, require `application/json` Content-Type on POST.
- **Token file on multi-user systems.** On Linux / multi-user hosts, loopback is shared across *all* users, so loopback alone is not an authorization boundary. A `0600`-permissioned token at `~/.airlock/token` is **required** there; clients must present it.
- **Resource limits** (anti-DoS, anti-wedge): max parked waiters per lock, max (locks + notes) per agent, payload size caps, queue depth caps.
- **Mandatory provenance** on notes and tasks: every note/task records writer `agent_id` + timestamp. Note content is a **cross-agent prompt-injection channel** — a consuming agent must treat note/task content as *data, never instructions*. The daemon records who wrote it so consumers can reason about trust; it does not and cannot sanitize the content's meaning.

### R10 — Tasks (v2.1, → G1)

Deliberately **descoped from the v2.0 core** to stay out of orchestrator territory — locks + events already cover most coordination, and a task queue risks drifting toward "spawns/schedules work," which is a non-goal (§9).

- `push_task` / `claim_next_task` / `complete_task` / `fail_task(task_id, requeue)` / `list_tasks`.
- A claim is a **lease bound to the claimant's presence TTL** (R5). If the claimant's presence expires, the lease expires and the task **auto-requeues** — no task is lost to a crashed worker.

---

## §6 Tool surface

Naming and parameters here are the contract referenced by §7. **24 tools** across five groups (5 locks · 6 notes/state · 3 presence · 3 events · 5 tasks-v2.1), plus a v1-compatibility appendix.

### Locks (5)

| Tool | Params | Returns | Errors |
|------|--------|---------|--------|
| `lock_resource` | `name`, `agent_id`, `ttl_seconds?`=900, `wait_seconds?`=25 (cap 50) | `{locked:true, lock_token, expires_in_seconds}` or `{locked:false, wake_token, queue_position, retry_with}` | `invalid_wake_token`, `limit_exceeded` (max waiters), `payload_too_large` |
| `unlock_resource` | `name`, `lock_token` | `{released:true}` | `invalid_token` (token does not match holder), `not_found` |
| `renew_lock` | `name`, `lock_token`, `ttl_seconds?` | `{renewed:true, expires_in_seconds}` | `invalid_token`, `not_found` (expired) |
| `list_locks` | — | `[{name, agent_id, expires_in_seconds, queue_depth}]` | — |
| `lock_resources` | `names:[string]`, `agent_id`, `ttl_seconds?`, `wait_seconds?` | all-or-nothing: `{locked:true, lock_tokens:{name:token}, expires_in_seconds}` or `{locked:false, wake_token, retry_with}` | `invalid_wake_token`, `limit_exceeded` |

`lock_resources` acquires every named lock atomically (all-or-nothing). To prevent two-lock deadlock, the daemon acquires in a **documented global lock-ordering** (lexicographic by `name`) regardless of the order in the request, so two agents requesting `{A,B}` and `{B,A}` can never each hold one and wait on the other.

### Notes / state (6)

| Tool | Params | Returns | Errors |
|------|--------|---------|--------|
| `set_note` | `key`, `value`, `agent_id`, `ttl_seconds?` | `{saved:true}` | `payload_too_large`, `limit_exceeded` (max notes/agent) |
| `get_note` | `key` | `{key, value, agent_id, written_at, expires_in_seconds?}` or `{found:false}` | — |
| `list_notes` | — | `[{key, value, agent_id, written_at, expires_in_seconds?}]` | — |
| `delete_note` | `key`, `agent_id` | `{deleted:true}` or `{found:false}` | — |
| `set_note_if` | `key`, `expected_value`, `new_value`, `agent_id` | `{swapped:true, value}` or `{swapped:false, current_value}` | `payload_too_large` |
| `increment_counter` | `name`, `by?`=1, `agent_id` | `{value}` (post-increment) | `payload_too_large` |

`delete_note` is **new in v2** — v1 has no delete and notes could only expire via TTL.

### Presence (3)

| Tool | Params | Returns | Errors |
|------|--------|---------|--------|
| `register_agent` | `agent_id`, `ttl_seconds?`=60 | `{registered:true, expires_in_seconds}` | `limit_exceeded` |
| `unregister_agent` | `agent_id` | `{unregistered:true}` — also releases held locks/leases and wakes waiters | — |
| `list_agents` | — | `[{agent_id, expires_in_seconds, locks_held, tasks_claimed}]` | — |

### Events (3)

| Tool | Params | Returns | Errors |
|------|--------|---------|--------|
| `signal_event` | `name`, `agent_id` | `{signaled:true, generation}` | `payload_too_large` |
| `wait_for_event` | `name`, `since_generation?`=0, `wait_seconds?`=25 (cap 50) | `{fired:true, generation}` or `{fired:false, generation, retry_with}` | — |
| `clear_event` | `name`, `agent_id` | `{cleared:true, generation:0}` | — |

### Tasks (5) — **v2.1, not in v2.0 core**

| Tool | Params | Returns | Errors |
|------|--------|---------|--------|
| `push_task` | `queue`, `payload`, `agent_id` | `{pushed:true, task_id}` | `payload_too_large`, `limit_exceeded` (queue depth) |
| `claim_next_task` | `queue`, `agent_id` | `{task_id, payload, task_token}` or `{empty:true}` | — |
| `complete_task` | `task_id`, `task_token` | `{completed:true}` | `invalid_token`, `not_found` |
| `fail_task` | `task_id`, `task_token`, `requeue?`=true | `{failed:true, requeued}` | `invalid_token`, `not_found` |
| `list_tasks` | `queue?` | `[{task_id, queue, state, claimant?, payload}]` | — |

A claim is a lease bound to the claimant's presence TTL (R5/R10); presence expiry auto-requeues the task.

### Appendix: v1 compatibility (transcribed from `Sources/airlock/MCP/MCPTools.swift`)

Every v1 tool keeps working **unchanged** on the same port (27183) with the same JSON. The exact v1 contract, transcribed from source (not memory):

| v1 tool | Required params | Optional params | Response shape |
|---------|-----------------|-----------------|----------------|
| `lock_resource` | `name`, `agent_id` | `ttl_minutes` (default 15) | `{locked:true, expires_in_seconds}` or `{locked:false, held_by, expires_in_seconds}` |
| `unlock_resource` | `name`, `agent_id` | — | `{released:true}` (no-op if held by another agent) |
| `renew_lock` | `name`, `agent_id` | `ttl_minutes` (default 15) | `{renewed:true, expires_in_seconds}`; errors: lock not found/expired, lock not owned by agent |
| `list_locks` | — | — | `[{name, agent_id, expires_in_seconds}]` |
| `set_note` | `key`, `value` | `author`, `ttl_minutes` | `{saved:true}` |
| `get_note` | `key` | — | `{key, value, author?, expires_in_seconds?}` or `{found:false}` |
| `list_notes` | — | — | `[{key, value, author?, expires_in_seconds?}]` |

That is the **complete** v1 surface — **7 tools**. Notes on the v2 mapping:

- v1 `lock_resource` returns no token and takes `ttl_minutes`; v2 `lock_resource` adds `lock_token` + `wait_seconds` and prefers `ttl_seconds`. The v1 request shape (with `agent_id`, `ttl_minutes`, no token) is still accepted and produces the v1 response shape — a v1 caller never sees a token and `unlock`/`renew` keyed on `name`+`agent_id` still works. New callers opt into the token path by reading `lock_token` from the response.
- v1 `set_note` uses `author`; v2 standardizes on `agent_id` for provenance but accepts `author` as an alias on the legacy path.
- v1 has **no** `delete_note`, **no** atomic ops (`set_note_if`/`increment_counter`), **no** presence/events/tasks. Those are all v2-new.

---

## §7 Architecture

A single Go daemon, one process, listening on `127.0.0.1:27183`.

**Transport.** MCP **streamable HTTP** is the primary surface, with a **legacy JSON-RPC POST fallback** so v1 clients keep working. The HTTP/MCP layer is hand-rolled (small, full control of streaming + fallback).

**Waiter parking.** Parked waiters live on **per-resource FIFO channels in memory, OUTSIDE any SQLite transaction.** A blocking `lock_resource` does its atomic "try to take the lock" inside a short transaction, and if it can't, it parks the goroutine on the resource's FIFO channel with a `context` deadline — it does *not* hold a DB transaction open while it waits. This is the core reason a blocking daemon doesn't melt the database under contention.

**Write path.** A **single dedicated write connection** with a `busy_timeout` serializes all writes; readers use the WAL's concurrent read path. SQLite's single-writer model is a feature here — it is the lock that makes `increment_counter` and `set_note_if` correct for free.

**Background reaper.** A background goroutine sweeps for expired lock TTLs and expired presence leases. On either, it **wakes the FIFO head** for the affected resource(s). Crucially, **lock release is not the only wake source**: a crashed holder whose TTL lapses, or whose presence lease expires, triggers a reaper wake — so waiters behind a dead agent do not eat their full `wait_seconds`. This is what makes R1's "abandoned waiters expire after 2× TTL" and R5's "expiry wakes waiters immediately" real.

**Schema sketch** (`~/.airlock/state.db`, WAL):

- `locks(name PK, agent_id, lock_token, acquired_at, expires_at)`
- `lock_waiters(wake_token PK, name, agent_id, enqueued_at, expires_at)` — FIFO order by `enqueued_at`
- `notes(key PK, value, agent_id, written_at, expires_at NULL)`
- `counters(name PK, value, agent_id, updated_at)`
- `agents(agent_id PK, registered_at, expires_at)`
- `events(name PK, generation, last_signaled_by, last_signaled_at)`
- `tasks(task_id PK, queue, payload, state, claimant NULL, task_token NULL, lease_expires_at NULL, created_at)` — **v2.1**

**Port-27183 handover from v1.** `airlock install-service` **unloads the v1 LaunchAgent before binding** so the v2 daemon takes the port cleanly. If the daemon hits `EADDRINUSE` on startup, it **probes the occupant**: it sends an MCP `initialize` and, if it gets a **v1-shaped** response, prints the exact fix (the command to unload the old LaunchAgent / stop the old daemon and retry) rather than a bare "address in use."

**No state import.** There is **no UserDefaults → SQLite migration.** Locks and notes are *ephemeral coordination state*, not durable user data — importing half-expired locks from a previous daemon generation would be worse than starting clean. v2 takes a deliberate clean cut.

**Service management** via `airlock install-service`:

- **macOS:** launchd user LaunchAgent.
- **Linux:** systemd **user** unit (`systemctl --user`).
- **WSL:** documented notes (no systemd by default on older WSL; fall back to a shell-managed background launch or `wsl.exe` autostart hook).

---

## §8 Distribution

Zero language runtime to install (G4). Channels:

- **Homebrew tap** — `brew install adamorad/tap/airlock`. **Bottle preferred** (prebuilt binary) so install is a download, not a source build.
- **GitHub Releases** — static binaries for `darwin-arm64`, `darwin-amd64`, `linux-arm64`, `linux-amd64`.
- **`go install`** — `go install github.com/adamorad/airlock@latest` for the Go-toolchain crowd.
- **curl installer** — `curl -fsSL https://airlock.dev/install.sh | sh` — detects OS/arch, fetches the matching release binary, installs the service.

---

## §9 Non-goals

- **Not a memory layer.** Recall, embeddings, and long-term agent memory belong to mem0/engram — Airlock holds *live coordination state* (who has the lock right now), not history.
- **Not multi-machine.** Single host only. Distributed coordination across machines is Redis/etcd territory and brings a consensus problem Airlock deliberately refuses.
- **Not an orchestrator.** Airlock **coordinates, it never spawns.** It arbitrates agents that already exist; it does not start, schedule, or supervise them. (This is why tasks are descoped to v2.1 and kept minimal.)
- **Not an A2A implementation.** It sits **below** A2A: A2A is *how agents talk*, Airlock is *the arbiter they share*. Implementing A2A would conflate the messaging standard with the coordination substrate.
- **No cloud component, ever.** No hosted control plane, no account, no telemetry-home. A local-only daemon is the entire trust model; a cloud component would break the "same machine, same user, loopback-only" security story that makes it safe.

---

## §10 Risks

### Client-timeout spike (the data behind R1's cap)

Measured against Claude Code's HTTP MCP client:

| Server-side block | `MCP_TOOL_TIMEOUT` | Outcome |
|---|---|---|
| 10s | default (60000) | completes |
| 25s | default | completes |
| 55s | default | completes |
| 120s | default | **aborts at ~60s** — `MCP server "<name>" tool "<tool>" timed out after Ns` |
| 120s | 30000 | aborts at ~30s |
| 120s | 180000 | completes (~120s) |

Conclusion: the **default client timeout is 60s**, and `MCP_TOOL_TIMEOUT` moves it both directions. `wait_seconds` defaults to 25 and caps at 50 — safely under the default — and the server must never assume the client raised it.

### Binary size

The Go binary is ~10MB vs. the Swift binary's ~1MB. **Acceptable** — it buys cross-platform + zero-runtime + the goroutine model — but noted so it isn't a surprise in the release notes.

### Homebrew bottle vs. source

Bottles must be built and uploaded per-arch; a source formula is simpler to maintain but makes `brew install` a compile (slow, needs Go). Lean bottle for the install-speed win; fall back to source formula only if bottle CI is a burden.

---

## §11 Roadmap + feedback loop

| Phase | Scope |
|-------|-------|
| **Phase 1 — Core** | Locks, wake-tokens/blocking waits, atomic state (`increment_counter`, `set_note_if`), SQLite/WAL store, v1 compatibility. Ship macOS + Linux binaries. |
| **Phase 2 — Presence + events** | `register_agent` heartbeats, presence-expiry lock release + waiter wake, generation-counted events. |
| **Phase 3 — Observability** | `airlock status`, `airlock watch`, optional `--dashboard` at `/dashboard`. |
| **Phase 4 — Tasks (v2.1)** | `push_task`/`claim_next_task`/`complete_task`/`fail_task`/`list_tasks` with presence-bound leases and auto-requeue. |

**Feedback loop.** A pinned **"tear this apart"** GitHub issue inviting attacks on the design, plus a **tool-votes discussion** to let early users rank which tools to build first. If any of this is wrong for your workflow, that issue is the place to say so.
