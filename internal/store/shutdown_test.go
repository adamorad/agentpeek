package store

import (
	"context"
	"testing"
	"time"
)

// reaperJoiner is implemented by every manager whose Start launches a background
// reaper goroutine. Wait must return only after that goroutine has exited.
type reaperJoiner interface {
	Start(ctx context.Context)
	Wait()
}

// TestManagers_ReapersJoinOnCancel verifies that each manager's reaper goroutine
// actually exits when its context is cancelled, and that Wait() joins it. This
// is the no-leak guarantee that lets the daemon Wait() on all managers before
// closing the DB: cancel the ctx, then assert Wait() returns promptly (the
// reaper saw ctx.Done() and returned, rather than leaking past shutdown).
func TestManagers_ReapersJoinOnCancel(t *testing.T) {
	s, _ := openTemp(t)
	lm := NewLockManager(s)

	cases := []struct {
		name string
		m    reaperJoiner
	}{
		{"LockManager", lm},
		{"PresenceManager", NewPresenceManager(s, lm)},
		{"TaskManager", NewTaskManager(s)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			tc.m.Start(ctx)

			// Cancel and assert Wait() returns within a short window. The reapers
			// tick at 250ms–1s, so a 2s timeout comfortably covers an in-flight
			// tick draining before the goroutine observes ctx.Done().
			cancel()

			done := make(chan struct{})
			go func() {
				tc.m.Wait()
				close(done)
			}()

			select {
			case <-done:
				// Reaper joined: no leak.
			case <-time.After(2 * time.Second):
				t.Fatalf("%s.Wait() did not return within 2s after ctx cancel — reaper goroutine leaked", tc.name)
			}
		})
	}
}
