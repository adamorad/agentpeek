package store

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTasks opens a temp store and returns a TaskManager. The reaper is NOT
// started here; tests that exercise requeue start it explicitly.
func newTasks(t *testing.T) (*TaskManager, *Store) {
	t.Helper()
	s, _ := openTemp(t)
	return NewTaskManager(s), s
}

// insertAgent writes a presence row directly so a claimant counts as "present"
// (or, with a past ttl, as absent). This avoids wiring a full PresenceManager
// for the lease tests.
func insertAgent(t *testing.T, s *Store, id string, ttlSeconds int) {
	t.Helper()
	now := Now()
	err := s.tx(func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO agents(agent_id, registered_at, expires_at) VALUES(?,?,?) "+
				"ON CONFLICT(agent_id) DO UPDATE SET expires_at=excluded.expires_at",
			id, now, now+int64(ttlSeconds),
		)
		return e
	})
	if err != nil {
		t.Fatalf("insertAgent %q: %v", id, err)
	}
}

func removeAgent(t *testing.T, s *Store, id string) {
	t.Helper()
	err := s.tx(func(tx *sql.Tx) error {
		_, e := tx.Exec("DELETE FROM agents WHERE agent_id = ?", id)
		return e
	})
	if err != nil {
		t.Fatalf("removeAgent %q: %v", id, err)
	}
}

func findTask(tasks []Task, id int64) (Task, bool) {
	for _, tk := range tasks {
		if tk.ID == id {
			return tk, true
		}
	}
	return Task{}, false
}

// Test 1: Push -> ClaimNext -> List(claimed) -> Complete(token) -> done;
// Complete(wrong token) -> false.
func TestTasks_PushClaimComplete(t *testing.T) {
	tm, _ := newTasks(t)

	id, err := tm.Push("q", "payload-1", "author-1", 0)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Push returned id=%d, want > 0", id)
	}

	task, token, claimed, err := tm.ClaimNext("q", "A", 60)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if !claimed {
		t.Fatalf("ClaimNext claimed=false, want true")
	}
	if task.ID != id {
		t.Fatalf("claimed task id=%d, want %d", task.ID, id)
	}
	if task.Payload != "payload-1" || task.Author != "author-1" {
		t.Fatalf("claimed task payload/author = %q/%q", task.Payload, task.Author)
	}
	if task.State != "claimed" || task.LeaseAgent != "A" {
		t.Fatalf("claimed task state/agent = %q/%q", task.State, task.LeaseAgent)
	}
	if token == "" {
		t.Fatalf("ClaimNext returned empty lease token")
	}

	tasks, err := tm.List("q")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	lt, ok := findTask(tasks, id)
	if !ok || lt.State != "claimed" {
		t.Fatalf("List shows task state=%q ok=%v, want claimed", lt.State, ok)
	}
	if lt.LeaseAgent != "A" || lt.LeaseExpiresInSeconds <= 0 {
		t.Fatalf("List lease info agent=%q exp=%d", lt.LeaseAgent, lt.LeaseExpiresInSeconds)
	}

	// Wrong token -> no-op.
	ok, err = tm.Complete(id, "wrong-token")
	if err != nil {
		t.Fatalf("Complete wrong token err: %v", err)
	}
	if ok {
		t.Fatalf("Complete with wrong token returned true, want false")
	}

	// Right token -> done.
	ok, err = tm.Complete(id, token)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !ok {
		t.Fatalf("Complete with correct token returned false, want true")
	}

	tasks, _ = tm.List("q")
	dt, _ := findTask(tasks, id)
	if dt.State != "done" {
		t.Fatalf("after Complete state=%q, want done", dt.State)
	}

	// Completing an already-done task -> false.
	ok, _ = tm.Complete(id, token)
	if ok {
		t.Fatalf("Complete on done task returned true, want false")
	}
}

// Test 2: priority then FIFO. Push p0, p0, p5 -> claim returns p5 first, then
// the older p0 before the newer p0.
func TestTasks_PriorityThenFIFO(t *testing.T) {
	tm, _ := newTasks(t)

	id0a, _ := tm.Push("q", "p0-old", "", 0)
	id0b, _ := tm.Push("q", "p0-new", "", 0)
	id5, _ := tm.Push("q", "p5", "", 5)

	first, _, _, _ := tm.ClaimNext("q", "A", 60)
	if first.ID != id5 {
		t.Fatalf("first claim id=%d, want p5 id=%d", first.ID, id5)
	}
	second, _, _, _ := tm.ClaimNext("q", "A", 60)
	if second.ID != id0a {
		t.Fatalf("second claim id=%d, want older p0 id=%d", second.ID, id0a)
	}
	third, _, _, _ := tm.ClaimNext("q", "A", 60)
	if third.ID != id0b {
		t.Fatalf("third claim id=%d, want newer p0 id=%d", third.ID, id0b)
	}
}

// Test 3: empty queue -> claimed=false.
func TestTasks_EmptyQueue(t *testing.T) {
	tm, _ := newTasks(t)
	task, token, claimed, err := tm.ClaimNext("empty", "A", 60)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if claimed {
		t.Fatalf("claimed=true on empty queue, want false")
	}
	if token != "" || task.ID != 0 {
		t.Fatalf("expected zero task/token, got id=%d token=%q", task.ID, token)
	}
}

// Test 4: atomic single-claim. Push 10 tasks, 20 goroutines claim concurrently;
// exactly 10 succeed and every claimed id is distinct. Run with -race.
func TestTasks_AtomicSingleClaim(t *testing.T) {
	tm, _ := newTasks(t)

	const nTasks = 10
	const nClaimers = 20
	for i := 0; i < nTasks; i++ {
		if _, err := tm.Push("q", "payload", "", 0); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}

	var (
		mu       sync.Mutex
		claimIDs = map[int64]int{}
		success  int64
		wg       sync.WaitGroup
		start    = make(chan struct{})
	)

	for i := 0; i < nClaimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			task, _, claimed, err := tm.ClaimNext("q", "A", 60)
			if err != nil {
				t.Errorf("ClaimNext: %v", err)
				return
			}
			if claimed {
				atomic.AddInt64(&success, 1)
				mu.Lock()
				claimIDs[task.ID]++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&success); got != nTasks {
		t.Fatalf("successful claims = %d, want %d", got, nTasks)
	}
	if len(claimIDs) != nTasks {
		t.Fatalf("distinct claimed ids = %d, want %d", len(claimIDs), nTasks)
	}
	for id, count := range claimIDs {
		if count != 1 {
			t.Fatalf("task id=%d claimed %d times, want exactly 1", id, count)
		}
	}
}

// Test 5: Fail(requeue=true) -> pending (claimable again); Fail(requeue=false)
// -> done.
func TestTasks_FailRequeueAndGiveUp(t *testing.T) {
	tm, _ := newTasks(t)

	// Requeue path.
	idR, _ := tm.Push("q", "requeue", "", 0)
	_, tokR, _, _ := tm.ClaimNext("q", "A", 60)
	ok, err := tm.Fail(idR, tokR, true)
	if err != nil {
		t.Fatalf("Fail requeue: %v", err)
	}
	if !ok {
		t.Fatalf("Fail(requeue=true) returned false, want true")
	}
	tasks, _ := tm.List("q")
	rt, _ := findTask(tasks, idR)
	if rt.State != "pending" || rt.LeaseAgent != "" {
		t.Fatalf("after Fail-requeue state=%q agent=%q, want pending/\"\"", rt.State, rt.LeaseAgent)
	}
	// Claimable again.
	again, _, claimed, _ := tm.ClaimNext("q", "B", 60)
	if !claimed || again.ID != idR {
		t.Fatalf("requeued task not reclaimable: claimed=%v id=%d", claimed, again.ID)
	}

	// Give-up path.
	idG, _ := tm.Push("q2", "giveup", "", 0)
	_, tokG, _, _ := tm.ClaimNext("q2", "A", 60)
	ok, err = tm.Fail(idG, tokG, false)
	if err != nil {
		t.Fatalf("Fail giveup: %v", err)
	}
	if !ok {
		t.Fatalf("Fail(requeue=false) returned false, want true")
	}
	tasks, _ = tm.List("q2")
	gt, _ := findTask(tasks, idG)
	if gt.State != "done" {
		t.Fatalf("after Fail-giveup state=%q, want done", gt.State)
	}
}

// Test 6: lease-expiry requeue. Agent A is present (so it is NOT requeued for
// absence); claim with a 1s lease; without renewing, the reaper requeues the
// task once the lease elapses. A second claim then succeeds.
func TestTasks_LeaseExpiryRequeue(t *testing.T) {
	tm, s := newTasks(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm.Start(ctx)

	insertAgent(t, s, "A", 3600) // present for the whole test

	id, _ := tm.Push("q", "payload", "", 0)
	_, _, claimed, _ := tm.ClaimNext("q", "A", 1)
	if !claimed {
		t.Fatalf("initial ClaimNext failed")
	}

	// Wait past the lease + a reaper tick.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		tasks, _ := tm.List("q")
		if tk, ok := findTask(tasks, id); ok && tk.State == "pending" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	tasks, _ := tm.List("q")
	tk, _ := findTask(tasks, id)
	if tk.State != "pending" {
		t.Fatalf("after lease expiry state=%q, want pending (reaper requeue)", tk.State)
	}

	// Claimable again.
	_, _, claimed2, _ := tm.ClaimNext("q", "A", 60)
	if !claimed2 {
		t.Fatalf("re-claim after requeue failed")
	}
}

// Test 7: claimant-absent requeue. Register B, claim with a long lease, then let
// B's presence lapse (remove it). The reaper requeues because the claimant is
// no longer present even though the lease has not elapsed.
func TestTasks_ClaimantAbsentRequeue(t *testing.T) {
	tm, s := newTasks(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm.Start(ctx)

	insertAgent(t, s, "B", 3600)

	id, _ := tm.Push("q", "payload", "", 0)
	_, _, claimed, _ := tm.ClaimNext("q", "B", 3600) // long lease, won't elapse
	if !claimed {
		t.Fatalf("initial ClaimNext failed")
	}

	// B departs / crashes: drop its presence row. Lease is still far from
	// expiry, so only the absence condition can trigger the requeue.
	removeAgent(t, s, "B")

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		tasks, _ := tm.List("q")
		if tk, ok := findTask(tasks, id); ok && tk.State == "pending" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	tasks, _ := tm.List("q")
	tk, _ := findTask(tasks, id)
	if tk.State != "pending" {
		t.Fatalf("after claimant absence state=%q, want pending (reaper requeue)", tk.State)
	}
}

// Test 8: RenewLease keeps a short lease alive so the reaper does NOT requeue
// within the window. Agent A stays present; we claim a 2s lease and renew it
// repeatedly while asserting the task stays claimed across reaper ticks.
func TestTasks_RenewLeasePreventsRequeue(t *testing.T) {
	tm, s := newTasks(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm.Start(ctx)

	insertAgent(t, s, "A", 3600)

	id, _ := tm.Push("q", "payload", "", 0)
	_, token, claimed, _ := tm.ClaimNext("q", "A", 2)
	if !claimed {
		t.Fatalf("initial ClaimNext failed")
	}

	// Over ~3s (longer than the 2s lease, spanning multiple reaper ticks), renew
	// every 500ms. The task must remain claimed throughout.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := tm.RenewLease(id, token, 2)
		if err != nil {
			t.Fatalf("RenewLease: %v", err)
		}
		if !ok {
			t.Fatalf("RenewLease returned false while lease still held")
		}
		tasks, _ := tm.List("q")
		tk, _ := findTask(tasks, id)
		if tk.State != "claimed" {
			t.Fatalf("task requeued despite renew: state=%q", tk.State)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Renew with wrong token -> false.
	if ok, _ := tm.RenewLease(id, "wrong", 2); ok {
		t.Fatalf("RenewLease with wrong token returned true, want false")
	}
}
