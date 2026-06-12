# AgentPeek

**Agent coordination daemon for macOS.** Named resource locks and a shared scratchpad — available to any MCP-compatible AI agent via a local HTTP server that's always running.

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

## The problem

Open two Claude Code sessions on the same repo — one adding auth, one adding notifications. Both reach for `npm install` at the same time. Both pick `003_` as the migration filename. Neither knows the other exists.

There's no shared state. No handoff. No way for one agent to know what another is doing.

## How it works

AgentPeek is a native Swift binary that runs as a macOS LaunchAgent — always on, survives IDE and terminal restarts. It listens on `127.0.0.1:27183` and speaks MCP over HTTP. Any agent that has it configured can acquire locks and read/write notes. Locks are TTL-based so they auto-expire if an agent crashes.

No Node.js. No Python. No database. One binary, zero dependencies.

## Install

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

**Claude Code:**
```bash
claude mcp add --transport http agentpeek http://localhost:27183
```

**Any MCP client** — add to your MCP config:
```json
{
  "agentpeek": {
    "transport": "http",
    "url": "http://127.0.0.1:27183"
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

## Tools

### Locks

| Tool | Parameters | Returns |
|------|-----------|---------|
| `lock_resource` | `name`, `agent_id`, `ttl_minutes` (default 15) | `{locked: true, expires_in_seconds}` or `{locked: false, held_by, expires_in_seconds}` |
| `unlock_resource` | `name`, `agent_id` | `{released: true}` |
| `renew_lock` | `name`, `agent_id`, `ttl_minutes` | `{renewed: true, expires_in_seconds}` |
| `list_locks` | — | `[{name, agent_id, expires_in_seconds}]` |

### Scratchpad

| Tool | Parameters | Returns |
|------|-----------|---------|
| `set_note` | `key`, `value`, `author?`, `ttl_minutes?` | `{saved: true}` |
| `get_note` | `key` | `{key, value, author?, expires_in_seconds?}` or `{found: false}` |
| `list_notes` | — | `[{key, value, author?, expires_in_seconds?}]` |

### Naming conventions

Use consistent names so agents understand each other:

- **Files:** `file:/abs/path/to/package.json`
- **Processes:** `npm-install`, `db-migrations`, `tests:unit`
- **Agent identity:** `agent:claude-session-abc`

## Example: two sessions, zero conflicts

Session A is adding auth. Session B is adding notifications. Both need to run migrations.

**Session A:**
```
lock_resource("db-migrations", "session-A", ttl_minutes=30)
→ { locked: true, expires_in_seconds: 1800 }

set_note("migration-counter", "003", author="session-A")
→ { saved: true }

# ... creates 003_add_users_table.sql ...

unlock_resource("db-migrations", "session-A")
→ { released: true }
```

**Session B (concurrent):**
```
lock_resource("db-migrations", "session-B", ttl_minutes=30)
→ { locked: false, held_by: "session-A", expires_in_seconds: 1743 }

# knows to wait — polls or defers the migration task

get_note("migration-counter")
→ { key: "migration-counter", value: "003", author: "session-A" }

# after A releases:
lock_resource("db-migrations", "session-B", ttl_minutes=30)
→ { locked: true }

# safely creates 004_add_notifications_table.sql
```

No conflicts. No coordination overhead. Each session calls 2–3 MCP tools.

## Architecture

```
agentpeek/
├── Package.swift
├── Sources/agentpeek/
│   ├── main.swift                  — wires stores + starts HTTP server
│   ├── Core/
│   │   ├── ResourceLockStore.swift — TTL-based named locks, ownership-enforced
│   │   └── NoteStore.swift         — shared key-value scratchpad
│   └── MCP/
│       ├── MCPServer.swift         — NWListener, loopback-only, security headers
│       ├── MCPHandler.swift        — JSON-RPC 2.0 dispatch
│       └── MCPTools.swift          — 7 tool implementations
└── com.agentpeek.daemon.plist      — LaunchAgent (KeepAlive: true)
```

- **Persistence:** UserDefaults — locks and notes survive daemon restarts
- **Thread safety:** Serial `DispatchQueue` per store
- **Security:** Binds to `127.0.0.1` only, validates `Host` header, rejects `Origin` header (DNS-rebinding defence)
- **Port:** `27183` (PortPeek uses `27182` — they're designed to coexist)

## Sister project

[PortPeek](https://portpeek.app) solves port conflicts for AI agents. AgentPeek solves the broader coordination problem. They run side by side.

## License

MIT
