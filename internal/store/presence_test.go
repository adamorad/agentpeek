package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newPresence opens a temp store and returns a PresenceManager plus the
// LockManager it is wired to. The lock manager's reaper is NOT started here;
// individual tests start it (and the presence reaper) when they need expiry.
func newPresence(t *testing.T) (*PresenceManager, *LockManager) {
	t.Helper()
	s, _ := openTemp(t)
	lm := NewLockManager(s)
	pm := NewPresenceManager(s, lm)
	return pm, lm
}

func findAgent(agents []AgentInfo, id string) (AgentInfo, bool) {
	for _, a := range agents {
		if a.AgentID == id {
			return a, true
		}
	}
	return AgentInfo{}, false
}

func TestPresence_RegisterThenList(t *testing.T) {
	pm, _ := newPresence(t)

	if err := pm.Register("A", 30); err != nil {
		t.Fatalf("Register: %v", err)
	}

	agents, err := pm.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	a, ok := findAgent(agents, "A")
	if !ok {
		t.Fatalf("agent A absent from ListAgents")
	}
	if a.ExpiresInSeconds <= 0 || a.ExpiresInSeconds > 30 {
		t.Fatalf("ExpiresInSeconds = %d, want ~30 (0,30]", a.ExpiresInSeconds)
	}
	if _, ok := findAgent(agents, "unknown"); ok {
		t.Fatalf("unknown agent should be absent")
	}
}

func TestPresence_HeartbeatExtendsTTL(t *testing.T) {
	pm, _ := newPresence(t)

	if err := pm.Register("A", 1); err != nil {
		t.Fatalf("Register ttl=1: %v", err)
	}
	if err := pm.Register("A", 60); err != nil {
		t.Fatalf("re-Register ttl=60: %v", err)
	}

	agents, err := pm.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	a, ok := findAgent(agents, "A")
	if !ok {
		t.Fatalf("agent A absent after heartbeat")
	}
	if a.ExpiresInSeconds <= 1 {
		t.Fatalf("ExpiresInSeconds = %d, want > 1 after extension", a.ExpiresInSeconds)
	}
}

func TestPresence_UnregisterReleasesLocks(t *testing.T) {
	pm, lm := newPresence(t)
	ctx := context.Background()

	if err := pm.Register("A", 60); err != nil {
		t.Fatalf("Register: %v", err)
	}
	res, err := lm.Lock(ctx, "res", "A", 60, 0, "")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !res.Locked {
		t.Fatalf("agent A should acquire free lock")
	}

	if err := pm.Unregister("A"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}

	// Agent gone.
	agents, err := pm.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if _, ok := findAgent(agents, "A"); ok {
		t.Fatalf("agent A should be gone after Unregister")
	}

	// Lock released.
	locks, err := lm.ListLocks()
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if len(locks) != 0 {
		t.Fatalf("expected no locks after Unregister, got %d", len(locks))
	}
}

func TestPresence_ExpiryReaperReleasesLocks(t *testing.T) {
	pm, lm := newPresence(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lm.Start(ctx)
	pm.Start(ctx)

	// Agent A holds a long-TTL lock but a short presence TTL.
	res, err := lm.Lock(ctx, "res", "A", 600, 0, "")
	if err != nil {
		t.Fatalf("Lock A: %v", err)
	}
	if !res.Locked {
		t.Fatalf("agent A should acquire free lock")
	}
	if err := pm.Register("A", 1); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Wait for the presence reaper (~1s tick) to expire A and release its locks.
	time.Sleep(1500 * time.Millisecond)

	// Agent gone from presence.
	agents, err := pm.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if _, ok := findAgent(agents, "A"); ok {
		t.Fatalf("agent A should be reaped after presence TTL")
	}

	// Lock released: a fresh acquire by B now succeeds despite A's long lock TTL.
	res2, err := lm.Lock(ctx, "res", "B", 60, 0, "")
	if err != nil {
		t.Fatalf("Lock B: %v", err)
	}
	if !res2.Locked {
		t.Fatalf("agent B should acquire the lock freed by A's expiry, HeldBy=%q", res2.HeldBy)
	}
}

func TestPresence_InvalidTTL(t *testing.T) {
	pm, _ := newPresence(t)

	if err := pm.Register("A", 0); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("Register ttl=0: got %v, want ErrInvalidTTL", err)
	}
	if err := pm.Register("A", -5); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("Register ttl=-5: got %v, want ErrInvalidTTL", err)
	}
}
