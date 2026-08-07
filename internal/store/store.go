// Package store persists overseer's task state in SQLite. Agent transcripts
// live on disk, not here; this package holds state and metadata only.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// ErrNotFound is returned when a lookup by ID matches no row.
var ErrNotFound = errors.New("not found")

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

// Open creates the parent directory if needed, opens the database, and
// applies the schema. Opening an existing database is safe and idempotent.
func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// modernc's driver is safe for concurrent use, but WAL plus a single
	// writer avoids SQLITE_BUSY churn from parallel task workers.
	db.SetMaxOpenConns(1)

	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("read schema: %w", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that need ad-hoc queries in tests.
func (s *Store) DB() *sql.DB { return s.db }
