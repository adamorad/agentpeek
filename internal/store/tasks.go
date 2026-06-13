package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// taskReaperInterval is how often the requeue reaper runs: find claimed tasks
// whose lease has elapsed or whose claimant is no longer present, and return
// them to the pending pool. ~1s is frequent enough to recover a crashed
// consumer's task promptly without busy-spinning the DB.
const taskReaperInterval = time.Second

// defaultLeaseSeconds is the lease length applied when ClaimNext is called with
// a non-positive leaseSeconds. A claim is a lease, not a permanent assignment:
// if the consumer dies or stalls, the reaper requeues the task after this.
const defaultLeaseSeconds = 120

// Task state constants. A task moves pending -> claimed (via ClaimNext) and
// then either claimed -> done (Complete / Fail-giveup) or claimed -> pending
// (Fail-requeue / reaper).
const (
	taskPending = "pending"
	taskClaimed = "claimed"
	taskDone    = "done"
)

// Task is a snapshot of a single row in the tasks table. LeaseAgent and
// LeaseExpiresInSeconds are only meaningful while State == "claimed"; they are
// "" / 0 otherwise.
type Task struct {
	ID                    int64
	Queue                 string
	Payload               string
	Author                string // provenance, "" if none
	Priority              int
	State                 string // "pending" | "claimed" | "done"
	LeaseAgent            string // "" unless claimed
	LeaseExpiresInSeconds int    // 0 unless claimed
}

// TaskManager is a durable, lease-based work queue over the tasks table. Push
// enqueues pending work; ClaimNext atomically leases the next item to a
// consumer; Complete/Fail/RenewLease are capability-gated by the lease token.
//
// The point of the manager is the requeue reaper (Start): a claim is a lease,
// and a task is returned to pending if the lease elapses without renewal OR the
// claiming agent is no longer present (crash-orphan recovery). This guarantees
// at-least-once delivery — a consumer that dies mid-task does not strand it.
type TaskManager struct {
	s *Store

	wg sync.WaitGroup // tracks the reaper goroutine so Wait can join it
}

// NewTaskManager constructs a TaskManager over the given store. Call Start to
// launch the background requeue reaper.
func NewTaskManager(s *Store) *TaskManager {
	return &TaskManager{s: s}
}

// Start launches the requeue reaper goroutine, which runs until ctx is
// cancelled.
func (t *TaskManager) Start(ctx context.Context) {
	t.wg.Add(1)
	go func() { defer t.wg.Done(); t.reapLoop(ctx) }()
}

// Wait blocks until the reaper goroutine has returned (after ctx is cancelled).
// Call it during shutdown before closing the store so no reaper is mid-query
// when the DB closes.
func (t *TaskManager) Wait() { t.wg.Wait() }

func (t *TaskManager) reapLoop(ctx context.Context) {
	tk := time.NewTicker(taskReaperInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.reapOnce()
		}
	}
}

// reapOnce returns to the pending pool every claimed task whose lease has
// elapsed OR whose claimant is no longer present. Both conditions are checked
// in a single guarded UPDATE so the requeue is atomic w.r.t. concurrent
// Complete/Fail/RenewLease/ClaimNext (all serialized by SetMaxOpenConns(1)):
//
//   - lease_expires_at <= now            -> lease elapsed without renewal
//   - lease_agent NOT IN (present agents) -> claimant crashed / departed
//
// A task that satisfies either is reset: state='pending', lease columns
// cleared, making it claimable again. Errors are best-effort; a transient
// failure just leaves the task to be reaped on the next tick.
func (t *TaskManager) reapOnce() {
	now := Now()
	_ = t.s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE tasks
			   SET state='pending', lease_agent=NULL, lease_token=NULL, lease_expires_at=NULL
			 WHERE state='claimed'
			   AND (
			         lease_expires_at <= ?
			      OR lease_agent NOT IN (SELECT agent_id FROM agents WHERE expires_at > ?)
			   )`,
			now, now,
		)
		return err
	})
}

// Push inserts a new pending task into queue with the given payload, author
// (provenance, may be ""), and priority. created_at is set to Now(). It returns
// the new task's autoincrement id.
func (t *TaskManager) Push(queue, payload, author string, priority int) (id int64, err error) {
	now := Now()
	err = t.s.tx(func(tx *sql.Tx) error {
		res, e := tx.Exec(
			`INSERT INTO tasks(queue, payload, author, priority, state, created_at)
			 VALUES(?,?,?,?,?,?)`,
			queue, payload, author, priority, taskPending, now,
		)
		if e != nil {
			return e
		}
		id, e = res.LastInsertId()
		return e
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ClaimNext atomically leases the next available task in queue to agentID. It
// selects the highest-priority, oldest (id ASC within a priority) pending task,
// marks it claimed, and mints a fresh lease token bound to a lease of
// leaseSeconds (clamped to defaultLeaseSeconds when <= 0).
//
// Atomicity: the SELECT (pick id) and the UPDATE (mark claimed) run inside ONE
// tx. With the store's SetMaxOpenConns(1) serialized writer, two concurrent
// claimers can never both observe and claim the same row — the loser's tx sees
// the row already in state='claimed' and picks the next pending one (or returns
// claimed=false when the queue is drained). The matching index
// idx_tasks_queue_state(queue, state, priority DESC, id) serves the ordering.
//
// claimed=false (with a zero Task and empty token) means the queue currently
// has no pending task.
func (t *TaskManager) ClaimNext(queue, agentID string, leaseSeconds int) (task Task, leaseToken string, claimed bool, err error) {
	if leaseSeconds <= 0 {
		leaseSeconds = defaultLeaseSeconds
	}
	now := Now()
	expiresAt := now + int64(leaseSeconds)

	token, terr := newToken()
	if terr != nil {
		return Task{}, "", false, terr
	}

	txErr := t.s.tx(func(tx *sql.Tx) error {
		var (
			id       int64
			payload  string
			author   sql.NullString
			priority int
		)
		// Highest priority first, then oldest id. This SELECT + the UPDATE below
		// form the atomic claim; under the single serialized writer no other
		// claimer can grab this id between the two statements.
		qerr := tx.QueryRow(
			`SELECT id, payload, author, priority
			   FROM tasks
			  WHERE queue = ? AND state = ?
			  ORDER BY priority DESC, id ASC
			  LIMIT 1`,
			queue, taskPending,
		).Scan(&id, &payload, &author, &priority)
		if errors.Is(qerr, sql.ErrNoRows) {
			return nil // queue empty: claimed stays false
		}
		if qerr != nil {
			return qerr
		}

		if _, e := tx.Exec(
			`UPDATE tasks
			   SET state=?, lease_agent=?, lease_token=?, lease_expires_at=?
			 WHERE id=? AND state=?`,
			taskClaimed, agentID, token, expiresAt, id, taskPending,
		); e != nil {
			return e
		}

		task = Task{
			ID:                    id,
			Queue:                 queue,
			Payload:               payload,
			Author:                author.String,
			Priority:              priority,
			State:                 taskClaimed,
			LeaseAgent:            agentID,
			LeaseExpiresInSeconds: leaseSeconds,
		}
		leaseToken = token
		claimed = true
		return nil
	})
	if txErr != nil {
		return Task{}, "", false, txErr
	}
	return task, leaseToken, claimed, nil
}

// Complete marks a claimed task done. It is capability-gated: leaseToken must
// match the row's lease_token and the task must still be in state='claimed'.
// Returns false (no error) if the task is absent, the token is wrong, or the
// task is not currently claimed.
func (t *TaskManager) Complete(id int64, leaseToken string) (bool, error) {
	return t.transition(id, leaseToken, taskDone, false)
}

// Fail releases a claimed task held under leaseToken. If requeue is true the
// task returns to pending (lease cleared) so it can be claimed again; otherwise
// it is marked done (give up). Capability-gated identically to Complete:
// returns false if absent / wrong token / not claimed.
func (t *TaskManager) Fail(id int64, leaseToken string, requeue bool) (bool, error) {
	if requeue {
		return t.transition(id, leaseToken, taskPending, true)
	}
	return t.transition(id, leaseToken, taskDone, false)
}

// transition is the shared capability-gated state change for Complete/Fail. It
// requires the row to exist, be state='claimed', and carry a matching
// lease_token. On match it sets the new state; when clearLease is true it also
// nulls the lease columns (the requeue path). Returns whether a row changed.
func (t *TaskManager) transition(id int64, leaseToken, newState string, clearLease bool) (bool, error) {
	if leaseToken == "" {
		return false, nil
	}
	var changed bool
	txErr := t.s.tx(func(tx *sql.Tx) error {
		var stmt string
		if clearLease {
			stmt = `UPDATE tasks
			          SET state=?, lease_agent=NULL, lease_token=NULL, lease_expires_at=NULL
			        WHERE id=? AND state='claimed' AND lease_token=?`
		} else {
			stmt = `UPDATE tasks
			          SET state=?
			        WHERE id=? AND state='claimed' AND lease_token=?`
		}
		res, e := tx.Exec(stmt, newState, id, leaseToken)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		changed = n > 0
		return nil
	})
	if txErr != nil {
		return false, txErr
	}
	return changed, nil
}

// RenewLease extends the lease on a still-held claim, for long-running work. It
// requires a matching lease_token and the task to be state='claimed' with a
// lease that has not yet elapsed; on success lease_expires_at becomes
// Now()+leaseSeconds. Returns false if absent / wrong token / not claimed /
// already lapsed, or for a non-positive leaseSeconds.
func (t *TaskManager) RenewLease(id int64, leaseToken string, leaseSeconds int) (bool, error) {
	if leaseToken == "" || leaseSeconds <= 0 {
		return false, nil
	}
	now := Now()
	expiresAt := now + int64(leaseSeconds)
	var changed bool
	txErr := t.s.tx(func(tx *sql.Tx) error {
		// Guard on lease_expires_at > now so a renew cannot resurrect a lease the
		// reaper has already (or is about to) reclaim; the race-free path is to
		// renew before expiry.
		res, e := tx.Exec(
			`UPDATE tasks
			    SET lease_expires_at=?
			  WHERE id=? AND state='claimed' AND lease_token=? AND lease_expires_at > ?`,
			expiresAt, id, leaseToken, now,
		)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		changed = n > 0
		return nil
	})
	if txErr != nil {
		return false, txErr
	}
	return changed, nil
}

// List returns every task in queue ordered by id ascending. Claimed tasks carry
// their live LeaseAgent and remaining LeaseExpiresInSeconds; pending/done tasks
// report "" / 0 for those fields.
func (t *TaskManager) List(queue string) ([]Task, error) {
	now := Now()
	rows, err := t.s.DB.Query(
		`SELECT id, queue, payload, author, priority, state, lease_agent, lease_expires_at
		   FROM tasks
		  WHERE queue = ?
		  ORDER BY id ASC`,
		queue,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var (
			task    Task
			author  sql.NullString
			agent   sql.NullString
			expires sql.NullInt64
		)
		if err := rows.Scan(
			&task.ID, &task.Queue, &task.Payload, &author,
			&task.Priority, &task.State, &agent, &expires,
		); err != nil {
			return nil, err
		}
		task.Author = author.String
		if task.State == taskClaimed {
			task.LeaseAgent = agent.String
			if expires.Valid {
				task.LeaseExpiresInSeconds = secsLeft(expires.Int64, now)
			}
		}
		out = append(out, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
