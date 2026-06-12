import Foundation

struct Note: Codable {
    let key: String
    let value: String
    let author: String?
    let expires: Date?

    var isExpired: Bool { expires.map { $0 < Date() } ?? false }
    var expiresInSeconds: Int? { expires.map { max(0, Int($0.timeIntervalSinceNow)) } }
}

final class NoteStore {
    private let queue = DispatchQueue(label: "com.agentpeek.notes")
    private var notes: [String: Note] = [:]
    private let defaultsKey = "agentpeek.notes"
    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        load()
    }

    func set(key: String, value: String, author: String?, ttlMinutes: Int?) {
        queue.sync {
            notes[key] = Note(
                key: key,
                value: value,
                author: author,
                expires: ttlMinutes.map { Date().addingTimeInterval(Double($0) * 60) }
            )
            save()
        }
    }

    func get(key: String) -> Note? {
        queue.sync {
            guard let note = notes[key], !note.isExpired else { return nil }
            return note
        }
    }

    func delete(key: String) {
        queue.sync {
            notes.removeValue(forKey: key)
            save()
        }
    }

    func allActive() -> [Note] {
        queue.sync {
            pruneExpired()
            return Array(notes.values)
        }
    }

    private func pruneExpired() {
        let before = notes.count
        notes = notes.filter { !$0.value.isExpired }
        if notes.count != before { save() }
    }

    private func save() {
        let data = try? JSONEncoder().encode(notes)
        defaults.set(data, forKey: defaultsKey)
    }

    private func load() {
        guard let data = defaults.data(forKey: defaultsKey),
              let decoded = try? JSONDecoder().decode([String: Note].self, from: data) else { return }
        notes = decoded
    }
}
