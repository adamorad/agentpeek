package cli

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/adamorad/airlock/internal/store"
)

// openTestStore opens a fresh store backed by a temp-dir SQLite file.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestRenderSnapshotPopulated builds a store with one of every kind of live
// (and one expired) row, then asserts the rendered snapshot reflects the live
// state and filters the expired lock.
func TestRenderSnapshotPopulated(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	lm := store.NewLockManager(s)
	pm := store.NewPresenceManager(s, lm)
	em := store.NewEventManager(s)
	tm := store.NewTaskManager(s)

	// A live lock.
	if _, err := lm.Lock(ctx, "build-lock", "agentA", 300, 0, ""); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// An expired lock, inserted directly with a past expires_at so it must be
	// filtered out of the snapshot.
	past := store.Now() - 100
	if _, err := s.DB.Exec(
		"INSERT INTO locks(name, agent_id, lock_token, acquired_at, expires_at) VALUES(?,?,?,?,?)",
		"stale-lock", "agentZ", "tok", past-10, past,
	); err != nil {
		t.Fatalf("insert expired lock: %v", err)
	}

	// A note with a TTL and one without.
	if err := s.SetNote("status", "all systems green", "agentA", 600); err != nil {
		t.Fatalf("SetNote ttl: %v", err)
	}
	longVal := strings.Repeat("x", 80)
	if err := s.SetNote("blob", longVal, "agentA", 0); err != nil {
		t.Fatalf("SetNote no-ttl: %v", err)
	}

	// A counter.
	if _, err := s.IncrementCounter("deploys", 7); err != nil {
		t.Fatalf("IncrementCounter: %v", err)
	}

	// An event (Signal mints generation 1).
	gen, err := em.Signal("config-changed")
	if err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if gen != 1 {
		t.Fatalf("expected generation 1, got %d", gen)
	}

	// An agent.
	if err := pm.Register("agentA", 120); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A pushed task.
	taskID, err := tm.Push("deploy", "ship v2", "agentA", 0)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	out, err := renderSnapshot(s)
	if err != nil {
		t.Fatalf("renderSnapshot: %v", err)
	}

	mustContain := func(sub string) {
		t.Helper()
		if !strings.Contains(out, sub) {
			t.Errorf("snapshot missing %q\n---\n%s", sub, out)
		}
	}
	mustNotContain := func(sub string) {
		t.Helper()
		if strings.Contains(out, sub) {
			t.Errorf("snapshot unexpectedly contains %q\n---\n%s", sub, out)
		}
	}

	// Live lock present, expired lock filtered.
	mustContain("build-lock")
	mustNotContain("stale-lock")

	// Counter name + value.
	mustContain("deploys")
	mustContain("7")

	// Event name + generation.
	mustContain("config-changed")

	// Notes: short value verbatim, long value truncated with ellipsis (not
	// shown in full).
	mustContain("all systems green")
	mustContain("…")
	mustNotContain(longVal)

	// Agent present.
	mustContain("agentA")

	// Task id/queue/state.
	mustContain("deploy")
	mustContain("ship v2")
	mustContain("pending")
	mustContain(strconv.FormatInt(taskID, 10))
}

// TestRenderSnapshotEmpty asserts every section renders a "(none)" line for an
// empty database.
func TestRenderSnapshotEmpty(t *testing.T) {
	s := openTestStore(t)

	out, err := renderSnapshot(s)
	if err != nil {
		t.Fatalf("renderSnapshot: %v", err)
	}

	for _, section := range []string{"LOCKS", "AGENTS", "NOTES", "COUNTERS", "EVENTS", "TASKS"} {
		if !strings.Contains(out, section) {
			t.Errorf("missing section header %q\n---\n%s", section, out)
		}
	}
	if got := strings.Count(out, "(none)"); got != 6 {
		t.Errorf("expected 6 (none) lines, got %d\n---\n%s", got, out)
	}
}

func TestHumanDur(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0s"},
		{-5, "0s"},
		{45, "45s"},
		{60, "1m"},
		{62, "1m2s"},
		{3600, "1h"},
		{3720, "1h2m"},
		{86400, "1d"},
		{86400 + 3600, "1d1h"},
		{252, "4m12s"},
	}
	for _, c := range cases {
		if got := humanDur(c.in); got != c.want {
			t.Errorf("humanDur(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 40); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	long := strings.Repeat("a", 50)
	got := truncate(long, 40)
	if len([]rune(got)) != 40 {
		t.Errorf("truncate len = %d, want 40", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncate should end with ellipsis: %q", got)
	}
}
