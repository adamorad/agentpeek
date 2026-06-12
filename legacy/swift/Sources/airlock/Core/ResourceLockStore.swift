import Foundation

enum LockError: Error {
    case heldBy(agentId: String, expiresIn: Int)
    case notFound
    case notOwned
}

struct ResourceLock: Codable {
    let name: String
    let agentId: String
    let expires: Date

    var isExpired: Bool { expires < Date() }
    var expiresInSeconds: Int { max(0, Int(expires.timeIntervalSinceNow)) }
}

final class ResourceLockStore {
    private let queue = DispatchQueue(label: "com.airlock.locks")
    private var locks: [String: ResourceLock] = [:]
    private let defaultsKey = "airlock.locks"
    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        load()
    }

    func lock(name: String, agentId: String, ttlMinutes: Int) throws {
        guard ttlMinutes > 0 else { return }
        try queue.sync {
            pruneExpired()
            if let existing = locks[name], !existing.isExpired {
                throw LockError.heldBy(agentId: existing.agentId, expiresIn: existing.expiresInSeconds)
            }
            locks[name] = ResourceLock(
                name: name,
                agentId: agentId,
                expires: Date().addingTimeInterval(Double(ttlMinutes) * 60)
            )
            save()
        }
    }

    func unlock(name: String, agentId: String) {
        queue.sync {
            guard let existing = locks[name], existing.agentId == agentId else { return }
            locks.removeValue(forKey: name)
            save()
        }
    }

    func renew(name: String, agentId: String, ttlMinutes: Int) throws {
        guard ttlMinutes > 0 else { return }
        try queue.sync {
            guard let existing = locks[name], !existing.isExpired else {
                throw LockError.notFound
            }
            guard existing.agentId == agentId else {
                throw LockError.notOwned
            }
            locks[name] = ResourceLock(
                name: name,
                agentId: agentId,
                expires: Date().addingTimeInterval(Double(ttlMinutes) * 60)
            )
            save()
        }
    }

    func allActiveLocks() -> [ResourceLock] {
        queue.sync {
            pruneExpired()
            return Array(locks.values)
        }
    }

    private func pruneExpired() {
        let before = locks.count
        locks = locks.filter { !$0.value.isExpired }
        if locks.count != before { save() }
    }

    private func save() {
        let data = try? JSONEncoder().encode(locks)
        defaults.set(data, forKey: defaultsKey)
    }

    private func load() {
        guard let data = defaults.data(forKey: defaultsKey),
              let decoded = try? JSONDecoder().decode([String: ResourceLock].self, from: data) else { return }
        locks = decoded
    }
}
