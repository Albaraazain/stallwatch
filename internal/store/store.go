// Package store persists signal samples in a local SQLite database.
// Persistence (rather than in-memory windows) means stall baselines
// survive restarts: stallwatch doesn't go blind for `over` after a deploy.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds honest
)

// Store is the sample database. Safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Sample is one observed value of a signal at a point in time.
type Sample struct {
	TS    time.Time
	Value float64
}

// Open creates or opens the SQLite database at path and ensures the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY
	// under concurrent signal goroutines.
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS samples (
			signal TEXT    NOT NULL,
			ts     INTEGER NOT NULL, -- unix seconds
			value  REAL    NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS samples_signal_ts ON samples (signal, ts)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("init schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Append records one sample for a signal.
func (s *Store) Append(signal string, ts time.Time, value float64) error {
	_, err := s.db.Exec(`INSERT INTO samples (signal, ts, value) VALUES (?, ?, ?)`,
		signal, ts.Unix(), value)
	return err
}

// Window returns samples for a signal at or after from, oldest first.
func (s *Store) Window(signal string, from time.Time) ([]Sample, error) {
	rows, err := s.db.Query(
		`SELECT ts, value FROM samples WHERE signal = ? AND ts >= ? ORDER BY ts`,
		signal, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var ts int64
		var v float64
		if err := rows.Scan(&ts, &v); err != nil {
			return nil, err
		}
		out = append(out, Sample{TS: time.Unix(ts, 0), Value: v})
	}
	return out, rows.Err()
}

// Prune deletes all samples older than before, across every signal.
func (s *Store) Prune(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM samples WHERE ts < ?`, before.Unix())
	return err
}

func (s *Store) Close() error { return s.db.Close() }
