# Security Policy

## Threat model

airlock is a coordination daemon for AI agents running on the **same machine, same user**. It binds an HTTP MCP server to the loopback interface (`127.0.0.1:27183`) only, and rejects requests with an unexpected `Host` header, any `Origin` header (browsers cannot call it), or a non-`application/json` Content-Type on POST.

### In scope (please report)

- Any way for the server to be reachable beyond the loopback interface.
- Bypasses of the `Host` / `Origin` / Content-Type validation (e.g. DNS rebinding or browser-based access that succeeds despite the checks).
- Anything that lets a non-local peer reach the API.

### Known, documented limitations (not vulnerabilities)

- **Same-user local access:** any process running as the same local user can call the API. This is by design; the daemon coordinates cooperating agents, it does not sandbox them.
- **Note content is untrusted cross-agent input:** values stored via `set_note` can be written by any local agent. Consuming agents must treat note content as data, never as instructions (prompt-injection hygiene is the consumer's responsibility).
- **`agent_id` is self-declared for provenance, not authorization.** It identifies who did something; it does not gate who *may*. In v2 this is **resolved** for the operations that matter: `lock_resource` returns a `lock_token` that `unlock_resource`/`renew_lock` require, and `claim_next_task` returns a `lease_token` that `complete_task`/`fail_task` require. A misbehaving agent that knows another agent's `agent_id` can no longer drop or steal its lock — it would need the capability token, which it never sees. (The legacy `agent_id`-keyed unlock path is retained for v1 compatibility; new callers should use tokens.)

### Token authentication on multi-user systems

On Linux and other multi-user hosts the loopback interface is shared across *all* local users, so loopback-only binding is not an authorization boundary there. On those systems Airlock **requires** a `0600`-permissioned bearer token at `~/.airlock/token`; clients must send it as `Authorization: Bearer <token>`. On single-user macOS the token is optional (loopback + Host/Origin checks suffice). `AIRLOCK_TOKEN` overrides the file on any OS.

## Reporting a vulnerability

Please use **GitHub private vulnerability reporting** on this repository (Security tab → "Report a vulnerability"). Do not open public issues for security reports.
