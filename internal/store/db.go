// Package store owns persistence for airlock: opening the SQLite database,
// running migrations, and exposing typed accessors over the connection.
package store

import (
	"database/sql"

	// Register the pure-Go SQLite driver. It is imported for its side effect
	// (driver registration under the name "sqlite") so later tasks can call
	// sql.Open("sqlite", path) without cgo. Chosen deliberately for static,
	// cross-compilable builds.
	_ "modernc.org/sqlite"
)

// Store wraps the application's database handle and is the single entry point
// for all persistence operations. Later tasks will hang query/mutation methods
// off this type (e.g. recording observed ports, leases, and audit events).
type Store struct {
	DB *sql.DB
}

// OpenDB opens (and, in a later task, migrates) the SQLite database located at
// path, returning a ready-to-use *Store.
//
// Placeholder: this stub does not yet open a connection. A future task will:
//   - ensure the parent directory exists,
//   - open the pure-Go modernc.org/sqlite driver with sane PRAGMAs
//     (WAL, foreign_keys=on, busy_timeout),
//   - run schema migrations,
//   - and return a *Store wrapping the live *sql.DB.
//
// For now it returns an empty *Store and a nil error so callers can compile
// against the contract without a real database.
func OpenDB(path string) (*Store, error) {
	_ = path
	return &Store{}, nil
}

// Close releases the underlying database connection if one is open.
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}
