import Foundation

let defaults = ProcessInfo.processInfo.environment["AGENTPEEK_DEFAULTS_SUITE"]
    .flatMap { UserDefaults(suiteName: $0) } ?? .standard
let lockStore = ResourceLockStore(defaults: defaults)
let noteStore = NoteStore(defaults: defaults)
let tools = MCPTools(lockStore: lockStore, noteStore: noteStore)

do {
    let server = MCPServer(tools: tools)
    try server.start()
    dispatchMain()
} catch {
    print("[AgentPeek] Failed to start: \(error)")
    exit(1)
}
