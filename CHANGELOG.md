# Changelog

## 2.0.0 — 2026-06-12

A complete rewrite of Airlock in Go. v1 (Swift, macOS-only) is archived at
`legacy/swift/` and tagged `v1.1.0`.

### Added

- **Cross-platform.** Single Go daemon runs on macOS and Linux. Pure-Go SQLite
  (`modernc.org/sqlite`, no cgo) makes Linux a trivial cross-compile.
- **Blocking lock waits.** `lock_resource` takes `wait_seconds` (0–50) to block
  until the lock frees instead of returning `locked:false` immediately. Timed-out
  callers get a `wake_token` + `queue_position` to keep their FIFO place in line.
  No more agent-authored retry loops.
- **Capability tokens.** `lock_resource` returns a `lock_token`; pass it to
  `unlock_resource`/`renew_lock`. `agent_id` is now provenance, not authorization.
  `claim_next_task` returns a `lease_token` required by `complete_task`/`fail_task`.
- **Atomic state.** `increment_counter` (collision-free unique numbers) and
  `set_note_if` (compare-and-swap) make read-modify-write race-free. `delete_note`
  added.
- **Multi-lock acquire.** `lock_resources` takes all-or-nothing in a documented
  lock ordering to avoid deadlock.
- **Agent presence.** `register_agent` heartbeats; when an agent's lease expires
  the daemon releases its locks and wakes the waiters behind them.
  `unregister_agent` / `list_agents`.
- **Events.** `signal_event` / `wait_for_event` / `clear_event` —
  generation-counted (missed-signal-safe), not a permanent latch.
- **Task queue.** `push_task` / `claim_next_task` / `complete_task` / `fail_task`
  / `list_tasks` with presence/lease-bound claims that auto-requeue on expiry.
- **CLI.** `airlock status` (one-shot snapshot of locks, agents, notes, counters,
  events, tasks) and `airlock watch` (live-updating view).
- **Service management.** `airlock install-service` / `uninstall-service` install
  a launchd LaunchAgent (macOS) or systemd user unit (Linux). Installing v2
  unloads the v1 LaunchAgent so it takes port 27183 cleanly.
- **SQLite WAL persistence** at `~/.airlock/state.db` replaces UserDefaults —
  transactional, crash-safe, with concurrent readers and a single writer.
- **Token-file auth.** A `0600` bearer token at `~/.airlock/token` is required on
  Linux/multi-user hosts (where loopback is shared) and optional on macOS.
  `AIRLOCK_TOKEN` overrides on any OS.

### Changed

- TTLs are now specified in `ttl_seconds`; `ttl_minutes` is accepted as a
  deprecated alias. The full v1 request/response shapes still work on the same
  port (27183).

## 1.0.0 — 2026-06-12

- 7 MCP tools: `lock_resource`, `unlock_resource`, `renew_lock`, `list_locks`, `set_note`, `get_note`, `list_notes`.
- Loopback-only HTTP server (127.0.0.1:27183) with Host / Origin / Content-Type enforcement.
- LaunchAgent with KeepAlive for always-on operation.
- UserDefaults persistence for locks and notes across restarts.
