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
	"time"

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
	{"tasks", "repo_id", "INTEGER NOT NULL DEFAULT 0"},
	{"proposals", "repo_id", "INTEGER NOT NULL DEFAULT 0"},
	// Which provider served a run, so subscription-covered usage and usage
	// metered against an endpoint the operator configured can be told apart
	// after the fact rather than added into one misleading figure.
	{"steps", "provider", "TEXT NOT NULL DEFAULT ''"},
	{"proposals", "provider", "TEXT NOT NULL DEFAULT ''"},
	// When the operator stopped a task. Not a state: see schema.sql.
	{"tasks", "stopped_at", "TEXT NOT NULL DEFAULT ''"},
	// Which attempt a task, and each of its steps, belongs to.
	{"tasks", "run_seq", "INTEGER NOT NULL DEFAULT 1"},
	{"steps", "run_seq", "INTEGER NOT NULL DEFAULT 1"},
	// What the wizard is doing, and what the architect conversation produced.
	{"proposals", "kind", "TEXT NOT NULL DEFAULT 'analyse'"},
	{"proposals", "design", "TEXT NOT NULL DEFAULT ''"},
	{"proposals", "architect_session", "TEXT NOT NULL DEFAULT ''"},
	// The short form of a task, beside the goal it summarises.
	{"tasks", "subject", "TEXT NOT NULL DEFAULT ''"},
	{"proposal_tasks", "subject", "TEXT NOT NULL DEFAULT ''"},
	{"backlog", "subject", "TEXT NOT NULL DEFAULT ''"},
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
	return backfillRepos(db)
}

// backfillRepos gives every pre-existing task and analysis a repository.
//
// Before repos were an entity a repository was a loose path repeated on every
// row. This creates one repo per distinct path and links the rows to it, so a
// database written by an older build arrives with its history already
// attributed rather than showing an empty repo list beside years of work.
//
// It is idempotent: the work is skipped entirely once nothing is unlinked.
func backfillRepos(db *sql.DB) error {
	var pending int
	err := db.QueryRow(`
		SELECT (SELECT COUNT(*) FROM tasks WHERE repo_id = 0 AND repo_path <> '')
		     + (SELECT COUNT(*) FROM proposals WHERE repo_id = 0 AND repo_path <> '')`).
		Scan(&pending)
	if err != nil {
		return fmt.Errorf("count unlinked rows: %w", err)
	}
	if pending == 0 {
		return nil
	}

	rows, err := db.Query(`
		SELECT DISTINCT repo_path FROM (
			SELECT repo_path FROM tasks     WHERE repo_id = 0 AND repo_path <> ''
			UNION
			SELECT repo_path FROM proposals WHERE repo_id = 0 AND repo_path <> ''
		) ORDER BY repo_path`)
	if err != nil {
		return fmt.Errorf("list unlinked paths: %w", err)
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return fmt.Errorf("scan unlinked path: %w", err)
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin backfill: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(rfc3339)
	for _, path := range paths {
		var id int64
		err := tx.QueryRow(`SELECT id FROM repos WHERE path = ?`, path).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			slug, err := freeSlug(tx, slugForPath(path))
			if err != nil {
				return err
			}
			res, err := tx.Exec(`
				INSERT INTO repos (slug, path, created_at, updated_at)
				VALUES (?,?,?,?)`, slug, path, now, now)
			if err != nil {
				return fmt.Errorf("backfill repo %s: %w", path, err)
			}
			if id, err = res.LastInsertId(); err != nil {
				return fmt.Errorf("backfill repo id: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("look up repo %s: %w", path, err)
		}

		for _, table := range []string{"tasks", "proposals"} {
			_, err := tx.Exec(fmt.Sprintf(
				`UPDATE %s SET repo_id = ? WHERE repo_id = 0 AND repo_path = ?`, table), id, path)
			if err != nil {
				return fmt.Errorf("link %s to repo %s: %w", table, path, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backfill: %w", err)
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
