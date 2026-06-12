package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// openTemp opens a store at a temp path and registers cleanup.
func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "airlock.db")
	s, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestOpenDB_FileWALAndTables(t *testing.T) {
	s, path := openTemp(t)

	// File created.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not created: %v", err)
	}

	// WAL engaged.
	var jm string
	if err := s.DB.QueryRow("PRAGMA journal_mode;").Scan(&jm); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if jm != "wal" {
		t.Fatalf("journal_mode = %q, want wal", jm)
	}

	// All 6 tables exist.
	want := []string{"agents", "counters", "events", "locks", "notes", "tasks"}
	rows, err := s.DB.Query(
		"SELECT name FROM sqlite_master WHERE type='table' AND name IN (?,?,?,?,?,?)",
		"agents", "counters", "events", "locks", "notes", "tasks",
	)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("tables = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tables = %v, want %v", got, want)
		}
	}
}

func TestTx_CommitOnSuccess(t *testing.T) {
	s, _ := openTemp(t)

	err := s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"INSERT INTO counters(name, value, updated_at) VALUES(?,?,?)",
			"committed", 7, Now(),
		)
		return err
	})
	if err != nil {
		t.Fatalf("tx returned error: %v", err)
	}

	var v int64
	if err := s.DB.QueryRow("SELECT value FROM counters WHERE name=?", "committed").Scan(&v); err != nil {
		t.Fatalf("row should be present after commit: %v", err)
	}
	if v != 7 {
		t.Fatalf("value = %d, want 7", v)
	}
}

func TestTx_RollbackOnError(t *testing.T) {
	s, _ := openTemp(t)

	sentinel := errors.New("boom")
	err := s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			"INSERT INTO counters(name, value, updated_at) VALUES(?,?,?)",
			"rolledback", 99, Now(),
		); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("tx err = %v, want sentinel", err)
	}

	var v int64
	err = s.DB.QueryRow("SELECT value FROM counters WHERE name=?", "rolledback").Scan(&v)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("row should be absent after rollback, got value=%d err=%v", v, err)
	}
}

func TestReopenPreservesData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "airlock.db")

	s1, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB #1: %v", err)
	}
	if _, err := s1.DB.Exec(
		"INSERT INTO counters(name, value, updated_at) VALUES(?,?,?)",
		"persist", 42, Now(),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	s2, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB #2: %v", err)
	}
	defer s2.Close()

	var v int64
	if err := s2.DB.QueryRow("SELECT value FROM counters WHERE name=?", "persist").Scan(&v); err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if v != 42 {
		t.Fatalf("value = %d, want 42", v)
	}
}

func TestOpenDB_CreatesMissingParentDir(t *testing.T) {
	dir := t.TempDir()
	// Nested directory that does not yet exist.
	path := filepath.Join(dir, "nested", "deeper", "airlock.db")

	s, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB with missing parent: %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("parent dir not created: %v", err)
	}
}
