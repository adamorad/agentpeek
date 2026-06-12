package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// Sentinel errors returned by the lock manager.
var (
	// ErrInvalidTTL is returned when a caller requests a lock with a
	// non-positive ttlSeconds. Defensive: a zero/negative TTL would mint a
	// lock that is born expired.
	ErrInvalidTTL = errors.New("store: ttlSeconds must be > 0")
	// ErrNotOwned is returned by Renew when the supplied lock_token does not
	// match the live lock's token (capability check failed).
	ErrNotOwned = errors.New("store: lock not owned by supplied token")
	// ErrNotFound is returned by Renew when there is no live lock with the
	// given name (it never existed or already expired).
	ErrNotFound = errors.New("store: lock not found")
)

// maxWaitSeconds caps a blocking wait. Claude Code aborts MCP tool calls at
// ~60s; 50s leaves headroom so we always return a structured wake_token result
// rather than letting the client time the call out.
const maxWaitSeconds = 50

// reaperInterval is how often the background reaper runs: expire locks, GC
// abandoned waiters, and wake the head of any now-free queue.
const reaperInterval = 250 * time.Millisecond

// LockInfo is a snapshot of a single held lock, returned by ListLocks.
type LockInfo struct {
	Name             string
	AgentID          string
	ExpiresInSeconds int
}

// LockResult is the outcome of a Lock / Renew attempt.
type LockResult struct {
	Locked           bool
	LockToken        string // set when Locked
	ExpiresInSeconds int    // set when Locked
	HeldBy           string // set when !Locked
	WakeToken        string // set when !Locked and the caller waited/was enqueued
	QueuePosition    int    // 1-based FIFO position when !Locked (1 = next in line)
	RetryWith        string // human/agent instruction to keep place in line
}

// waiter is one in-memory slot in a lock's FIFO wait queue. It survives across
// separate HTTP requests: when a blocking Lock call times out, its waiter stays
// enqueued keyed by wakeToken, and a re-poll with that token re-attaches to this
// same slot (idempotent — no double-enqueue, no FIFO position loss).
type waiter struct {
	wakeToken string
	agentID   string
	// ch is signalled (a value sent, or closed) to wake this waiter. Buffered
	// with cap 1 so a signaller never blocks and a coalesced signal is not lost
	// if the waiter is between blocking receives (cross-request).
	ch        chan struct{}
	expiresAt int64 // Unix seconds; waiter is GC'd by the reaper once past this
}

// lockQueue is the in-memory FIFO of waiters for a single lock name.
type lockQueue struct {
	waiters []*waiter
}

// LockManager coordinates DB-backed locks plus in-memory blocking wait queues.
//
// The DB row (table locks) is the single source of truth for who holds a lock;
// acquisition is an atomic guarded conditional write inside a tx. The in-memory
// queues only decide ordering/wakeups for blocked callers and never themselves
// grant a lock — a woken waiter always re-checks the DB under a tx.
type LockManager struct {
	s *Store

	mu     sync.Mutex            // guards queues
	queues map[string]*lockQueue // by lock name; entries pruned when empty
}

// NewLockManager constructs a LockManager over the given store. Call Start to
// launch the background reaper.
func NewLockManager(s *Store) *LockManager {
	return &LockManager{
		s:      s,
		queues: make(map[string]*lockQueue),
	}
}

// Start launches the reaper goroutine, which runs until ctx is cancelled.
func (m *LockManager) Start(ctx context.Context) {
	go m.reapLoop(ctx)
}

func (m *LockManager) reapLoop(ctx context.Context) {
	t := time.NewTicker(reaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reapOnce()
		}
	}
}

// reapOnce deletes expired lock rows, GCs abandoned waiters, and signals the
// head of every queue whose lock is currently free.
func (m *LockManager) reapOnce() {
	now := Now()

	// 1. Delete expired lock rows. This is best-effort; lazy expiry on acquire
	// is the correctness guarantee, the reaper just keeps the table tidy and
	// frees slots promptly for blocked waiters.
	_ = m.s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM locks WHERE expires_at <= ?", now)
		return err
	})

	// 2. GC abandoned waiters (past expiresAt) and signal the head of any free
	// queue. We snapshot the names under the lock, then query DB freedom
	// without holding mu (never hold mu across DB I/O is not strictly required
	// here since tx doesn't block on channels, but we keep the critical section
	// tight regardless).
	m.mu.Lock()
	for name, q := range m.queues {
		// Drop expired waiters wherever they sit in the FIFO.
		kept := q.waiters[:0]
		for _, w := range q.waiters {
			if w.expiresAt > now {
				kept = append(kept, w)
			}
		}
		q.waiters = kept
		if len(q.waiters) == 0 {
			delete(m.queues, name)
		}
	}
	// Collect the set of (name, headWaiter) to consider for waking.
	type headEntry struct {
		name string
		head *waiter
	}
	var heads []headEntry
	for name, q := range m.queues {
		if len(q.waiters) > 0 {
			heads = append(heads, headEntry{name: name, head: q.waiters[0]})
		}
	}
	m.mu.Unlock()

	// For each head, if the lock is currently free, signal it. Signalling is
	// coalesced (buffered cap-1 channel) so repeated reaper ticks are harmless.
	// The lockIsFree read here is un-transacted and advisory only: it merely
	// decides whether to wake the head. The woken waiter re-validates freedom
	// under a tx in tryAcquire before it can ever hold the lock, so a stale read
	// here causes at most a spurious wake (the waiter re-blocks), never a
	// double-grant.
	for _, h := range heads {
		if m.lockIsFree(h.name, now) {
			signal(h.head)
		}
	}
}

// lockIsFree reports whether the named lock has no live holder right now (no
// row, or the row is expired).
func (m *LockManager) lockIsFree(name string, now int64) bool {
	var expiresAt int64
	err := m.s.DB.QueryRow("SELECT expires_at FROM locks WHERE name = ?", name).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil {
		return false
	}
	return expiresAt <= now
}

// signal wakes a waiter without blocking. The channel is buffered (cap 1); if a
// signal is already pending we coalesce (the default branch drops the dup).
func signal(w *waiter) {
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

// Lock attempts to acquire the named lock for agentID with the given ttl. When
// waitSeconds > 0 the call blocks up to waitSeconds (clamped to [0,50]) for the
// lock to become available, honouring ctx cancellation. See package docs for the
// wake-token re-poll protocol.
func (m *LockManager) Lock(ctx context.Context, name, agentID string, ttlSeconds, waitSeconds int, wakeToken string) (LockResult, error) {
	if ttlSeconds <= 0 {
		return LockResult{}, ErrInvalidTTL
	}
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	if waitSeconds > maxWaitSeconds {
		waitSeconds = maxWaitSeconds
	}

	// First attempt: try to acquire immediately regardless of wait mode.
	res, acquired, holder, err := m.tryAcquire(name, agentID, ttlSeconds)
	if err != nil {
		return LockResult{}, err
	}
	if acquired {
		return res, nil
	}

	// Non-blocking: report held, no wake token.
	if waitSeconds == 0 {
		return LockResult{
			Locked:           false,
			HeldBy:           holder.agentID,
			ExpiresInSeconds: holder.remaining,
		}, nil
	}

	// Blocking: enqueue (or re-attach to existing slot) and wait.
	w := m.enqueue(name, agentID, wakeToken, ttlSeconds, waitSeconds)

	deadline := time.NewTimer(time.Duration(waitSeconds) * time.Second)
	defer deadline.Stop()

	for {
		// Drain any pending signal then attempt acquire. We only return Locked
		// if WE are the FIFO head and the DB grants it — non-head waiters that
		// happen to get woken just re-block.
		if m.isHead(name, w) {
			res, acquired, holder, err := m.tryAcquire(name, agentID, ttlSeconds)
			if err != nil {
				return LockResult{}, err
			}
			if acquired {
				m.dequeue(name, w)
				return res, nil
			}
			_ = holder
		}

		select {
		case <-ctx.Done():
			// Client disconnected. Keep the slot — a re-poll resumes it.
			return m.notAcquiredResult(name, agentID, w), nil
		case <-deadline.C:
			// Timed out. Keep the slot; return wake token so the caller re-polls.
			return m.notAcquiredResult(name, agentID, w), nil
		case <-w.ch:
			// Woken (by reaper / unlock / release). Loop and re-attempt.
		}
	}
}

// holderInfo describes the live holder of a lock for !Locked results.
type holderInfo struct {
	agentID   string
	remaining int
}

// tryAcquire performs an atomic acquire attempt in a single tx. A lock is free
// if there is no row OR the existing row is expired (treated as free and
// overwritten). The guarded conditional write makes this safe under concurrent
// callers given SetMaxOpenConns(1): the whole read-modify-write runs inside one
// serialized tx, so two goroutines cannot both observe "free" and both insert.
func (m *LockManager) tryAcquire(name, agentID string, ttlSeconds int) (res LockResult, acquired bool, holder holderInfo, err error) {
	now := Now()
	token, terr := newToken()
	if terr != nil {
		return LockResult{}, false, holderInfo{}, terr
	}
	expiresAt := now + int64(ttlSeconds)

	txErr := m.s.tx(func(tx *sql.Tx) error {
		var (
			rowAgent   string
			rowExpires int64
		)
		qerr := tx.QueryRow(
			"SELECT agent_id, expires_at FROM locks WHERE name = ?", name,
		).Scan(&rowAgent, &rowExpires)

		switch {
		case errors.Is(qerr, sql.ErrNoRows):
			// Free: insert fresh.
			if _, e := tx.Exec(
				"INSERT INTO locks(name, agent_id, lock_token, acquired_at, expires_at) VALUES(?,?,?,?,?)",
				name, agentID, token, now, expiresAt,
			); e != nil {
				return e
			}
			acquired = true
		case qerr != nil:
			return qerr
		case rowExpires <= now:
			// Expired holder: overwrite. Guard on the stale expiry so a
			// concurrent fresh acquire (impossible under 1 conn, but correct
			// regardless) cannot be clobbered.
			if _, e := tx.Exec(
				"UPDATE locks SET agent_id=?, lock_token=?, acquired_at=?, expires_at=? WHERE name=? AND expires_at<=?",
				agentID, token, now, expiresAt, name, now,
			); e != nil {
				return e
			}
			acquired = true
		default:
			// Live holder: not acquired.
			holder = holderInfo{agentID: rowAgent, remaining: secsLeft(rowExpires, now)}
		}
		return nil
	})
	if txErr != nil {
		return LockResult{}, false, holderInfo{}, txErr
	}

	if acquired {
		return LockResult{
			Locked:           true,
			LockToken:        token,
			ExpiresInSeconds: ttlSeconds,
		}, true, holderInfo{}, nil
	}
	return LockResult{}, false, holder, nil
}

// waiterGCSlack is extra headroom (seconds) added on top of the wait window
// when computing a parked waiter's GC horizon, so a well-behaved client that
// re-polls right at the edge of its wait window is never GC'd out from under
// itself before the re-poll lands.
const waiterGCSlack = 30

// waiterHorizon returns the Unix-seconds GC horizon for a parked waiter. The
// horizon must comfortably exceed the maximum time a well-behaved client can be
// away between polls — the wait window — NOT the lock TTL (a short TTL with a
// long wait must not let the reaper drop the slot before the next re-poll). We
// take the larger of 2×TTL (legacy slack for tiny waits) and waitSeconds+slack.
// Note widen-before-multiply: 2*int64(ttlSeconds), not int64(2*ttlSeconds).
func waiterHorizon(now int64, ttlSeconds, waitSeconds int) int64 {
	byTTL := 2 * int64(ttlSeconds)
	byWait := int64(waitSeconds) + waiterGCSlack
	h := byTTL
	if byWait > h {
		h = byWait
	}
	return now + h
}

// enqueue appends a new waiter or re-attaches to an existing slot keyed by
// wakeToken (idempotent re-poll). The waiter's GC horizon is based on the wait
// window (see waiterHorizon) so a caller that re-polls within its wait window
// retains its FIFO place; abandoned slots are reaped.
func (m *LockManager) enqueue(name, agentID, wakeToken string, ttlSeconds, waitSeconds int) *waiter {
	m.mu.Lock()
	defer m.mu.Unlock()

	q := m.queues[name]
	if q == nil {
		q = &lockQueue{}
		m.queues[name] = q
	}

	expiresAt := waiterHorizon(Now(), ttlSeconds, waitSeconds)

	// Idempotent re-poll: if wakeToken matches an existing waiter, refresh and
	// reuse it (keep FIFO position).
	if wakeToken != "" {
		for _, w := range q.waiters {
			if w.wakeToken == wakeToken {
				w.expiresAt = expiresAt
				return w
			}
		}
	}

	// New waiter: append to the tail.
	w := &waiter{
		wakeToken: mustToken(),
		agentID:   agentID,
		ch:        make(chan struct{}, 1),
		expiresAt: expiresAt,
	}
	q.waiters = append(q.waiters, w)
	return w
}

// isHead reports whether w is currently the FIFO head of name's queue.
func (m *LockManager) isHead(name string, w *waiter) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[name]
	if q == nil || len(q.waiters) == 0 {
		return false
	}
	return q.waiters[0] == w
}

// dequeue removes w from name's queue (called once it has won the lock).
func (m *LockManager) dequeue(name string, w *waiter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[name]
	if q == nil {
		return
	}
	kept := q.waiters[:0]
	for _, x := range q.waiters {
		if x != w {
			kept = append(kept, x)
		}
	}
	q.waiters = kept
	if len(q.waiters) == 0 {
		delete(m.queues, name)
	}
}

// notAcquiredResult builds the !Locked result for a waiter that is staying in
// the queue (timeout or ctx cancel), including its current FIFO position and the
// holder for HeldBy.
func (m *LockManager) notAcquiredResult(name, agentID string, w *waiter) LockResult {
	now := Now()

	// Current holder (if any) for HeldBy / ExpiresInSeconds.
	var heldBy string
	var remaining int
	var (
		rowAgent   string
		rowExpires int64
	)
	err := m.s.DB.QueryRow(
		"SELECT agent_id, expires_at FROM locks WHERE name = ?", name,
	).Scan(&rowAgent, &rowExpires)
	if err == nil && rowExpires > now {
		heldBy = rowAgent
		remaining = secsLeft(rowExpires, now)
	}

	// FIFO position, read under mu so it reflects the live queue.
	pos := m.queuePosition(name, w)

	// Never advertise a live WakeToken/RetryWith with QueuePosition 0: if the
	// waiter has been GC'd or dequeued between holder read and position read, the
	// slot is gone, so returning its wake_token would tell the client "you're
	// queued, re-poll with this token" when there is nothing to re-attach to. In
	// that case omit the token (a re-poll with an unknown token re-enqueues at the
	// tail anyway). A brief HeldBy staleness is fine; a live-token-with-position-0
	// is not.
	if pos < 1 {
		return LockResult{
			Locked:           false,
			HeldBy:           heldBy,
			ExpiresInSeconds: remaining,
		}
	}

	return LockResult{
		Locked:           false,
		HeldBy:           heldBy,
		ExpiresInSeconds: remaining,
		WakeToken:        w.wakeToken,
		QueuePosition:    pos,
		RetryWith:        "call lock_resource again with wake_token=" + w.wakeToken + " to keep your place in line",
	}
}

// queuePosition returns the 1-based FIFO position of w in name's queue, or 0 if
// it is no longer present.
func (m *LockManager) queuePosition(name string, w *waiter) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[name]
	if q == nil {
		return 0
	}
	for i, x := range q.waiters {
		if x == w {
			return i + 1
		}
	}
	return 0
}

// LockMany acquires all of names atomically (all-or-nothing). Names are sorted
// ascending first so any two callers requesting overlapping sets acquire in the
// same global order, eliminating the classic two-lock deadlock. For v1 this is
// non-blocking: waitSeconds is accepted for signature stability but a single
// immediate attempt is made (no enqueue). If any name is held by a live lock,
// none are acquired.
func (m *LockManager) LockMany(ctx context.Context, names []string, agentID string, ttlSeconds, waitSeconds int) (acquired bool, tokens map[string]string, heldBy string, err error) {
	if ttlSeconds <= 0 {
		return false, nil, "", ErrInvalidTTL
	}
	if len(names) == 0 {
		return true, map[string]string{}, "", nil
	}

	// Deduplicate + globally order.
	sorted := dedupeSorted(names)

	now := Now()
	expiresAt := now + int64(ttlSeconds)
	result := make(map[string]string, len(sorted))

	txErr := m.s.tx(func(tx *sql.Tx) error {
		// Phase 1: check every name is free.
		for _, n := range sorted {
			var rowAgent string
			var rowExpires int64
			qerr := tx.QueryRow(
				"SELECT agent_id, expires_at FROM locks WHERE name = ?", n,
			).Scan(&rowAgent, &rowExpires)
			switch {
			case errors.Is(qerr, sql.ErrNoRows):
				// free
			case qerr != nil:
				return qerr
			case rowExpires > now:
				// Live holder blocks the whole batch.
				heldBy = rowAgent
				acquired = false
				return errAbortBatch
			default:
				// expired: treated as free, will be overwritten in phase 2
			}
		}

		// Phase 2: acquire all (insert or overwrite-expired).
		for _, n := range sorted {
			tok, terr := newToken()
			if terr != nil {
				return terr
			}
			if _, e := tx.Exec(
				"INSERT INTO locks(name, agent_id, lock_token, acquired_at, expires_at) VALUES(?,?,?,?,?) "+
					"ON CONFLICT(name) DO UPDATE SET agent_id=excluded.agent_id, lock_token=excluded.lock_token, "+
					"acquired_at=excluded.acquired_at, expires_at=excluded.expires_at",
				n, agentID, tok, now, expiresAt,
			); e != nil {
				return e
			}
			result[n] = tok
		}
		acquired = true
		return nil
	})

	if txErr != nil && !errors.Is(txErr, errAbortBatch) {
		return false, nil, "", txErr
	}
	if !acquired {
		return false, nil, heldBy, nil
	}
	return true, result, "", nil
}

// errAbortBatch is an internal sentinel used to roll back a LockMany tx when a
// name is already held; it is never surfaced to callers.
var errAbortBatch = errors.New("store: lockmany batch aborted (name held)")

// Unlock releases the named lock. Authorization: either lockToken matches the
// row's token (capability), or (v1 compat) lockToken is empty and agentID
// matches the row's agent_id. On success the row is deleted and the queue head
// woken. Not held / not authorized is a no-op (released=false, nil error).
func (m *LockManager) Unlock(name, lockToken, agentID string) (released bool, err error) {
	txErr := m.s.tx(func(tx *sql.Tx) error {
		var (
			rowToken string
			rowAgent string
		)
		qerr := tx.QueryRow(
			"SELECT lock_token, agent_id FROM locks WHERE name = ?", name,
		).Scan(&rowToken, &rowAgent)
		if errors.Is(qerr, sql.ErrNoRows) {
			return nil // not held: no-op
		}
		if qerr != nil {
			return qerr
		}

		authorized := (lockToken != "" && lockToken == rowToken) ||
			(lockToken == "" && agentID != "" && agentID == rowAgent)
		if !authorized {
			return nil // not owner: no-op
		}

		if _, e := tx.Exec("DELETE FROM locks WHERE name = ?", name); e != nil {
			return e
		}
		released = true
		return nil
	})
	if txErr != nil {
		return false, txErr
	}
	if released {
		m.wakeHead(name)
	}
	return released, nil
}

// Renew extends the named lock's expiry by ttlSeconds from now, requiring a
// matching lock_token. Returns ErrInvalidTTL for bad ttl, ErrNotFound if there
// is no live lock, ErrNotOwned if the token does not match.
func (m *LockManager) Renew(name, lockToken string, ttlSeconds int) (LockResult, error) {
	if ttlSeconds <= 0 {
		return LockResult{}, ErrInvalidTTL
	}
	now := Now()
	expiresAt := now + int64(ttlSeconds)

	var outErr error
	txErr := m.s.tx(func(tx *sql.Tx) error {
		var (
			rowToken   string
			rowExpires int64
		)
		qerr := tx.QueryRow(
			"SELECT lock_token, expires_at FROM locks WHERE name = ?", name,
		).Scan(&rowToken, &rowExpires)
		if errors.Is(qerr, sql.ErrNoRows) {
			outErr = ErrNotFound
			return nil
		}
		if qerr != nil {
			return qerr
		}
		if rowExpires <= now {
			// Already expired — treat as not found (a renew can't resurrect).
			outErr = ErrNotFound
			return nil
		}
		if lockToken == "" || lockToken != rowToken {
			outErr = ErrNotOwned
			return nil
		}
		if _, e := tx.Exec(
			"UPDATE locks SET expires_at = ? WHERE name = ?", expiresAt, name,
		); e != nil {
			return e
		}
		return nil
	})
	if txErr != nil {
		return LockResult{}, txErr
	}
	if outErr != nil {
		return LockResult{}, outErr
	}
	return LockResult{
		Locked:           true,
		LockToken:        lockToken,
		ExpiresInSeconds: ttlSeconds,
	}, nil
}

// RenewByAgent is the v1-compatible renew path: it extends the named lock by
// ttlSeconds only if the live holder's agent_id matches agentID (capability via
// identity rather than via lock_token). Same error contract as Renew:
// ErrInvalidTTL for bad ttl, ErrNotFound if there is no live lock, ErrNotOwned
// if the holder is a different agent. Prefer Renew (token-based) for new code.
func (m *LockManager) RenewByAgent(name, agentID string, ttlSeconds int) (LockResult, error) {
	if ttlSeconds <= 0 {
		return LockResult{}, ErrInvalidTTL
	}
	now := Now()
	expiresAt := now + int64(ttlSeconds)

	var outErr error
	txErr := m.s.tx(func(tx *sql.Tx) error {
		var (
			rowAgent   string
			rowExpires int64
		)
		qerr := tx.QueryRow(
			"SELECT agent_id, expires_at FROM locks WHERE name = ?", name,
		).Scan(&rowAgent, &rowExpires)
		if errors.Is(qerr, sql.ErrNoRows) {
			outErr = ErrNotFound
			return nil
		}
		if qerr != nil {
			return qerr
		}
		if rowExpires <= now {
			// Already expired — a renew cannot resurrect it.
			outErr = ErrNotFound
			return nil
		}
		if agentID == "" || agentID != rowAgent {
			outErr = ErrNotOwned
			return nil
		}
		if _, e := tx.Exec(
			"UPDATE locks SET expires_at = ? WHERE name = ?", expiresAt, name,
		); e != nil {
			return e
		}
		return nil
	})
	if txErr != nil {
		return LockResult{}, txErr
	}
	if outErr != nil {
		return LockResult{}, outErr
	}
	return LockResult{
		Locked:           true,
		ExpiresInSeconds: ttlSeconds,
	}, nil
}

// ListLocks returns a snapshot of all currently live (non-expired) locks.
func (m *LockManager) ListLocks() ([]LockInfo, error) {
	now := Now()
	rows, err := m.s.DB.Query(
		"SELECT name, agent_id, expires_at FROM locks WHERE expires_at > ? ORDER BY name", now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LockInfo
	for rows.Next() {
		var (
			name    string
			agent   string
			expires int64
		)
		if err := rows.Scan(&name, &agent, &expires); err != nil {
			return nil, err
		}
		out = append(out, LockInfo{
			Name:             name,
			AgentID:          agent,
			ExpiresInSeconds: secsLeft(expires, now),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ReleaseAgentLocks deletes every lock held by agentID and wakes the head of
// each affected queue. Used by presence expiry. Returns the number deleted.
func (m *LockManager) ReleaseAgentLocks(agentID string) (count int, err error) {
	var freed []string
	txErr := m.s.tx(func(tx *sql.Tx) error {
		rows, qerr := tx.Query("SELECT name FROM locks WHERE agent_id = ?", agentID)
		if qerr != nil {
			return qerr
		}
		var names []string
		for rows.Next() {
			var n string
			if e := rows.Scan(&n); e != nil {
				rows.Close()
				return e
			}
			names = append(names, n)
		}
		if e := rows.Err(); e != nil {
			rows.Close()
			return e
		}
		rows.Close()

		if len(names) == 0 {
			return nil
		}
		if _, e := tx.Exec("DELETE FROM locks WHERE agent_id = ?", agentID); e != nil {
			return e
		}
		freed = names
		count = len(names)
		return nil
	})
	if txErr != nil {
		return 0, txErr
	}
	for _, n := range freed {
		m.wakeHead(n)
	}
	return count, nil
}

// wakeHead signals the FIFO head of name's queue, if any.
func (m *LockManager) wakeHead(name string) {
	m.mu.Lock()
	var head *waiter
	if q := m.queues[name]; q != nil && len(q.waiters) > 0 {
		head = q.waiters[0]
	}
	m.mu.Unlock()
	if head != nil {
		signal(head)
	}
}

// --- helpers ---

// newToken mints a fresh 128-bit random hex token via crypto/rand.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// mustToken mints a token, panicking only if the OS CSPRNG fails (catastrophic;
// the process cannot do meaningful security work without entropy).
func mustToken() string {
	t, err := newToken()
	if err != nil {
		panic("store: crypto/rand failed: " + err.Error())
	}
	return t
}

// secsLeft returns whole seconds remaining until expiresAt (>= 0).
func secsLeft(expiresAt, now int64) int {
	d := expiresAt - now
	if d < 0 {
		d = 0
	}
	return int(d)
}

// dedupeSorted returns names sorted ascending with duplicates removed.
func dedupeSorted(names []string) []string {
	cp := append([]string(nil), names...)
	sort.Strings(cp)
	out := cp[:0]
	var last string
	first := true
	for _, n := range cp {
		if first || n != last {
			out = append(out, n)
			last = n
			first = false
		}
	}
	return out
}
