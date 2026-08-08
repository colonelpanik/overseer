package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Repo is a repository overseer works on.
type Repo struct {
	ID            int64
	Slug          string
	Path          string
	OriginURL     string
	DefaultBranch string
	// Detected is the toolchain probe's one-line summary, shown so an
	// operator can see overseer understood the repository.
	Detected string
	// VerifyCommand, BlockingSeverity and CostCapUSD are defaults new tasks
	// inherit. Empty or zero means "fall through to the daemon default",
	// never "off" — a repository that wants no verify gate simply has none
	// configured anywhere.
	VerifyCommand    string
	BlockingSeverity string
	CostCapUSD       float64
	ArchivedAt       time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Archived reports whether the repository has been put away.
func (r Repo) Archived() bool { return !r.ArchivedAt.IsZero() }

const repoColumns = `id, slug, path, origin_url, default_branch, detected,
	verify_command, blocking_severity, cost_cap_usd, archived_at,
	created_at, updated_at`

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// slugForPath derives a short name from a repository's directory.
func slugForPath(path string) string {
	base := filepath.Base(strings.TrimRight(path, string(filepath.Separator)))
	s := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if s == "" {
		return "repo"
	}
	return s
}

// execer is the subset of *sql.DB and *sql.Tx that freeSlug needs, so the
// backfill can reuse it inside its transaction.
type execer interface {
	QueryRow(query string, args ...any) *sql.Row
}

// freeSlug returns base, or base-2, base-3 … until one is unused.
//
// Two repositories can sit in directories with the same name — a vendored
// copy, or the same project checked out twice — and the slug is what names one
// in a task file, so it has to be unique even when the basename is not.
func freeSlug(db execer, base string) (string, error) {
	for attempt := 1; attempt <= 100; attempt++ {
		slug := base
		if attempt > 1 {
			slug = fmt.Sprintf("%s-%d", base, attempt)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM repos WHERE slug = ?`, slug).Scan(&n); err != nil {
			return "", fmt.Errorf("check slug %s: %w", slug, err)
		}
		if n == 0 {
			return slug, nil
		}
	}
	return "", fmt.Errorf("no free slug for %q after 100 attempts", base)
}

func scanRepo(sc interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	var archived, created, updated string
	err := sc.Scan(&r.ID, &r.Slug, &r.Path, &r.OriginURL, &r.DefaultBranch,
		&r.Detected, &r.VerifyCommand, &r.BlockingSeverity, &r.CostCapUSD,
		&archived, &created, &updated)
	if err != nil {
		return Repo{}, err
	}
	if archived != "" {
		if r.ArchivedAt, err = time.Parse(rfc3339, archived); err != nil {
			return Repo{}, fmt.Errorf("parse archived_at: %w", err)
		}
	}
	if r.CreatedAt, err = time.Parse(rfc3339, created); err != nil {
		return Repo{}, fmt.Errorf("parse repo created_at: %w", err)
	}
	if r.UpdatedAt, err = time.Parse(rfc3339, updated); err != nil {
		return Repo{}, fmt.Errorf("parse repo updated_at: %w", err)
	}
	return r, nil
}

// UpsertRepo registers a repository by path, or returns the existing one.
//
// The path is the identity. Calling this on every submit and every analysis is
// what makes registration a side effect of use rather than a step before it,
// so an existing task file keeps working and the repo list fills itself in.
// Fields left empty on r do not overwrite what is already stored: a probe that
// came back blank must not erase a verify command the operator set by hand.
func (s *Store) UpsertRepo(ctx context.Context, r Repo) (Repo, error) {
	if strings.TrimSpace(r.Path) == "" {
		return Repo{}, errors.New("a repository needs a path")
	}

	existing, err := s.RepoByPath(ctx, r.Path)
	switch {
	case errors.Is(err, ErrNotFound):
		now := time.Now().UTC()
		slug, err := freeSlug(s.db, slugForPath(r.Path))
		if err != nil {
			return Repo{}, err
		}
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO repos (slug, path, origin_url, default_branch, detected,
				verify_command, blocking_severity, cost_cap_usd, archived_at,
				created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,'',?,?)`,
			slug, r.Path, r.OriginURL, r.DefaultBranch, r.Detected,
			r.VerifyCommand, r.BlockingSeverity, r.CostCapUSD,
			now.Format(rfc3339), now.Format(rfc3339))
		if err != nil {
			return Repo{}, fmt.Errorf("insert repo: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Repo{}, fmt.Errorf("repo id: %w", err)
		}
		r.ID, r.Slug, r.CreatedAt, r.UpdatedAt = id, slug, now, now
		return r, nil

	case err != nil:
		return Repo{}, err
	}

	// Refresh only what the caller actually learned.
	if r.OriginURL != "" {
		existing.OriginURL = r.OriginURL
	}
	if r.DefaultBranch != "" {
		existing.DefaultBranch = r.DefaultBranch
	}
	if r.Detected != "" {
		existing.Detected = r.Detected
	}
	if err := s.SaveRepo(ctx, existing); err != nil {
		return Repo{}, err
	}
	return existing, nil
}

// SaveRepo writes a repository's mutable fields.
func (s *Store) SaveRepo(ctx context.Context, r Repo) error {
	r.UpdatedAt = time.Now().UTC()
	archived := ""
	if !r.ArchivedAt.IsZero() {
		archived = r.ArchivedAt.Format(rfc3339)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE repos SET origin_url=?, default_branch=?, detected=?,
			verify_command=?, blocking_severity=?, cost_cap_usd=?,
			archived_at=?, updated_at=?
		WHERE id=?`,
		r.OriginURL, r.DefaultBranch, r.Detected, r.VerifyCommand,
		r.BlockingSeverity, r.CostCapUSD, archived,
		r.UpdatedAt.Format(rfc3339), r.ID)
	if err != nil {
		return fmt.Errorf("update repo: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("repo %d: %w", r.ID, ErrNotFound)
	}
	return nil
}

// GetRepo loads one repository by ID.
func (s *Store) GetRepo(ctx context.Context, id int64) (Repo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE id = ?`, id)
	r, err := scanRepo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, fmt.Errorf("repo %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Repo{}, fmt.Errorf("get repo: %w", err)
	}
	return r, nil
}

// RepoByPath loads one repository by its path.
func (s *Store) RepoByPath(ctx context.Context, path string) (Repo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE path = ?`, path)
	r, err := scanRepo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, fmt.Errorf("repo %q: %w", path, ErrNotFound)
	}
	if err != nil {
		return Repo{}, fmt.Errorf("get repo by path: %w", err)
	}
	return r, nil
}

// RepoBySlug loads one repository by its short name, which is how a task file
// may name a repository instead of repeating its path.
func (s *Store) RepoBySlug(ctx context.Context, slug string) (Repo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE slug = ?`, slug)
	r, err := scanRepo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, fmt.Errorf("repo %q: %w", slug, ErrNotFound)
	}
	if err != nil {
		return Repo{}, fmt.Errorf("get repo by slug: %w", err)
	}
	return r, nil
}

// ListRepos returns every repository, oldest first so the list is stable as
// new ones register.
func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoColumns+` FROM repos ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query repos: %w", err)
	}
	defer rows.Close()

	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
