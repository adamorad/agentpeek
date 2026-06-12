import Foundation

@MainActor
final class MCPTools {
    private let lockStore: ResourceLockStore
    private let noteStore: NoteStore

    nonisolated init(lockStore: ResourceLockStore, noteStore: NoteStore) {
        self.lockStore = lockStore
        self.noteStore = noteStore
    }

    var toolDefinitions: [[String: Any]] {
        [
            makeTool(
                "lock_resource",
                "Acquire a named lock. Returns {locked:true, expires_in_seconds} or {locked:false, held_by, expires_in_seconds}. Naming conventions: file:/abs/path for files, npm-install for processes, agent:id for presence.",
                required: ["name", "agent_id"],
                optional: ["ttl_minutes"]
            ),
            makeTool(
                "unlock_resource",
                "Release a lock you hold. No-op if held by another agent. Returns {released:true}.",
                required: ["name", "agent_id"],
                optional: []
            ),
            makeTool(
                "renew_lock",
                "Extend your lock's TTL (heartbeat for long-running tasks). Returns {renewed:true, expires_in_seconds}.",
                required: ["name", "agent_id"],
                optional: ["ttl_minutes"]
            ),
            makeTool(
                "list_locks",
                "List all active locks with holder and TTL remaining. Returns [{name, agent_id, expires_in_seconds}].",
                required: [],
                optional: []
            ),
            makeTool(
                "set_note",
                "Write a shared note. Any agent can read or overwrite. Optional TTL. Returns {saved:true}.",
                required: ["key", "value"],
                optional: ["author", "ttl_minutes"]
            ),
            makeTool(
                "get_note",
                "Read a shared note by key. Returns {key, value, author?, expires_in_seconds?} or {found:false}.",
                required: ["key"],
                optional: []
            ),
            makeTool(
                "list_notes",
                "List all active notes. Returns [{key, value, author?, expires_in_seconds?}].",
                required: [],
                optional: []
            ),
        ]
    }

    func handle(tool: String, params: [String: Any]) -> Any {
        switch tool {
        case "lock_resource":
            guard let name = params["name"] as? String,
                  let agentId = params["agent_id"] as? String else {
                return errorResult("missing required params: name, agent_id")
            }
            let ttl = (params["ttl_minutes"] as? Int) ?? (params["ttl_minutes"] as? Double).map { Int($0) } ?? 15
            do {
                try lockStore.lock(name: name, agentId: agentId, ttlMinutes: ttl)
                let expiry = lockStore.allActiveLocks().first { $0.name == name }?.expiresInSeconds ?? (ttl * 60)
                return ["locked": true, "expires_in_seconds": expiry]
            } catch LockError.heldBy(let holder, let exp) {
                return ["locked": false, "held_by": holder, "expires_in_seconds": exp]
            } catch {
                return errorResult(error.localizedDescription)
            }

        case "unlock_resource":
            guard let name = params["name"] as? String,
                  let agentId = params["agent_id"] as? String else {
                return errorResult("missing required params: name, agent_id")
            }
            lockStore.unlock(name: name, agentId: agentId)
            return ["released": true]

        case "renew_lock":
            guard let name = params["name"] as? String,
                  let agentId = params["agent_id"] as? String else {
                return errorResult("missing required params: name, agent_id")
            }
            let ttl = (params["ttl_minutes"] as? Int) ?? (params["ttl_minutes"] as? Double).map { Int($0) } ?? 15
            do {
                try lockStore.renew(name: name, agentId: agentId, ttlMinutes: ttl)
                let expiry = lockStore.allActiveLocks().first { $0.name == name }?.expiresInSeconds ?? (ttl * 60)
                return ["renewed": true, "expires_in_seconds": expiry]
            } catch LockError.notFound {
                return errorResult("lock '\(name)' not found or expired")
            } catch LockError.notOwned {
                return errorResult("lock '\(name)' is not owned by agent '\(agentId)'")
            } catch {
                return errorResult(error.localizedDescription)
            }

        case "list_locks":
            return lockStore.allActiveLocks().map { lock in
                ["name": lock.name, "agent_id": lock.agentId, "expires_in_seconds": lock.expiresInSeconds] as [String: Any]
            }

        case "set_note":
            guard let key = params["key"] as? String,
                  let value = params["value"] as? String else {
                return errorResult("missing required params: key, value")
            }
            let author = params["author"] as? String
            let ttl = (params["ttl_minutes"] as? Int) ?? (params["ttl_minutes"] as? Double).map { Int($0) }
            noteStore.set(key: key, value: value, author: author, ttlMinutes: ttl)
            return ["saved": true]

        case "get_note":
            guard let key = params["key"] as? String else {
                return errorResult("missing required param: key")
            }
            guard let note = noteStore.get(key: key) else {
                return ["found": false]
            }
            var result: [String: Any] = ["key": note.key, "value": note.value]
            if let author = note.author { result["author"] = author }
            if let exp = note.expiresInSeconds { result["expires_in_seconds"] = exp }
            return result

        case "list_notes":
            return noteStore.allActive().map { note -> [String: Any] in
                var r: [String: Any] = ["key": note.key, "value": note.value]
                if let a = note.author { r["author"] = a }
                if let e = note.expiresInSeconds { r["expires_in_seconds"] = e }
                return r
            }

        default:
            return errorResult("unknown tool: \(tool)")
        }
    }

    private func makeTool(_ name: String, _ description: String, required: [String], optional: [String]) -> [String: Any] {
        var properties: [String: Any] = [:]
        for key in required { properties[key] = ["type": "string"] }
        for key in optional { properties[key] = ["type": "string"] }
        return [
            "name": name,
            "description": description,
            "inputSchema": [
                "type": "object",
                "properties": properties,
                "required": required
            ]
        ]
    }

    private func errorResult(_ message: String) -> [String: Any] {
        ["error": message]
    }
}
