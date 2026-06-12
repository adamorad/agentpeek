# Airlock

**Run five agents on one repo without them stomping on each other.**

Named resource locks, atomic shared state, presence, events, and a task queue for AI agents — exposed over a local HTTP MCP server that's always running, so any session, terminal, or CI job can coordinate with any other. Cross-platform Go daemon, SQLite-backed, with real **blocking waits** — no more poll loops.

![CI](https://github.com/adamorad/airlock/actions/workflows/ci.yml/badge.svg)
![License](https://img.shields.io/badge/license-MIT-blue)
![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey)
![MCP](https://img.shields.io/badge/MCP-compatible-7C3AED)

![Airlock demo](docs/assets/demo.gif)

*Two agents reach for the same `npm-install` lock — one wins, the other blocks on a server-side wait and acquires the instant the first releases.*

<!-- Static fallback if the GIF doesn't render: -->

```
Agent A                          Agent B
──────                           ──────
lock_resource("npm-install")     lock_resource("npm-install", wait_seconds=50)
→ { locked: true, lock_token }   (parked — the daemon holds the connection open)
npm install ...
unlock_resource(lock_token)
                                 → { locked: true, lock_token }   ← wakes instantly
                                 npm install ...
```

## The problem

Open two Claude Code sessions on the same repo — one adding auth, one adding notifications. Both reach for `npm install` at the same time. Both pick `003_` as the next migration filename. Neither knows the other exists.

There's no shared state. No handoff. No way for one agent to know what another is doing. Coordination lives in fragile prompt instructions ("please wait until migrations finish") that nothing enforces.

## How it works

Airlock is a single Go daemon that runs as a background service — always on, surviving IDE and terminal restarts. It listens on `127.0.0.1:27183` and speaks MCP over HTTP. Any agent that has it configured can:

- **acquire locks** — and *block* on a contended one (`wait_seconds`, up to 50s), so the daemon parks the caller and wakes it the instant the lock frees. No agent-authored retry loops.
- **share atomic state** — `increment_counter` for collision-free unique numbers, `set_note_if` for compare-and-swap. Read-modify-write is race-free, not best-effort.
- **announce presence** — `register_agent` heartbeats; when an agent dies, its locks release and its waiters wake immediately.
- **signal events** — generation-counted `signal_event` / `wait_for_event` for handoffs without polling.
- **queue work** — a small task queue with presence-bound leases that auto-requeue on crash.

State lives in **SQLite (WAL mode)** at `~/.airlock/state.db` — transactional, crash-safe, with concurrent readers and a single writer. The store is pure-Go (`modernc.org/sqlite`, no cgo), which is what makes the Linux build a trivial cross-compile. One binary, no language runtime, no external database.

## Install

### (a) `go install`

```bash
go install github.com/adamorad/airlock@latest   # builds the `airlock` binary
airlock install-service                          # launchd (macOS) / systemd user unit (Linux)
```

### (b) Homebrew

```bash
brew install adamorad/tap/airlock
airlock install-service
```

### (c) Build from source

```bash
git clone https://github.com/adamorad/airlock.git
cd airlock
go build -o airlock .
sudo install airlock /usr/local/bin/airlock     # or anywhere on your PATH
airlock install-service
```

`install-service` registers an always-on background service (and unloads the old v1 LaunchAgent if present, so the v2 daemon takes port 27183 cleanly).

### Verify

```bash
airlock status
# Airlock — 127.0.0.1:27183
#
# LOCKS (0)
#   (none)
# ...
```

### Configure your agent

**Claude Code** — one command:

```bash
claude mcp add --transport http airlock http://localhost:27183
```

**Cursor / Windsurf / any MCP client** — add this to your MCP config:

```json
{
  "mcpServers": {
    "airlock": {
      "type": "http",
      "url": "http://127.0.0.1:27183"
    }
  }
}
```

**On Linux** (and any multi-user host) a bearer token is **required** — loopback is shared across all users there, so it isn't an authorization boundary on its own. The daemon writes a `0600` token to `~/.airlock/token` on first run; send it as an `Authorization: Bearer <token>` header:

```json
{
  "mcpServers": {
    "airlock": {
      "type": "http",
      "url": "http://127.0.0.1:27183",
      "headers": { "Authorization": "Bearer <paste contents of ~/.airlock/token>" }
    }
  }
}
```

On **macOS** the daemon is loopback-only by default and the token is optional. `AIRLOCK_TOKEN` overrides the file on any OS.

## Tools

Airlock exposes **22 tools** over MCP. Every tool result is a JSON object (the `list_*` tools return a JSON array). TTLs are in `ttl_seconds`; `ttl_minutes` is accepted as a deprecated alias.

### Locks

| Tool | Args | Returns |
|------|------|---------|
| `lock_resource` | `name`, `agent_id`, `ttl_seconds?`=900, `wait_seconds?`=0 (cap 50), `wake_token?` | `{locked:true, lock_token, expires_in_seconds}` — or, if contended, `{locked:false, held_by, expires_in_seconds}` plus `{wake_token, queue_position, retry_with}` when you were queued |
| `unlock_resource` | `name`, `lock_token` (preferred) or `agent_id` | `{released: bool}` |
| `renew_lock` | `name`, `lock_token` (preferred) or `agent_id`, `ttl_seconds?`=900 | `{renewed:true, expires_in_seconds}` or `{renewed:false, error}` |
| `list_locks` | — | `[{name, agent_id, expires_in_seconds}]` |
| `lock_resources` | `names:[string]`, `agent_id`, `ttl_seconds?`=900 | all-or-nothing: `{locked:true, tokens:{name:token}}` or `{locked:false, held_by}` |

`wait_seconds` is the coordination-by-default knob: pass it (up to **50**) and `lock_resource` **blocks** server-side until the lock frees, instead of returning `locked:false` immediately. The cap stays under Claude Code's 60s default per-tool-call timeout. The returned **`lock_token` is a capability** — `unlock_resource`/`renew_lock` require it (the `agent_id` path is kept for v1 compatibility). `lock_resources` acquires in a documented lock-ordering (lexicographic by name) so two callers can't deadlock.

### Notes & State

| Tool | Args | Returns |
|------|------|---------|
| `set_note` | `key`, `value`, `author?`, `ttl_seconds?` | `{saved:true}` |
| `get_note` | `key` | `{key, value, author?, expires_in_seconds?}` or `{found:false}` |
| `list_notes` | — | `[{key, value, author?, expires_in_seconds?}]` |
| `delete_note` *(v2)* | `key` | `{deleted: bool}` |
| `set_note_if` *(v2)* | `key`, `expected_value`, `new_value`, `author?`, `ttl_seconds?` | `{swapped: bool}` (true only if the stored value equaled `expected_value`; an absent/expired note counts as `""`) |
| `increment_counter` *(v2)* | `name`, `by?`=1 | `{value}` (post-increment; collision-free under concurrency) |

### Presence

| Tool | Args | Returns |
|------|------|---------|
| `register_agent` *(v2)* | `agent_id`, `ttl_seconds?`=60 | `{registered:true, expires_in_seconds}` — re-call to stay alive; when it lapses the agent's locks auto-release |
| `unregister_agent` *(v2)* | `agent_id` | `{unregistered:true}` — also releases held locks |
| `list_agents` *(v2)* | — | `[{agent_id, expires_in_seconds}]` |

### Events

| Tool | Args | Returns |
|------|------|---------|
| `signal_event` *(v2)* | `name` | `{generation}` (the new, bumped generation; wakes all waiters) |
| `wait_for_event` *(v2)* | `name`, `last_seen_generation?`=0, `wait_seconds?`=25 (cap 50) | `{generation, fired}` — blocks until the generation advances past `last_seen_generation`, or the window expires (`fired:false`) |
| `clear_event` *(v2)* | `name` | `{cleared:true}` |

Events are **generation-counted**, not latched: pass back the generation you last saw and a signal that fired between calls is never missed.

### Tasks

| Tool | Args | Returns |
|------|------|---------|
| `push_task` *(v2)* | `queue`, `payload`, `author?`, `priority?`=0 | `{id}` (higher priority / older claimed first) |
| `claim_next_task` *(v2)* | `queue`, `agent_id`, `lease_seconds?`=120 | `{claimed:true, id, payload, lease_token}` or `{claimed:false}` |
| `complete_task` *(v2)* | `id`, `lease_token` | `{completed: bool}` |
| `fail_task` *(v2)* | `id`, `lease_token`, `requeue?`=true | `{failed: bool}` (requeue=true returns it to pending; false gives up) |
| `list_tasks` *(v2)* | `queue` | `[{id, queue, payload, priority, state, author?, lease_agent?, lease_expires_in_seconds?}]` |

A claim is a **lease**: if the claimant doesn't `complete_task`/`fail_task` within `lease_seconds`, the task auto-requeues for another consumer — no work is lost to a crashed worker. The `lease_token` from `claim_next_task` is the capability `complete_task`/`fail_task` require.

### Naming conventions

Use consistent names so agents understand each other:

- **Files:** `file:/abs/path/to/package.json`
- **Processes:** `npm-install`, `db-migrations`, `tests:unit`
- **Agent identity:** `agent:claude-session-abc`

## Recipes

### (a) Serialize edits to one file with a blocking lock

Two subagents both need to edit `package.json`. They serialize on a single lock — the second one *blocks* (no retry loop) and wakes the instant the first releases.

```bash
# Subagent A — block up to 50s to acquire, edit, release.
A=$(curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"file:/repo/package.json","agent_id":"sub-A","wait_seconds":50}}}' \
  | jq -r '.result.content[0].text | fromjson | .lock_token')
# ...edit the file...
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"unlock_resource\",\"arguments\":{\"name\":\"file:/repo/package.json\",\"lock_token\":\"$A\"}}}" \
  | jq -c '.result.content[0].text | fromjson'      # → {"released":true}

# Subagent B — the same call with wait_seconds=50 parks until A releases, then
# returns {locked:true, lock_token} the moment the lock frees. No polling.
```

### (b) Unique migration filenames — race-free

The classic v1 hazard: two agents `get_note` the counter at the same instant, both read `003`, both write `004`. In v2 there is no read-modify-write to lose — `increment_counter` is atomic, so 100 concurrent callers get 100 distinct numbers.

```bash
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"increment_counter","arguments":{"name":"migration-seq"}}}' \
  | jq -c '.result.content[0].text | fromjson'      # → {"value":4}
# Use the returned value: create 0004_*.sql. The next caller gets 5, never a collision.
```

### (c) Producer/consumer handoff via the task queue

A planner agent pushes work; one or more worker agents claim it. A lease means a crashed worker's task auto-requeues instead of vanishing.

```bash
# Producer — enqueue a unit of work.
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"push_task","arguments":{"queue":"build","payload":"compile module X","author":"planner"}}}' \
  | jq -c '.result.content[0].text | fromjson'      # → {"id":1}

# Consumer — claim the next task, do the work, complete it with the lease token.
CLAIM=$(curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"claim_next_task","arguments":{"queue":"build","agent_id":"worker-1"}}}' \
  | jq -c '.result.content[0].text | fromjson')     # → {"claimed":true,"id":1,"payload":"compile module X","lease_token":"..."}
ID=$(echo "$CLAIM" | jq -r .id); TOK=$(echo "$CLAIM" | jq -r .lease_token)
# ...do the work...
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"complete_task\",\"arguments\":{\"id\":$ID,\"lease_token\":\"$TOK\"}}}" \
  | jq -c '.result.content[0].text | fromjson'      # → {"completed":true}
```

## `airlock status`

One-shot snapshot of everything the daemon is coordinating — locks (and who holds them), present agents, notes, counters, events, and tasks. Runs against `~/.airlock/state.db`, so it works even from a different shell. `airlock watch` reprints it live once a second.

```
Airlock — 127.0.0.1:27183

LOCKS (2)
  NAME                     HELD BY    EXPIRES IN
  file:/repo/package.json  sub-A      4m30s
  npm-install              session-A  12m

AGENTS (2)
  AGENT      EXPIRES IN
  session-A  48s
  worker-1   59s

NOTES (1)
  KEY            VALUE              AUTHOR     EXPIRES IN
  deploy-status  building           planner    —

COUNTERS (1)
  NAME           VALUE
  migration-seq  4

EVENTS (1)
  NAME             GENERATION
  migrations-done  2

TASKS (1)
  ID  QUEUE  STATE    PAYLOAD            LEASE AGENT
  2   build  claimed  compile module Y   worker-1
```

## Architecture

A single Go daemon, one process, on `127.0.0.1:27183`.

- **Store.** SQLite in **WAL mode** at `~/.airlock/state.db` — pure-Go (`modernc.org/sqlite`, no cgo), transactional, crash-safe. A single dedicated write connection serializes writes; readers use the WAL's concurrent path. SQLite's single-writer model is what makes `increment_counter` and `set_note_if` correct for free.
- **Blocking waits.** A parked waiter is a goroutine on a per-resource **in-memory FIFO channel** with a `context` deadline — *outside* any SQLite transaction, so a blocking daemon never melts the database under contention. A background **reaper** sweeps expired lock TTLs and presence leases and wakes the FIFO head, so waiters behind a dead holder don't eat their full wait window.
- **Capability tokens.** `lock_token` / `lease_token` gate `unlock`/`renew` and `complete`/`fail`. `agent_id` is provenance, not authorization.
- **Security.** Loopback-only bind, `Host`-header allowlist, reject any `Origin` header (DNS-rebinding defense), require `application/json` on POST — plus a `0600` bearer token at `~/.airlock/token`, required on Linux/multi-user hosts. See [SECURITY.md](SECURITY.md).
- **Port 27183.** [PortPeek](https://adamorad.github.io/portpeek) owns `27182` (the digits of *e*); Airlock takes the next port so the two daemons coexist.

v1 (Swift, macOS-only) is archived at `legacy/swift/` and tagged `v1.1.0`. The full v1 wire contract still works unchanged on v2 — same port, same JSON.

## Roadmap / v2 status

**v2.0.0 ships the core** — locks with blocking waits, atomic state, presence, and events — **plus the task queue**, all in Go and cross-platform (macOS + Linux). The design spec and rationale, including how Airlock sits against adjacent tools, live in [docs/ROADMAP-v2.md](docs/ROADMAP-v2.md). If the roadmap is wrong for your workflow, [open an issue](https://github.com/adamorad/airlock/issues) — there's a pinned "tear this apart" thread for exactly that.

## Uninstall

```bash
airlock uninstall-service        # remove the launchd / systemd service
brew uninstall airlock           # if installed via the tap: brew uninstall adamorad/tap/airlock
rm -rf ~/.airlock                # optional: drop the state db + token
```

## FAQ

**Why port 27183?** PortPeek already uses `27182` (the first digits of *e*, Euler's number). Airlock takes the next port, `27183`, so the two daemons coexist on one machine without colliding.

**What's the security model?** Airlock binds loopback only, validates the `Host` header, rejects any `Origin` header, and requires `application/json` on POST. On Linux/multi-user hosts a `0600` bearer token at `~/.airlock/token` is **required** (loopback is shared across users there); on macOS it's optional. Capability tokens (`lock_token` / `lease_token`) mean a buggy or prompt-injected agent can't drop or steal another agent's lock just by guessing its `agent_id`. The deliberate, documented limit: any process running as the same local user can call the API — Airlock coordinates cooperating agents, it doesn't sandbox them. Note/task content is untrusted cross-agent input; consuming agents must treat it as data, never instructions. Full threat model: [SECURITY.md](SECURITY.md).

**How does it relate to PortPeek?** [PortPeek](https://adamorad.github.io/portpeek) solves *port* conflicts for AI agents (who's bound to which port). Airlock solves the broader *coordination* problem (who's doing which task). Same lineage, adjacent ports (`27182` / `27183`), designed to run side by side.

## Contributing

Issues and PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

Licensed under the [MIT License](LICENSE).

Built by [Adam Morad](https://il.linkedin.com/in/adam-morad).
