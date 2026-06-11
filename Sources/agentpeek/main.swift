import Foundation

let lockStore = ResourceLockStore()
let noteStore = NoteStore()
let tools = MCPTools(lockStore: lockStore, noteStore: noteStore)

do {
    let server = MCPServer(tools: tools)
    try server.start()
    dispatchMain()
} catch {
    print("[AgentPeek] Failed to start: \(error)")
    exit(1)
}
