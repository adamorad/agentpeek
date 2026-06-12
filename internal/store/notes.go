package store

import (
	"database/sql"
	"errors"
)

// Note is a single key/value entry from the notes table. Notes are a simple
// shared scratchpad: any agent may write a key, and provenance is recorded in
// Author (free-form, may be empty). A note may carry an expiry; expired notes
// are treated as absent on read (lazy expiry — no reaper required).
type Note struct {
	Key   string
	Value string
	// Author is provenance for the last writer. "" if none was supplied.
	Author string
	// ExpiresInSeconds is whole seconds remaining until expiry (>= 0) when
	// HasExpiry is true; 0 and HasExpiry false means the note never expires.
	ExpiresInSeconds int
	HasExpiry        bool
}

// SetNote upserts key→value with provenance author. updated_at is set to now.
// A positive ttlSeconds sets expires_at = now + ttlSeconds; a zero or negative
// ttlSeconds stores a NULL expiry (never expires).
func (s *Store) SetNote(key, value, author string, ttlSeconds int) error {
	now := Now()
	var expiresAt sql.NullInt64
	if ttlSeconds > 0 {
		expiresAt = sql.NullInt64{Int64: now + int64(ttlSeconds), Valid: true}
	}
	return s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"INSERT INTO notes(key, value, author, updated_at, expires_at) VALUES(?,?,?,?,?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value, author=excluded.author, "+
				"updated_at=excluded.updated_at, expires_at=excluded.expires_at",
			key, value, nullableAuthor(author), now, expiresAt,
		)
		return err
	})
}

// GetNote returns the note for key. The bool is false (and Note is zero) when
// the key is absent or its note has expired. Errors are reserved for real I/O
// failures.
func (s *Store) GetNote(key string) (Note, bool, error) {
	now := Now()
	var (
		value     string
		author    sql.NullString
		expiresAt sql.NullInt64
	)
	err := s.DB.QueryRow(
		"SELECT value, author, expires_at FROM notes WHERE key = ? AND (expires_at IS NULL OR expires_at > ?)",
		key, now,
	).Scan(&value, &author, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, false, nil
	}
	if err != nil {
		return Note{}, false, err
	}
	return noteFrom(key, value, author, expiresAt, now), true, nil
}

// ListNotes returns every non-expired note, ordered by key for determinism.
func (s *Store) ListNotes() ([]Note, error) {
	now := Now()
	rows, err := s.DB.Query(
		"SELECT key, value, author, expires_at FROM notes WHERE expires_at IS NULL OR expires_at > ? ORDER BY key",
		now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var (
			key       string
			value     string
			author    sql.NullString
			expiresAt sql.NullInt64
		)
		if err := rows.Scan(&key, &value, &author, &expiresAt); err != nil {
			return nil, err
		}
		out = append(out, noteFrom(key, value, author, expiresAt, now))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteNote removes key. It reports whether a row was actually deleted (false
// if the key did not exist). Note: this deletes regardless of expiry, so it
// also tidies an expired-but-still-present row.
func (s *Store) DeleteNote(key string) (bool, error) {
	var deleted bool
	err := s.tx(func(tx *sql.Tx) error {
		res, e := tx.Exec("DELETE FROM notes WHERE key = ?", key)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		deleted = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// SetNoteIf is an atomic compare-and-swap. It reads the current effective value
// of key (an absent OR expired note counts as ""), and only if that equals
// expected does it upsert newValue (with the same expiry rules as SetNote).
// Returns true when the swap happened, false when expected did not match (in
// which case nothing is changed). To create a brand-new key, pass expected="".
//
// The whole read-compare-write runs in a single tx; combined with the store's
// single-writer connection (SetMaxOpenConns(1)) this is a true CAS — no two
// callers can both observe the same "current" value and both win.
func (s *Store) SetNoteIf(key, expected, newValue, author string, ttlSeconds int) (bool, error) {
	now := Now()
	var expiresAt sql.NullInt64
	if ttlSeconds > 0 {
		expiresAt = sql.NullInt64{Int64: now + int64(ttlSeconds), Valid: true}
	}

	var swapped bool
	err := s.tx(func(tx *sql.Tx) error {
		// Effective current value: "" if absent or expired, else the stored value.
		var (
			value      string
			rowExpires sql.NullInt64
		)
		current := ""
		qerr := tx.QueryRow(
			"SELECT value, expires_at FROM notes WHERE key = ?", key,
		).Scan(&value, &rowExpires)
		switch {
		case errors.Is(qerr, sql.ErrNoRows):
			// absent → effective ""
		case qerr != nil:
			return qerr
		case rowExpires.Valid && rowExpires.Int64 <= now:
			// expired → effective ""
		default:
			current = value
		}

		if current != expected {
			return nil // mismatch: leave swapped=false, change nothing
		}

		if _, e := tx.Exec(
			"INSERT INTO notes(key, value, author, updated_at, expires_at) VALUES(?,?,?,?,?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value, author=excluded.author, "+
				"updated_at=excluded.updated_at, expires_at=excluded.expires_at",
			key, newValue, nullableAuthor(author), now, expiresAt,
		); e != nil {
			return e
		}
		swapped = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return swapped, nil
}

// IncrementCounter atomically adds by to the named counter, creating it at by
// if it does not yet exist, and returns the new value. The upsert and read-back
// run in one tx so concurrent increments cannot lose updates (verified under
// -race in the tests).
func (s *Store) IncrementCounter(name string, by int64) (int64, error) {
	now := Now()
	var newValue int64
	err := s.tx(func(tx *sql.Tx) error {
		if _, e := tx.Exec(
			"INSERT INTO counters(name, value, updated_at) VALUES(?,?,?) "+
				"ON CONFLICT(name) DO UPDATE SET value=value+excluded.value, updated_at=excluded.updated_at",
			name, by, now,
		); e != nil {
			return e
		}
		return tx.QueryRow("SELECT value FROM counters WHERE name = ?", name).Scan(&newValue)
	})
	if err != nil {
		return 0, err
	}
	return newValue, nil
}

// --- helpers ---

// nullableAuthor maps "" to a SQL NULL so absent provenance round-trips as NULL
// rather than an empty string.
func nullableAuthor(author string) sql.NullString {
	return sql.NullString{String: author, Valid: author != ""}
}

// noteFrom assembles a Note from scanned columns, computing the remaining-time
// fields from expiresAt relative to now.
func noteFrom(key, value string, author sql.NullString, expiresAt sql.NullInt64, now int64) Note {
	n := Note{
		Key:    key,
		Value:  value,
		Author: author.String, // "" when NULL
	}
	if expiresAt.Valid {
		n.HasExpiry = true
		n.ExpiresInSeconds = secsLeft(expiresAt.Int64, now)
	}
	return n
}
