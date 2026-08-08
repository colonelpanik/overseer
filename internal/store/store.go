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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// addedColumns are columns introduced after the first release. schema.sql
// creates them on a fresh database, but CREATE TABLE IF NOT EXISTS is a no-op
// against an existing one, so a database written by an older build needs each
// of these added by hand.
var addedColumns = []struct{ table, column, decl string }{
	{"tasks", "cost_cap_usd", "REAL NOT NULL DEFAULT 0"},
}

// migrate brings an existing database up to the current schema. It is
// idempotent: every step checks for its own effect first, so opening an
// already-current database does nothing.
func migrate(db *sql.DB) error {
	for _, c := range addedColumns {
		has, err := hasColumn(db, c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	// PRAGMA does not accept a bound parameter for the table name. The names
	// come from addedColumns, not from anything a user supplies.
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan %s columns: %w", table, err)
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that need ad-hoc queries in tests.
func (s *Store) DB() *sql.DB { return s.db }
