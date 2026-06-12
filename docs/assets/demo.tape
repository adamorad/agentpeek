Output docs/assets/demo.gif
Set FontSize 16
Set Width 1200
Set Height 600
Set Theme "Catppuccin Mocha"
Type "# Session A locks npm-install" Enter Sleep 600ms
Type `curl -s -X POST localhost:27183 -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"npm-install","agent_id":"session-A","ttl_minutes":5}}}' | jq -c '.result.content[0].text | fromjson'` Enter
Sleep 2s
Type "# Session B tries the same lock -- denied" Enter Sleep 600ms
Type `curl -s -X POST localhost:27183 -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"npm-install","agent_id":"session-B","ttl_minutes":5}}}' | jq -c '.result.content[0].text | fromjson'` Enter
Sleep 2s
Type "# Session A releases -- B can now acquire" Enter Sleep 600ms
Type `curl -s -X POST localhost:27183 -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"unlock_resource","arguments":{"name":"npm-install","agent_id":"session-A"}}}' | jq -c '.result.content[0].text | fromjson'` Enter
Sleep 1s
Type `curl -s -X POST localhost:27183 -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"lock_resource","arguments":{"name":"npm-install","agent_id":"session-B","ttl_minutes":5}}}' | jq -c '.result.content[0].text | fromjson'` Enter
Sleep 3s
