package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// EventManager coordinates DB-backed, generation-counted events plus in-memory
// blocking waits.
//
// The DB row (table events) is the single source of truth: each Signal bumps a
// monotonically increasing generation for the named event. A waiter passes the
// generation it last observed; Wait returns as soon as the current generation
// is strictly GREATER than that. This makes signals durable against the classic
// lost-wakeup: a Signal that fires before a late waiter begins waiting is not
// missed, because the waiter compares against the persisted generation rather
// than relying solely on an in-memory wakeup.
//
// The in-memory registry holds one broadcast channel per event name. To wake
// every waiter on a name we close the current channel and install a fresh one
// (the classic Go broadcast idiom). Waiters grab the current channel under the
// mutex BEFORE reading the DB generation, then block in a select; closing the
// channel they captured wakes them even if the close raced just after their
// read. After waking they re-loop and re-check the generation.
type EventManager struct {
	s *Store

	mu    sync.Mutex // guards chans
	chans map[string]chan struct{}
}

// NewEventManager constructs an EventManager over the given store. Events need
// no background reaper: they persist until explicitly Cleared.
func NewEventManager(s *Store) *EventManager {
	return &EventManager{
		s:     s,
		chans: make(map[string]chan struct{}),
	}
}

// broadcastChan returns the current broadcast channel for name, creating it on
// first use. The returned channel is closed (and replaced) by broadcast to wake
// all current waiters. Callers capture this channel under the mutex before
// reading the DB generation to avoid a lost-wakeup window.
func (e *EventManager) broadcastChan(name string) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := e.chans[name]
	if ch == nil {
		ch = make(chan struct{})
		e.chans[name] = ch
	}
	return ch
}

// broadcast wakes all waiters on name by closing the current channel and
// installing a fresh one for subsequent waiters. A no-op if no channel exists
// yet (no one is waiting).
func (e *EventManager) broadcast(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ch := e.chans[name]; ch != nil {
		close(ch)
		e.chans[name] = make(chan struct{})
	}
}

// Signal bumps the named event's generation by one (inserting with generation=1
// if absent) and updates updated_at to now, then broadcasts to wake every
// current waiter. It returns the new generation.
func (e *EventManager) Signal(name string) (generation int64, err error) {
	now := Now()
	txErr := e.s.tx(func(tx *sql.Tx) error {
		if _, ex := tx.Exec(
			"INSERT INTO events(name, generation, updated_at) VALUES(?, 1, ?) "+
				"ON CONFLICT(name) DO UPDATE SET generation = generation + 1, updated_at = ?",
			name, now, now,
		); ex != nil {
			return ex
		}
		return tx.QueryRow(
			"SELECT generation FROM events WHERE name = ?", name,
		).Scan(&generation)
	})
	if txErr != nil {
		return 0, txErr
	}
	// Broadcast only after the tx commits so woken waiters observe the new
	// generation in the DB.
	e.broadcast(name)
	return generation, nil
}

// Generation returns the current generation for name, or 0 if absent.
func (e *EventManager) Generation(name string) (int64, error) {
	var gen int64
	err := e.s.DB.QueryRow("SELECT generation FROM events WHERE name = ?", name).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return gen, nil
}

// Wait blocks until the named event's generation advances beyond
// lastSeenGeneration, the deadline elapses, or ctx is cancelled.
//
// waitSeconds is clamped to [0, maxWaitSeconds]. The loop reads the current
// generation (0 if absent); if it is strictly greater than lastSeenGeneration
// it returns (current, true, nil) immediately. Otherwise it blocks on the
// event's broadcast channel, the deadline, or ctx cancellation, then re-checks.
// On deadline/ctx without the generation advancing it returns (current, false,
// nil). waitSeconds=0 performs a single immediate check with no blocking.
func (e *EventManager) Wait(ctx context.Context, name string, lastSeenGeneration int64, waitSeconds int) (generation int64, fired bool, err error) {
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	if waitSeconds > maxWaitSeconds {
		waitSeconds = maxWaitSeconds
	}

	var deadline <-chan time.Time
	if waitSeconds > 0 {
		timer := time.NewTimer(time.Duration(waitSeconds) * time.Second)
		defer timer.Stop()
		deadline = timer.C
	}

	for {
		// Capture the broadcast channel BEFORE reading the generation. A Signal
		// that commits and closes this channel after our read but before our
		// select will still wake us (closed channel), so we never miss a wakeup.
		ch := e.broadcastChan(name)

		current, qerr := e.Generation(name)
		if qerr != nil {
			return 0, false, qerr
		}
		if current > lastSeenGeneration {
			return current, true, nil
		}

		// Non-blocking mode: a single immediate check, no waiting.
		if waitSeconds == 0 {
			return current, false, nil
		}

		select {
		case <-ch:
			// Broadcast (Signal or Clear): re-loop and re-check generation.
		case <-deadline:
			return current, false, nil
		case <-ctx.Done():
			return current, false, nil
		}
	}
}

// Clear deletes the named event's row, resetting its generation to absent (0),
// then broadcasts so any current waiters re-evaluate. Cleared events do NOT fire
// a waiter, since a generation of 0 is never greater than a non-negative
// lastSeenGeneration.
func (e *EventManager) Clear(name string) error {
	txErr := e.s.tx(func(tx *sql.Tx) error {
		_, ex := tx.Exec("DELETE FROM events WHERE name = ?", name)
		return ex
	})
	if txErr != nil {
		return txErr
	}
	e.broadcast(name)
	return nil
}
