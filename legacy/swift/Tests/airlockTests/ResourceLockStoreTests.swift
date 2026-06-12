import XCTest
@testable import airlock

final class ResourceLockStoreTests: XCTestCase {
    private var store: ResourceLockStore!
    private var defaults: UserDefaults!

    override func setUp() {
        defaults = UserDefaults(suiteName: "test.airlock.\(UUID().uuidString)")
        store = ResourceLockStore(defaults: defaults)
    }

    func testLockThenContend() throws {
        try store.lock(name: "r", agentId: "A", ttlMinutes: 5)
        XCTAssertThrowsError(try store.lock(name: "r", agentId: "B", ttlMinutes: 5)) { error in
            guard case LockError.heldBy(let holder, _) = error else { return XCTFail("wrong error") }
            XCTAssertEqual(holder, "A")
        }
    }

    func testUnlockNotOwnerIsNoop() throws {
        try store.lock(name: "r", agentId: "A", ttlMinutes: 5)
        store.unlock(name: "r", agentId: "B")
        XCTAssertEqual(store.allActiveLocks().count, 1)
    }

    func testRenewNotOwnedThrows() throws {
        try store.lock(name: "r", agentId: "A", ttlMinutes: 5)
        XCTAssertThrowsError(try store.renew(name: "r", agentId: "B", ttlMinutes: 5))
    }

    func testRenewMissingThrowsNotFound() {
        XCTAssertThrowsError(try store.renew(name: "ghost", agentId: "A", ttlMinutes: 5)) { error in
            guard case LockError.notFound = error else { return XCTFail("expected notFound") }
        }
    }

    func testZeroTTLDoesNotAcquire() throws {
        // Regression: zero-TTL "lock" must not exist afterwards.
        try? store.lock(name: "r", agentId: "A", ttlMinutes: 0)
        XCTAssertTrue(store.allActiveLocks().isEmpty)
    }

    func testPersistenceAcrossInstances() throws {
        try store.lock(name: "r", agentId: "A", ttlMinutes: 5)
        let reloaded = ResourceLockStore(defaults: defaults)
        XCTAssertEqual(reloaded.allActiveLocks().first?.agentId, "A")
    }
}
