import XCTest
@testable import agentpeek

final class NoteStoreTests: XCTestCase {
    private var store: NoteStore!
    private var defaults: UserDefaults!

    override func setUp() {
        defaults = UserDefaults(suiteName: "test.agentpeek.\(UUID().uuidString)")
        store = NoteStore(defaults: defaults)
    }

    func testSetGetRoundtrip() {
        store.set(key: "k", value: "v", author: "A", ttlMinutes: 5)
        let note = store.get(key: "k")
        XCTAssertEqual(note?.value, "v")
        XCTAssertEqual(note?.author, "A")
    }

    func testGetExpiredNoteReturnsNil() {
        // A negative TTL puts the expiry in the past, so the note is born expired.
        store.set(key: "k", value: "v", author: nil, ttlMinutes: -1)
        XCTAssertNil(store.get(key: "k"))
    }

    func testDeleteRemovesNote() {
        store.set(key: "k", value: "v", author: nil, ttlMinutes: nil)
        store.delete(key: "k")
        XCTAssertNil(store.get(key: "k"))
    }

    func testPersistenceAcrossInstances() {
        store.set(key: "k", value: "v", author: "A", ttlMinutes: 5)
        let reloaded = NoteStore(defaults: defaults)
        let note = reloaded.get(key: "k")
        XCTAssertEqual(note?.value, "v")
        XCTAssertEqual(note?.author, "A")
    }
}
