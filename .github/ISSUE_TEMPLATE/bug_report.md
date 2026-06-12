---
name: Bug report
about: Report a problem with agentpeek
title: ''
labels: bug
assignees: ''
---

**agentpeek version**
<!-- e.g. 1.0.0 -->

**macOS version**
<!-- e.g. macOS 14.5 (Sonoma) -->

**Repro curl command**
```bash
# e.g.
curl -s -X POST http://127.0.0.1:27183/ -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"r","agent_id":"A"}}}'
```

**Expected behavior**
<!-- What you expected to happen -->

**Actual behavior**
<!-- What actually happened, including the full response -->
