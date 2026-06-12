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
- **`agent_id` is self-declared:** it identifies, but does not authenticate or authorize. A misbehaving local agent can claim any `agent_id`. Capability tokens are planned for v2 — see [docs/ROADMAP-v2.md](docs/ROADMAP-v2.md).

## Reporting a vulnerability

Please use **GitHub private vulnerability reporting** on this repository (Security tab → "Report a vulnerability"). Do not open public issues for security reports.
