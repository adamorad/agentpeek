package store

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// presenceReaperInterval is how often the presence reaper runs: find expired
// agents, release their locks, and delete their rows. ~1s is frequent enough to
// recover a crashed agent's locks promptly without busy-spinning the DB.
const presenceReaperInterval = time.Second

// AgentInfo is a snapshot of a single registered, non-expired agent.
type AgentInfo struct {
	AgentID          string
	ExpiresInSeconds int
}

// PresenceManager tracks live agents via a heartbeat (TTL) in the agents table
// and runs a background reaper that, on expiry, releases the dead agent's locks
// before deleting its row. This is the crash-recovery path: an agent that dies
// without calling Unregister has its locks freed when its presence TTL lapses,
// rather than lingering until each lock's own (typically longer) TTL.
type PresenceManager struct {
	s     *Store
	locks *LockManager

	wg sync.WaitGroup // tracks the reaper goroutine so Wait can join it
}

// NewPresenceManager constructs a PresenceManager over the given store and lock
// manager. Call Start to launch the background expiry reaper.
func NewPresenceManager(s *Store, locks *LockManager) *PresenceManager {
	return &PresenceManager{s: s, locks: locks}
}

// Start launches the expiry reaper goroutine, which runs until ctx is cancelled.
func (p *PresenceManager) Start(ctx context.Context) {
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.reapLoop(ctx) }()
}

// Wait blocks until the reaper goroutine has returned (after ctx is cancelled).
// Call it during shutdown before closing the store so no reaper is mid-query
// when the DB closes.
func (p *PresenceManager) Wait() { p.wg.Wait() }

func (p *PresenceManager) reapLoop(ctx context.Context) {
	t := time.NewTicker(presenceReaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.reapOnce()
		}
	}
}

// reapOnce finds every agent whose TTL has lapsed and, for each, releases its
// locks (waking any waiters) and deletes its presence row. Errors are
// best-effort: a transient failure just leaves the agent to be reaped on the
// next tick.
func (p *PresenceManager) reapOnce() {
	now := Now()

	var expired []string
	rows, err := p.s.DB.Query("SELECT agent_id FROM agents WHERE expires_at <= ?", now)
	if err != nil {
		return
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		expired = append(expired, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}

	for _, id := range expired {
		// Release the dead agent's locks first (frees them + wakes waiters),
		// then remove the presence row. Order matters only for tidiness; both
		// are idempotent.
		_, _ = p.locks.ReleaseAgentLocks(id)
		_ = p.s.tx(func(tx *sql.Tx) error {
			_, e := tx.Exec("DELETE FROM agents WHERE agent_id = ?", id)
			return e
		})
	}
}

// Register registers agentID with a heartbeat TTL of ttlSeconds, extending the
// expiry to Now()+ttlSeconds. On first registration registered_at is set to
// Now(); on a subsequent heartbeat (re-register) the original registered_at is
// preserved and only expires_at is extended. A non-positive ttlSeconds returns
// ErrInvalidTTL.
func (p *PresenceManager) Register(agentID string, ttlSeconds int) error {
	if ttlSeconds <= 0 {
		return ErrInvalidTTL
	}
	now := Now()
	expiresAt := now + int64(ttlSeconds)
	return p.s.tx(func(tx *sql.Tx) error {
		// Upsert: insert fresh with registered_at=now, or on conflict keep the
		// existing registered_at and only bump expires_at.
		_, e := tx.Exec(
			"INSERT INTO agents(agent_id, registered_at, expires_at) VALUES(?,?,?) "+
				"ON CONFLICT(agent_id) DO UPDATE SET expires_at=excluded.expires_at",
			agentID, now, expiresAt,
		)
		return e
	})
}

// Unregister is a graceful exit: it removes the agent's presence row AND
// releases all locks the agent holds (waking any waiters). Unknown agents are a
// no-op.
func (p *PresenceManager) Unregister(agentID string) error {
	if _, err := p.locks.ReleaseAgentLocks(agentID); err != nil {
		return err
	}
	return p.s.tx(func(tx *sql.Tx) error {
		_, e := tx.Exec("DELETE FROM agents WHERE agent_id = ?", agentID)
		return e
	})
}

// ListAgents returns a snapshot of all currently registered, non-expired agents
// ordered by agent_id. Expired agents are filtered lazily (expires_at > Now())
// so the result is correct even before the reaper has deleted them.
func (p *PresenceManager) ListAgents() ([]AgentInfo, error) {
	now := Now()
	rows, err := p.s.DB.Query(
		"SELECT agent_id, expires_at FROM agents WHERE expires_at > ? ORDER BY agent_id", now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AgentInfo
	for rows.Next() {
		var (
			id      string
			expires int64
		)
		if err := rows.Scan(&id, &expires); err != nil {
			return nil, err
		}
		out = append(out, AgentInfo{
			AgentID:          id,
			ExpiresInSeconds: secsLeft(expires, now),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
