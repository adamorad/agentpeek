import Foundation

let lockStore = ResourceLockStore()
let noteStore = NoteStore()
let tools = MCPTools(lockStore: lockStore, noteStore: noteStore)

do {
    let server = MCPServer(tools: tools)
    try server.start()
    print("[AgentPeek] Started. Listening on 127.0.0.1:27183")
    dispatchMain()
} catch {
    print("[AgentPeek] Failed to start: \(error)")
    exit(1)
}
