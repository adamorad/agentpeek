package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSetGetNote_RoundtripAndOverwrite(t *testing.T) {
	s, _ := openTemp(t)

	if err := s.SetNote("k", "v1", "alice", 0); err != nil {
		t.Fatalf("SetNote: %v", err)
	}
	n, ok, err := s.GetNote("k")
	if err != nil || !ok {
		t.Fatalf("GetNote: ok=%v err=%v", ok, err)
	}
	if n.Key != "k" || n.Value != "v1" || n.Author != "alice" {
		t.Fatalf("got %+v", n)
	}
	if n.HasExpiry || n.ExpiresInSeconds != 0 {
		t.Fatalf("expected no expiry, got HasExpiry=%v exp=%d", n.HasExpiry, n.ExpiresInSeconds)
	}

	// Overwrite updates value + author.
	if err := s.SetNote("k", "v2", "bob", 0); err != nil {
		t.Fatalf("SetNote overwrite: %v", err)
	}
	n, ok, _ = s.GetNote("k")
	if !ok || n.Value != "v2" || n.Author != "bob" {
		t.Fatalf("after overwrite got %+v", n)
	}

	// Empty author round-trips as empty string.
	if err := s.SetNote("noauthor", "x", "", 0); err != nil {
		t.Fatalf("SetNote noauthor: %v", err)
	}
	n, _, _ = s.GetNote("noauthor")
	if n.Author != "" {
		t.Fatalf("expected empty author, got %q", n.Author)
	}
}

func TestNote_TTLExpiry(t *testing.T) {
	s, _ := openTemp(t)

	if err := s.SetNote("temp", "soon", "alice", 1); err != nil {
		t.Fatalf("SetNote ttl: %v", err)
	}
	n, ok, err := s.GetNote("temp")
	if err != nil || !ok {
		t.Fatalf("expected present now: ok=%v err=%v", ok, err)
	}
	if !n.HasExpiry || n.ExpiresInSeconds <= 0 {
		t.Fatalf("expected positive expiry, got HasExpiry=%v exp=%d", n.HasExpiry, n.ExpiresInSeconds)
	}

	// A never-expiring note alongside, to assert ListNotes filters only the expired one.
	if err := s.SetNote("perm", "stays", "alice", 0); err != nil {
		t.Fatalf("SetNote perm: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)

	if _, ok, _ := s.GetNote("temp"); ok {
		t.Fatalf("expected temp to be expired/absent")
	}
	notes, err := s.ListNotes()
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	for _, nn := range notes {
		if nn.Key == "temp" {
			t.Fatalf("ListNotes must exclude expired note, got %+v", notes)
		}
	}
	if len(notes) != 1 || notes[0].Key != "perm" {
		t.Fatalf("expected only perm, got %+v", notes)
	}
}

func TestDeleteNote(t *testing.T) {
	s, _ := openTemp(t)

	_ = s.SetNote("k", "v", "alice", 0)

	deleted, err := s.DeleteNote("k")
	if err != nil || !deleted {
		t.Fatalf("first delete: deleted=%v err=%v", deleted, err)
	}
	if _, ok, _ := s.GetNote("k"); ok {
		t.Fatalf("expected absent after delete")
	}
	deleted, err = s.DeleteNote("k")
	if err != nil || deleted {
		t.Fatalf("second delete should be false: deleted=%v err=%v", deleted, err)
	}
}

func TestListNotes_OrderedExcludesExpired(t *testing.T) {
	s, _ := openTemp(t)

	_ = s.SetNote("charlie", "3", "", 0)
	_ = s.SetNote("alpha", "1", "", 0)
	_ = s.SetNote("bravo", "2", "", 0)
	// An already-expired note: ttl of 1s then sleep, plus a live one.
	_ = s.SetNote("zulu", "gone", "", 1)
	time.Sleep(1100 * time.Millisecond)

	notes, err := s.ListNotes()
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	gotKeys := make([]string, len(notes))
	for i, n := range notes {
		gotKeys[i] = n.Key
	}
	want := []string{"alpha", "bravo", "charlie"}
	if len(gotKeys) != len(want) {
		t.Fatalf("keys = %v, want %v", gotKeys, want)
	}
	for i := range want {
		if gotKeys[i] != want[i] {
			t.Fatalf("keys = %v, want %v (ordering)", gotKeys, want)
		}
	}
}

func TestSetNoteIf_CAS(t *testing.T) {
	s, _ := openTemp(t)

	// Create on absent key with expected="".
	ok, err := s.SetNoteIf("k", "", "first", "alice", 0)
	if err != nil || !ok {
		t.Fatalf("CAS create: ok=%v err=%v", ok, err)
	}
	n, _, _ := s.GetNote("k")
	if n.Value != "first" {
		t.Fatalf("after create got %q", n.Value)
	}

	// Wrong expected → no swap, value unchanged.
	ok, err = s.SetNoteIf("k", "WRONG", "second", "bob", 0)
	if err != nil || ok {
		t.Fatalf("CAS wrong expected: ok=%v err=%v", ok, err)
	}
	n, _, _ = s.GetNote("k")
	if n.Value != "first" {
		t.Fatalf("value must be unchanged, got %q", n.Value)
	}

	// Correct expected → swap.
	ok, err = s.SetNoteIf("k", "first", "second", "bob", 0)
	if err != nil || !ok {
		t.Fatalf("CAS correct expected: ok=%v err=%v", ok, err)
	}
	n, _, _ = s.GetNote("k")
	if n.Value != "second" {
		t.Fatalf("after swap got %q", n.Value)
	}

	// Expired note counts as "": CAS with expected="" should overwrite it.
	_ = s.SetNote("e", "old", "alice", 1)
	time.Sleep(1100 * time.Millisecond)
	ok, err = s.SetNoteIf("e", "", "revived", "alice", 0)
	if err != nil || !ok {
		t.Fatalf("CAS over expired (expected=\"\"): ok=%v err=%v", ok, err)
	}
	n, present, _ := s.GetNote("e")
	if !present || n.Value != "revived" {
		t.Fatalf("expected revived, present=%v val=%q", present, n.Value)
	}
}

func TestIncrementCounter(t *testing.T) {
	s, _ := openTemp(t)

	v, err := s.IncrementCounter("c", 5)
	if err != nil || v != 5 {
		t.Fatalf("first increment: v=%d err=%v", v, err)
	}
	v, _ = s.IncrementCounter("c", 3)
	if v != 8 {
		t.Fatalf("accumulate: v=%d, want 8", v)
	}
	v, _ = s.IncrementCounter("c", -10)
	if v != -2 {
		t.Fatalf("decrement: v=%d, want -2", v)
	}
}

func TestIncrementCounter_Concurrent(t *testing.T) {
	s, _ := openTemp(t)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.IncrementCounter("hits", 1); err != nil {
				t.Errorf("IncrementCounter: %v", err)
			}
		}()
	}
	wg.Wait()

	v, err := s.IncrementCounter("hits", 0)
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	if v != n {
		t.Fatalf("final counter = %d, want %d", v, n)
	}
}

func TestSetNoteIf_SingleWinner(t *testing.T) {
	s, _ := openTemp(t)

	const n = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		val := fmt.Sprintf("x%d", i)
		go func(v string) {
			defer wg.Done()
			ok, err := s.SetNoteIf("flag", "", v, "", 0)
			if err != nil {
				t.Errorf("SetNoteIf: %v", err)
				return
			}
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(val)
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("expected exactly one CAS winner, got %d", winners)
	}
	if _, ok, _ := s.GetNote("flag"); !ok {
		t.Fatalf("expected flag note to exist after race")
	}
}
