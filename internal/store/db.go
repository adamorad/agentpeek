// Package store owns persistence for airlock: opening the SQLite database,
// running migrations, and exposing typed accessors over the connection.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Register the pure-Go SQLite driver. It is imported for its side effect
	// (driver registration under the name "sqlite") so we can call
	// sql.Open("sqlite", dsn) without cgo. Chosen deliberately for static,
	// cross-compilable builds.
	_ "modernc.org/sqlite"
)

// Store wraps the application's database handle and is the single entry point
// for all persistence operations. Sibling files in this package (locks, notes,
// counters, presence, events, tasks) hang query/mutation methods off this type.
type Store struct {
	DB *sql.DB
}

// schema is the complete, idempotent DDL for every airlock table. All
// timestamps are Unix seconds (INTEGER). A NULL expires_at means "never
// expires" where the column is nullable.
const schema = `
CREATE TABLE IF NOT EXISTS locks (
  name        TEXT PRIMARY KEY,
  agent_id    TEXT NOT NULL,
  lock_token  TEXT NOT NULL,
  acquired_at INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notes (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  author     TEXT,
  updated_at INTEGER NOT NULL,
  expires_at INTEGER            -- NULL = never expires
);
CREATE TABLE IF NOT EXISTS counters (
  name       TEXT PRIMARY KEY,
  value      INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS agents (
  agent_id      TEXT PRIMARY KEY,
  registered_at INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  name        TEXT PRIMARY KEY,
  generation  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  queue            TEXT NOT NULL,
  payload          TEXT NOT NULL,
  author           TEXT,
  priority         INTEGER NOT NULL DEFAULT 0,
  state            TEXT NOT NULL DEFAULT 'pending',  -- pending | claimed | done
  lease_agent      TEXT,
  lease_token      TEXT,
  lease_expires_at INTEGER,
  created_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_queue_state ON tasks(queue, state, priority DESC, id);
`

// OpenDB opens the SQLite database at path, ensuring its parent directory
// exists, applying WAL + busy_timeout + foreign_keys pragmas, and running the
// idempotent schema. It returns a ready-to-use *Store.
func OpenDB(path string) (*Store, error) {
	// Ensure the parent directory (e.g. ~/.airlock/) exists.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create parent dir %q: %w", dir, err)
		}
	}

	// modernc.org/sqlite accepts pragmas in the DSN via repeated _pragma query
	// params. We enable WAL (durable, concurrent reads), a 5s busy timeout, and
	// foreign key enforcement.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
		path,
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}

	// Deliberately serialize all DB access through a single connection. This
	// coordination workload is low-throughput, and a single writer eliminates
	// SQLITE_BUSY races entirely while WAL still provides durability. Any
	// waiting/blocking (e.g. agents waiting on a lock) happens in-memory in the
	// locks layer, never while holding a DB transaction — so a single conn does
	// not serialize logical waits, only physical DB I/O.
	db.SetMaxOpenConns(1)

	// Verify WAL actually engaged. With modernc, journal_mode in the DSN takes
	// effect on the first physical connection; assert it returns "wal".
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: query journal_mode: %w", err)
	}
	if journalMode != "wal" {
		_ = db.Close()
		return nil, fmt.Errorf("store: expected WAL journal mode, got %q", journalMode)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	return &Store{DB: db}, nil
}

// Close releases the underlying database connection if one is open.
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}

// tx runs fn inside a transaction, committing if fn returns nil and rolling
// back otherwise (or on panic). Later tasks use this for atomic multi-statement
// writes.
func (s *Store) tx(fn func(*sql.Tx) error) error {
	t, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = t.Rollback()
			panic(p)
		}
	}()
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	if err := t.Commit(); err != nil {
		return fmt.Errorf("store: commit tx: %w", err)
	}
	return nil
}

// Now returns the current time as Unix seconds. Tests use relative offsets from
// this rather than asserting absolute wall-clock values.
func Now() int64 {
	return time.Now().Unix()
}
