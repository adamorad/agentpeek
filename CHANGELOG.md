# Changelog

## Unreleased

- CI smoke hardened: hermetic UserDefaults via `AGENTPEEK_DEFAULTS_SUITE` env var, readiness poll instead of fixed sleep, daemon teardown on exit.
- Added `NoteStore` unit tests (roundtrip, expiry, delete, persistence).

## 1.0.0 — 2026-06-12

- 7 MCP tools: `lock_resource`, `unlock_resource`, `renew_lock`, `list_locks`, `set_note`, `get_note`, `list_notes`.
- Loopback-only HTTP server (127.0.0.1:27183) with Host / Origin / Content-Type enforcement.
- LaunchAgent with KeepAlive for always-on operation.
- UserDefaults persistence for locks and notes across restarts.
