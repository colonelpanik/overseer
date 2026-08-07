package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task is one unit of work: a goal against a repository, driven through the
// plan and execute loops.
type Task struct {
	ID               int64
	Slug             string
	RepoPath         string
	Goal             string
	Constraints      string
	State            string
	Phase            string
	Iteration        int
	MaxIterations    int
	BlockingSeverity string
	PlanSessionID    string
	ExecSessionID    string
	Branch           string
	BaseRef          string
	// GitCommonDir and GitAdminDir are resolved with rev-parse when the
	// worktree is created, never assumed from RepoPath: a submitted path may
	// itself be a linked worktree, where .git is a file.
	GitCommonDir string
	GitAdminDir  string
	WorktreeDir  string
	PRURL        string
	ErrMsg       string
	// FindingHashes records the blocking-findings fingerprint seen at each
	// iteration of the current phase, for oscillation detection.
	FindingHashes []string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const rfc3339 = time.RFC3339Nano

// CreateTask inserts t and returns it with ID and timestamps populated.
func (s *Store) CreateTask(ctx context.Context, t Task) (Task, error) {
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	if t.MaxIterations == 0 {
		t.MaxIterations = 10
	}
	if t.BlockingSeverity == "" {
		t.BlockingSeverity = "any"
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (slug, repo_path, goal, constraints, state, phase,
			iteration, max_iterations, blocking_severity, plan_session_id,
			exec_session_id, branch, base_ref, git_common_dir, git_admin_dir,
			worktree_dir, pr_url, err_msg,
			finding_hashes, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Slug, t.RepoPath, t.Goal, t.Constraints, t.State, t.Phase,
		t.Iteration, t.MaxIterations, t.BlockingSeverity, t.PlanSessionID,
		t.ExecSessionID, t.Branch, t.BaseRef, t.GitCommonDir, t.GitAdminDir,
		t.WorktreeDir, t.PRURL, t.ErrMsg,
		strings.Join(t.FindingHashes, "\n"),
		t.CreatedAt.Format(rfc3339), t.UpdatedAt.Format(rfc3339))
	if err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Task{}, fmt.Errorf("task id: %w", err)
	}
	t.ID = id
	return t, nil
}

const taskColumns = `id, slug, repo_path, goal, constraints, state, phase,
	iteration, max_iterations, blocking_severity, plan_session_id,
	exec_session_id, branch, base_ref, git_common_dir, git_admin_dir,
	worktree_dir, pr_url, err_msg, finding_hashes,
	created_at, updated_at`

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var hashes, created, updated string
	err := sc.Scan(&t.ID, &t.Slug, &t.RepoPath, &t.Goal, &t.Constraints,
		&t.State, &t.Phase, &t.Iteration, &t.MaxIterations,
		&t.BlockingSeverity, &t.PlanSessionID, &t.ExecSessionID, &t.Branch,
		&t.BaseRef, &t.GitCommonDir, &t.GitAdminDir,
		&t.WorktreeDir, &t.PRURL, &t.ErrMsg, &hashes, &created, &updated)
	if err != nil {
		return Task{}, err
	}
	if hashes != "" {
		t.FindingHashes = strings.Split(hashes, "\n")
	}
	t.CreatedAt, err = time.Parse(rfc3339, created)
	if err != nil {
		return Task{}, fmt.Errorf("parse created_at: %w", err)
	}
	t.UpdatedAt, err = time.Parse(rfc3339, updated)
	if err != nil {
		return Task{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return t, nil
}

// GetTask loads one task by ID.
func (s *Store) GetTask(ctx context.Context, id int64) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task: %w", err)
	}
	return t, nil
}

// ListTasks returns every task, newest first.
func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskColumns+` FROM tasks ORDER BY id DESC`)
}

// ClaimableTasks returns tasks a worker may advance: anything not in a
// terminal or human-gated state.
func (s *Store) ClaimableTasks(ctx context.Context) ([]Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskColumns+`
		FROM tasks
		WHERE state NOT IN ('done', 'failed', 'escalated')
		ORDER BY id ASC`)
}

func (s *Store) queryTasks(ctx context.Context, q string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveTask writes every mutable field of t and bumps updated_at.
func (s *Store) SaveTask(ctx context.Context, t Task) error {
	t.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET state=?, phase=?, iteration=?, max_iterations=?,
			blocking_severity=?, plan_session_id=?, exec_session_id=?,
			branch=?, base_ref=?, git_common_dir=?, git_admin_dir=?,
			worktree_dir=?, pr_url=?, err_msg=?, finding_hashes=?,
			updated_at=?
		WHERE id=?`,
		t.State, t.Phase, t.Iteration, t.MaxIterations, t.BlockingSeverity,
		t.PlanSessionID, t.ExecSessionID, t.Branch, t.BaseRef, t.GitCommonDir,
		t.GitAdminDir, t.WorktreeDir, t.PRURL,
		t.ErrMsg, strings.Join(t.FindingHashes, "\n"),
		t.UpdatedAt.Format(rfc3339), t.ID)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("task %d: %w", t.ID, ErrNotFound)
	}
	return nil
}
