# AgentPeek

**Run five agents on one repo without them stomping on each other.**

Named resource locks and a shared scratchpad for AI agents — exposed over a local HTTP MCP server that's always running, so any session, terminal, or CI job can coordinate with any other.

![CI](https://github.com/adamorad/agentpeek/actions/workflows/ci.yml/badge.svg)
![License](https://img.shields.io/badge/license-MIT-blue)
![Platform](https://img.shields.io/badge/platform-macOS%2013%2B-lightgrey)
![MCP](https://img.shields.io/badge/MCP-compatible-7C3AED)

![AgentPeek demo](docs/assets/demo.gif)

*Two agents reach for the same `npm-install` lock — one wins, the other is told who holds it, and it acquires the instant the first releases.*

<!-- Static fallback if the GIF doesn't render: -->

```
Agent A                          Agent B
──────                           ──────
lock_resource("npm-install")     lock_resource("npm-install")
→ { locked: true }               → { locked: false, held_by: "A" }
npm install ...                  (waits)
unlock_resource("npm-install")
                                 lock_resource("npm-install")
                                 → { locked: true }
                                 npm install ...
```

## See it yourself in 30 seconds

With the daemon running (see [Quickstart](#quickstart)), open two terminals and watch them coordinate. Every response is unwrapped with `jq -c '.result.content[0].text | fromjson'` so you see clean JSON instead of escaped strings.

**Terminal 1 — acquire the lock:**

```bash
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"npm-install","agent_id":"session-A","ttl_minutes":5}}}' \
  | jq -c '.result.content[0].text | fromjson'
# → {"expires_in_seconds":299,"locked":true}
```

**Terminal 2 — same lock, denied (and told who holds it):**

```bash
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"npm-install","agent_id":"session-B","ttl_minutes":5}}}' \
  | jq -c '.result.content[0].text | fromjson'
# → {"expires_in_seconds":299,"held_by":"session-A","locked":false}
```

**Terminal 1 — release:**

```bash
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"unlock_resource","arguments":{"name":"npm-install","agent_id":"session-A"}}}' \
  | jq -c '.result.content[0].text | fromjson'
# → {"released":true}
```

**Terminal 2 — retry, now it acquires:**

```bash
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"npm-install","agent_id":"session-B","ttl_minutes":5}}}' \
  | jq -c '.result.content[0].text | fromjson'
# → {"expires_in_seconds":299,"locked":true}
```

That's the whole model: one shared, always-on arbiter; agents ask before they act.

## Quickstart

### 1. Install via Homebrew

```bash
brew install adamorad/tap/agentpeek
```

### 2. Load the LaunchAgent

```bash
cp $(brew --prefix)/share/agentpeek/com.agentpeek.daemon.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.agentpeek.daemon.plist
```

AgentPeek now starts automatically on login and restarts if it crashes.

### 3. Verify

```bash
curl -s -X POST http://127.0.0.1:27183/ \
  -H "Content-Type: application/json" \
  -H "Host: 127.0.0.1:27183" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
# → {"result":{"serverInfo":{"name":"agentpeek","version":"1.0.0"},...}}
```

### 4. Configure your agent

**Claude Code** — one command:

```bash
claude mcp add --transport http agentpeek http://localhost:27183
```

**Cursor / Windsurf / any MCP client** — add this to your MCP config:

```json
{
  "mcpServers": {
    "agentpeek": {
      "type": "http",
      "url": "http://127.0.0.1:27183"
    }
  }
}
```

<details>
<summary>Build from source</summary>

```bash
git clone https://github.com/adamorad/agentpeek.git
cd agentpeek
swift build -c release
sudo cp .build/release/agentpeek /usr/local/bin/agentpeek
cp com.agentpeek.daemon.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.agentpeek.daemon.plist
```

</details>

## The problem

Open two Claude Code sessions on the same repo — one adding auth, one adding notifications. Both reach for `npm install` at the same time. Both pick `003_` as the migration filename. Neither knows the other exists.

There's no shared state. No handoff. No way for one agent to know what another is doing.

## How it works

AgentPeek is a native Swift binary that runs as a macOS LaunchAgent — always on, surviving IDE and terminal restarts. It listens on `127.0.0.1:27183` and speaks MCP over HTTP. Any agent that has it configured can acquire locks and read/write notes. Locks are TTL-based, so they auto-expire if an agent crashes.

No Node.js. No Python. No database. One binary, zero dependencies.

## Tools

### Locks

| Tool | Parameters | Returns |
|------|-----------|---------|
| `lock_resource` | `name`, `agent_id`, `ttl_minutes` (default 15) | `{locked: true, expires_in_seconds}` or `{locked: false, held_by, expires_in_seconds}` |
| `unlock_resource` | `name`, `agent_id` | `{released: true}` (no-op if held by another agent) |
| `renew_lock` | `name`, `agent_id`, `ttl_minutes` | `{renewed: true, expires_in_seconds}` |
| `list_locks` | — | `[{name, agent_id, expires_in_seconds}]` |

### Scratchpad

| Tool | Parameters | Returns |
|------|-----------|---------|
| `set_note` | `key`, `value`, `author?`, `ttl_minutes?` | `{saved: true}` |
| `get_note` | `key` | `{key, value, author?, expires_in_seconds?}` or `{found: false}` |
| `list_notes` | — | `[{key, value, author?, expires_in_seconds?}]` |

#### Naming conventions

Use consistent names so agents understand each other:

- **Files:** `file:/abs/path/to/package.json`
- **Processes:** `npm-install`, `db-migrations`, `tests:unit`
- **Agent identity:** `agent:claude-session-abc`

> **Honest semantics.** v1 locks are **poll-based**, not blocking. A contended `lock_resource` returns `{locked: false, held_by}` *immediately* — the agent that wants the lock re-calls when it's ready to try again. There is no server-side queue and no wake-on-release yet. True blocking waits (call once, the daemon parks you and wakes you the instant the lock frees) land in v2. See the [v2 roadmap](docs/ROADMAP-v2.md).

## How it compares

How AgentPeek (v1, this repo) sits against adjacent tools, **as of June 2026**. Distilled from the fuller landscape in the [v2 roadmap, §3](docs/ROADMAP-v2.md).

| Tool | Always-on daemon | Cross-session locks | TTL auto-expiry | Zero deps | MCP-native |
|------|:-:|:-:|:-:|:-:|:-:|
| [agent-orchestration](https://github.com/madebyaris/agent-orchestration) (MCP locks/queue/memory, Node, session-scoped) | ❌ | ❌ | ❓ | ❌ (Node) | ✅ |
| [Swarm Tools](https://swarmtools.ai) (file reservations + agent mail, tied to its orchestrator) | ❌ | partial | ❓ | ❌ | ❌ |
| memory layers — [mem0](https://github.com/mem0ai/mem0) / engram (recall, not real-time coordination) | ❌ | ❌ | ❌ | ❌ | ✅ |
| Redis / `flock` lockfiles (capable, but real setup or no TTL + no MCP surface) | partial | ✅ | ❌ | ❌ | ❌ |
| **AgentPeek** | ✅ | ✅ | ✅ | ✅ | ✅ |

The unoccupied corner: an *always-on*, *zero-dependency*, *MCP-native* coordinator that survives every session. Memory layers remember the past; AgentPeek arbitrates the present. Redis has the semantics but no MCP surface and a real install.

## Recipes

### (a) Serialize `npm install` across two sessions

Both sessions want to run `npm install`; only one should at a time.

```bash
# Session A — take the lock, install, release.
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"npm-install","agent_id":"session-A","ttl_minutes":5}}}' \
  | jq -c '.result.content[0].text | fromjson'      # → {"locked":true,...}
npm install
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"unlock_resource","arguments":{"name":"npm-install","agent_id":"session-A"}}}' \
  | jq -c '.result.content[0].text | fromjson'      # → {"released":true}

# Session B — if locked:false, it sees held_by and re-polls; on locked:true it proceeds.
```

### (b) Unique migration filenames via shared notes

Two sessions both need the next migration number. They keep a counter in the scratchpad.

```bash
# Read the current counter...
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_note","arguments":{"key":"migration-counter"}}}' \
  | jq -c '.result.content[0].text | fromjson'      # → {"key":"migration-counter","value":"003",...}

# ...then write the next one before creating 004_*.sql:
curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"set_note","arguments":{"key":"migration-counter","value":"004","author":"session-B"}}}' \
  | jq -c '.result.content[0].text | fromjson'      # → {"saved":true}
```

> **Caveat — this is read-modify-write and can race.** Two agents that `get_note` at the same instant both read `003` and both write `004`. To make it safe in v1, hold a lock (`migration-counter`) around the get-then-set. v2 closes this properly with `set_note_if` (compare-and-swap) and `increment_counter` (atomic, collision-free) — see the [roadmap](docs/ROADMAP-v2.md).

### (c) A CI script and an interactive agent sharing one lock

A CI job and a developer's agent both deploy from the same machine. Neither should deploy while the other is mid-deploy — so both gate on the same `deploy` lock.

```bash
# CI script (ci-deploy.sh)
RESULT=$(curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"deploy","agent_id":"ci-runner","ttl_minutes":10}}}' \
  | jq -r '.result.content[0].text | fromjson | .locked')
if [ "$RESULT" = "true" ]; then
  ./deploy.sh
  curl -s -X POST localhost:27183 -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"unlock_resource","arguments":{"name":"deploy","agent_id":"ci-runner"}}}' > /dev/null
else
  echo "deploy held by another agent — skipping this run"
fi
```

The interactive agent acquires the very same `deploy` lock with its own `agent_id` before it deploys, so the two never overlap.

## Roadmap

v1 (this repo) is shipped and supported. v2 is the next-generation coordination layer — design-complete, build in progress:

- **Blocking waits.** Call `lock_resource` once; the daemon parks you and wakes you the instant the lock frees — no agent-authored retry loops.
- **Atomic state.** `increment_counter` (collision-free unique numbers) and `set_note_if` (compare-and-swap) make read-modify-write safe.
- **Presence / heartbeats.** `register_agent` leases — a crashed agent's locks release immediately and its waiters wake.
- **Events.** `signal_event` / `wait_for_event` (generation-counted, missed-signal-safe) for handoffs without polling.
- **Linux.** A Go rewrite with a pure-Go SQLite store ships `linux-amd64` / `linux-arm64` binaries alongside macOS.

Full design and rationale: [docs/ROADMAP-v2.md](docs/ROADMAP-v2.md). If the roadmap is wrong for your workflow, [open an issue](https://github.com/adamorad/agentpeek/issues) — there's a pinned "tear this apart" thread for exactly that.

## Uninstall

```bash
# 1. Stop and unload the LaunchAgent
launchctl unload ~/Library/LaunchAgents/com.agentpeek.daemon.plist

# 2. Remove the plist
rm ~/Library/LaunchAgents/com.agentpeek.daemon.plist

# 3. Remove the binary
brew uninstall agentpeek          # if installed via the tap: brew uninstall adamorad/tap/agentpeek
```

## FAQ

**Why port 27183?** PortPeek already uses `27182` (the first digits of *e*, Euler's number). AgentPeek takes the next port, `27183`, so the two daemons coexist on one machine without colliding.

**What's the security model?** AgentPeek binds to the loopback interface (`127.0.0.1`) only, validates the `Host` header, and rejects any request carrying an `Origin` header (browsers can't reach it — a DNS-rebinding defence). The deliberate, documented caveat: **any process running as the same local user can call the API** — the daemon coordinates cooperating agents, it doesn't sandbox them. Note content is untrusted cross-agent input; consuming agents must treat it as data, never instructions. Full threat model: [SECURITY.md](SECURITY.md).

**How does it relate to PortPeek?** [PortPeek](https://adamorad.github.io/portpeek) solves *port* conflicts for AI agents (who's bound to which port). AgentPeek solves the broader *coordination* problem (who's doing which task). Same lineage, adjacent ports (`27182` / `27183`), designed to run side by side.

## Contributing

Issues and PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

Licensed under the [MIT License](LICENSE).

Built by [Adam Morad](https://il.linkedin.com/in/adam-morad).
