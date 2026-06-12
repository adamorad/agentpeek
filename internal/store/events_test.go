package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// newEventManager opens a temp store and returns an EventManager. Events need no
// background goroutine, so there is nothing to start or stop.
func newEventManager(t *testing.T) *EventManager {
	t.Helper()
	s, _ := openTemp(t)
	return NewEventManager(s)
}

// Test 1: Signal increments the generation (1,2,3...); Generation reflects it;
// an absent event reports 0.
func TestEvents_SignalIncrementsGeneration(t *testing.T) {
	e := newEventManager(t)

	gen, err := e.Generation("evt")
	if err != nil {
		t.Fatalf("Generation absent: %v", err)
	}
	if gen != 0 {
		t.Fatalf("absent event should report generation 0, got %d", gen)
	}

	for want := int64(1); want <= 3; want++ {
		got, err := e.Signal("evt")
		if err != nil {
			t.Fatalf("Signal: %v", err)
		}
		if got != want {
			t.Fatalf("Signal returned generation %d, want %d", got, want)
		}
		cur, err := e.Generation("evt")
		if err != nil {
			t.Fatalf("Generation: %v", err)
		}
		if cur != want {
			t.Fatalf("Generation returned %d, want %d", cur, want)
		}
	}
}

// Test 2: Wait returns immediately when current > lastSeen.
func TestEvents_WaitImmediateWhenAhead(t *testing.T) {
	e := newEventManager(t)

	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	start := time.Now()
	gen, fired, err := e.Wait(context.Background(), "evt", 0, 5)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !fired {
		t.Fatalf("expected fired=true")
	}
	if gen != 1 {
		t.Fatalf("expected generation 1, got %d", gen)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Wait should have returned immediately, took %v", elapsed)
	}
}

// Test 3: A blocked waiter wakes promptly on a Signal that arrives mid-wait.
func TestEvents_BlockingWake(t *testing.T) {
	e := newEventManager(t)

	// Establish a baseline generation so the waiter waits for the NEXT signal.
	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal baseline: %v", err)
	}
	baseline, err := e.Generation("evt")
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}

	type result struct {
		gen   int64
		fired bool
		err   error
	}
	resCh := make(chan result, 1)
	start := time.Now()
	go func() {
		gen, fired, err := e.Wait(context.Background(), "evt", baseline, 3)
		resCh <- result{gen, fired, err}
	}()

	time.Sleep(200 * time.Millisecond)
	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal wake: %v", err)
	}

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		if !r.fired {
			t.Fatalf("expected fired=true after signal")
		}
		if r.gen != baseline+1 {
			t.Fatalf("expected generation %d, got %d", baseline+1, r.gen)
		}
		if elapsed := time.Since(start); elapsed >= 3*time.Second {
			t.Fatalf("Wait should have woken well before the 3s deadline, took %v", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Wait did not return before the 3s deadline")
	}
}

// Test 4: The generation property — a Signal that fires BEFORE the waiter starts
// is not missed. Signal first, then Wait(lastSeen=0) returns immediately rather
// than blocking for a second signal.
func TestEvents_NoMissedSignal(t *testing.T) {
	e := newEventManager(t)

	// Signal fires before any waiter exists.
	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	start := time.Now()
	// Late waiter passes lastSeen=0; current generation (1) > 0, so it must
	// return immediately and NOT block waiting for a second signal.
	gen, fired, err := e.Wait(context.Background(), "evt", 0, 3)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !fired {
		t.Fatalf("expected fired=true (signal must not be missed)")
	}
	if gen != 1 {
		t.Fatalf("expected generation 1, got %d", gen)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Wait blocked despite a prior signal (lost-wakeup), took %v", elapsed)
	}
}

// Test 5: Timeout — with no signal, Wait returns fired=false after ~waitSeconds
// and the generation is unchanged.
func TestEvents_Timeout(t *testing.T) {
	e := newEventManager(t)

	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal baseline: %v", err)
	}
	baseline, err := e.Generation("evt")
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}

	start := time.Now()
	gen, fired, err := e.Wait(context.Background(), "evt", baseline, 1)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if fired {
		t.Fatalf("expected fired=false on timeout")
	}
	if gen != baseline {
		t.Fatalf("expected generation unchanged (%d), got %d", baseline, gen)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("Wait returned too early (%v); expected ~1s", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Wait took too long (%v) for a 1s deadline", elapsed)
	}
}

// Test 6: Clear resets the generation to 0, and a Clear alone does not fire a
// blocked waiter.
func TestEvents_ClearResetsAndDoesNotFire(t *testing.T) {
	e := newEventManager(t)

	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	baseline, err := e.Generation("evt")
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}

	type result struct {
		gen   int64
		fired bool
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		gen, fired, err := e.Wait(context.Background(), "evt", baseline, 2)
		resCh <- result{gen, fired, err}
	}()

	// Let the waiter block, then Clear. The clear broadcasts (waking the
	// waiter's select) but must NOT cause it to fire, because generation 0 is
	// not greater than its lastSeen baseline.
	time.Sleep(200 * time.Millisecond)
	if err := e.Clear("evt"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if gen, err := e.Generation("evt"); err != nil {
		t.Fatalf("Generation after clear: %v", err)
	} else if gen != 0 {
		t.Fatalf("expected generation 0 after clear, got %d", gen)
	}

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		if r.fired {
			t.Fatalf("Clear alone must not fire a waiter")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Wait did not return within deadline after clear")
	}
}

// Test 7: Multiple waiters all wake on a single Signal (broadcast).
func TestEvents_MultipleWaitersBroadcast(t *testing.T) {
	e := newEventManager(t)

	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal baseline: %v", err)
	}
	baseline, err := e.Generation("evt")
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}

	const n = 3
	type result struct {
		gen   int64
		fired bool
		err   error
	}
	resCh := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gen, fired, err := e.Wait(context.Background(), "evt", baseline, 3)
			resCh <- result{gen, fired, err}
		}()
	}

	// Give all waiters time to begin blocking, then a single signal.
	time.Sleep(200 * time.Millisecond)
	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal wake: %v", err)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("not all waiters returned before deadline")
	}
	close(resCh)

	count := 0
	for r := range resCh {
		count++
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		if !r.fired {
			t.Fatalf("each waiter should fire on broadcast")
		}
		if r.gen != baseline+1 {
			t.Fatalf("expected generation %d, got %d", baseline+1, r.gen)
		}
	}
	if count != n {
		t.Fatalf("expected %d results, got %d", n, count)
	}
}

// Test: waitSeconds=0 performs a single immediate check without blocking.
func TestEvents_NonBlockingZeroWait(t *testing.T) {
	e := newEventManager(t)

	if _, err := e.Signal("evt"); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	start := time.Now()
	gen, fired, err := e.Wait(context.Background(), "evt", 1, 0)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if fired {
		t.Fatalf("expected fired=false (current 1 not > lastSeen 1)")
	}
	if gen != 1 {
		t.Fatalf("expected generation 1, got %d", gen)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitSeconds=0 should not block, took %v", elapsed)
	}
}
