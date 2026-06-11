// Sources/agentpeek/MCP/MCPTools.swift — STUB, will be replaced in Task 5
import Foundation

@MainActor
final class MCPTools {
    nonisolated init(lockStore: ResourceLockStore, noteStore: NoteStore) {}

    var toolDefinitions: [[String: Any]] { [] }

    func handle(tool: String, params: [String: Any]) -> Any {
        ["error": "not implemented"]
    }
}
