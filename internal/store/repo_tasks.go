package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"overseer/internal/loop"
)

// Task is one unit of work: a goal against a repository, driven through the
// plan and execute loops.
type Task struct {
	ID   int64
	Slug string
	// RepoID links the task to its registered repository. RepoPath stays
	// alongside it as the resolved path, so every existing reader keeps
	// working and the migration is additive.
	RepoID   int64
	RepoPath string
	// Subject is the one line the task is listed under; Goal is the whole
	// instruction and may be a paragraph. An analysis writes both. Empty
	// means nobody supplied one, and Headline derives it from the goal.
	//
	// SaveTask deliberately does not write it, for the same reason it does not
	// write Goal — see the note there.
	Subject          string
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
	// VerifyCommand is run in the worktree after each implementation turn
	// and must exit zero before the code review happens. Empty disables the
	// gate.
	VerifyCommand string
	// FindingHashes records the blocking-findings fingerprint seen at each
	// iteration of the current phase, for oscillation detection.
	FindingHashes []string
	// CostCapUSD is an advisory ceiling on this task's spend. Passing it
	// raises a banner; it never stops a turn, because killing an agent
	// mid-edit leaves the worktree in a state nobody asked for. Zero means no
	// cap.
	CostCapUSD float64
	// StoppedAt is when the operator stopped this task, or zero. It is not a
	// state, because State names the action that was in flight and that is
	// what loop.Pending re-dispatches when the task is started again.
	//
	// SaveTask deliberately does not write it — see the note there.
	StoppedAt time.Time
	// RunSeq is which attempt this is. A restart bumps it, and the worktree,
	// branch, transcripts and agent state are keyed by RunSlug, so one attempt
	// cannot append to another's.
	RunSeq    int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Stopped reports whether the operator has stopped this task.
func (t Task) Stopped() bool { return !t.StoppedAt.IsZero() }

// RunSlug names this attempt's branch, worktree and run directory.
//
// The first attempt is the bare slug, so nothing about an unrestarted task
// changes and every existing branch keeps its name.
func (t Task) RunSlug() string {
	if t.RunSeq <= 1 {
		return t.Slug
	}
	return fmt.Sprintf("%s-r%d", t.Slug, t.RunSeq)
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
	if t.RunSeq == 0 {
		t.RunSeq = 1
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (slug, repo_id, repo_path, subject, goal, constraints, state, phase,
			iteration, max_iterations, blocking_severity, plan_session_id,
			exec_session_id, branch, base_ref, git_common_dir, git_admin_dir,
			worktree_dir, pr_url, err_msg, verify_command,
			finding_hashes, cost_cap_usd, run_seq, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Slug, t.RepoID, t.RepoPath, t.Subject, t.Goal, t.Constraints, t.State, t.Phase,
		t.Iteration, t.MaxIterations, t.BlockingSeverity, t.PlanSessionID,
		t.ExecSessionID, t.Branch, t.BaseRef, t.GitCommonDir, t.GitAdminDir,
		t.WorktreeDir, t.PRURL, t.ErrMsg, t.VerifyCommand,
		strings.Join(t.FindingHashes, "\n"), t.CostCapUSD, t.RunSeq,
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

const taskColumns = `id, slug, repo_id, repo_path, subject, goal, constraints, state, phase,
	iteration, max_iterations, blocking_severity, plan_session_id,
	exec_session_id, branch, base_ref, git_common_dir, git_admin_dir,
	worktree_dir, pr_url, err_msg, verify_command, finding_hashes,
	cost_cap_usd, stopped_at, run_seq, created_at, updated_at`

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var hashes, stopped, created, updated string
	err := sc.Scan(&t.ID, &t.Slug, &t.RepoID, &t.RepoPath, &t.Subject, &t.Goal, &t.Constraints,
		&t.State, &t.Phase, &t.Iteration, &t.MaxIterations,
		&t.BlockingSeverity, &t.PlanSessionID, &t.ExecSessionID, &t.Branch,
		&t.BaseRef, &t.GitCommonDir, &t.GitAdminDir,
		&t.WorktreeDir, &t.PRURL, &t.ErrMsg, &t.VerifyCommand,
		&hashes, &t.CostCapUSD, &stopped, &t.RunSeq, &created, &updated)
	if err != nil {
		return Task{}, err
	}
	if hashes != "" {
		t.FindingHashes = strings.Split(hashes, "\n")
	}
	if stopped != "" {
		if t.StoppedAt, err = time.Parse(rfc3339, stopped); err != nil {
			return Task{}, fmt.Errorf("parse stopped_at: %w", err)
		}
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

// GetTaskBySlug loads one task by its slug.
func (s *Store) GetTaskBySlug(ctx context.Context, slug string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE slug = ?`, slug)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %q: %w", slug, ErrNotFound)
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task by slug: %w", err)
	}
	return t, nil
}

// ClaimableTasks returns tasks a worker may advance: anything not terminal,
// not stopped by the operator, and not a queued task still waiting on a
// dependency.
//
// The dependency gate applies only while a task is queued. Once it has a
// worktree it is in flight, and a dependency that regresses afterwards must
// not yank a running task back out of the pool mid-phase.
//
// The stop gate is not sufficient on its own. This list is read once per poll
// and a worker then drives its task for however long that takes, so a stop
// landing mid-task is delivered to that worker directly; this only keeps the
// scheduler from picking the task up again afterwards.
//
// The terminal states come from loop.TerminalStateNames rather than being
// spelled out here. They used to be literals, which meant the state machine and
// this query each held their own opinion of what "terminal" meant and nothing
// checked they agreed.
func (s *Store) ClaimableTasks(ctx context.Context) ([]Task, error) {
	terminal := loop.TerminalStateNames()
	args := make([]any, 0, len(terminal))
	holes := make([]string, len(terminal))
	for i, state := range terminal {
		holes[i] = "?"
		args = append(args, state)
	}
	return s.queryTasks(ctx, `SELECT `+taskColumns+`
		FROM tasks
		WHERE state NOT IN (`+strings.Join(holes, ",")+`)
		  AND stopped_at = ''
		  AND (state <> 'queued' OR NOT EXISTS (
		        SELECT 1 FROM task_deps d
		        JOIN tasks p ON p.id = d.depends_on_id
		        WHERE d.task_id = tasks.id AND p.state <> 'done'))
		ORDER BY id ASC`, args...)
}

// StopTask parks or unparks one task.
//
// A single column, on purpose. Every other writer of this row holds a copy read
// before the operator pressed anything, so a full-row SaveTask racing this
// would revert it — and the race is not narrow, because a worker's copy is read
// once and held for the whole task. Writing only the column the operator
// changed means the two writers cannot lose to each other at all.
func (s *Store) StopTask(ctx context.Context, id int64, stopped bool) error {
	at := ""
	if stopped {
		at = time.Now().UTC().Format(rfc3339)
	}
	return s.touchTask(ctx, id, `UPDATE tasks SET stopped_at=?, updated_at=? WHERE id=?`, at)
}

// SaveTaskClearingStop writes every mutable field exactly as SaveTask does, and
// clears stopped_at in the same statement.
//
// Used where a stop ends in something other than starting again — abandoning a
// stopped task, or restarting it — so a task can never be left terminal and
// stopped at once, which would render as two contradictory things on the board.
func (s *Store) SaveTaskClearingStop(ctx context.Context, t Task) error {
	if err := s.SaveTask(ctx, t); err != nil {
		return err
	}
	return s.StopTask(ctx, t.ID, false)
}

// RestartTask writes the fields a restart resets, and bumps run_seq.
//
// run_seq is what separates one attempt from the next: the branch, the
// worktree, the transcripts and the agent state are all keyed by RunSlug, so
// without the bump attempt two would adopt attempt one's worktree and append to
// its transcripts.
func (s *Store) RestartTask(ctx context.Context, t Task) error {
	t.UpdatedAt = time.Now().UTC()
	return s.touchTask(ctx, t.ID, `
		UPDATE tasks SET state=?, phase=?, iteration=?, max_iterations=?,
			subject=?, goal=?, constraints=?, plan_session_id='', exec_session_id='',
			branch='', base_ref='', git_common_dir='', git_admin_dir='',
			worktree_dir='', err_msg='', finding_hashes='',
			stopped_at='', run_seq=run_seq+1, updated_at=?
		WHERE id=?`,
		t.State, t.Phase, t.Iteration, t.MaxIterations, t.Subject, t.Goal, t.Constraints)
}

// ClearExecSession forgets the execution session, so the next exec turn starts
// fresh and re-reads PLAN.md rather than running on what the old session
// remembers of it.
//
// Narrow, like the other operator-driven writes: it runs from a request handler
// holding a row read before the operator typed anything.
func (s *Store) ClearExecSession(ctx context.Context, id int64) error {
	return s.touchTask(ctx, id, `UPDATE tasks SET exec_session_id='', updated_at=? WHERE id=?`)
}

// touchTask runs a single-row update that ends with updated_at and id, and
// reports a missing row as ErrNotFound.
func (s *Store) touchTask(ctx context.Context, id int64, query string, args ...any) error {
	args = append(args, time.Now().UTC().Format(rfc3339), id)
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update task %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("task %d: %w", id, ErrNotFound)
	}
	return nil
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
//
// stopped_at, run_seq, goal and subject are deliberately absent. Every caller holds a copy of
// the row read before the operator touched anything, so writing those columns
// here would silently revert a stop — or a restart — landing while a worker was
// mid-step. That is the exact lost update they exist to survive, and it cannot
// be fixed by ordering because the worker's read happens minutes earlier.
// They are written only by StopTask, SaveTaskClearingStop and RestartTask,
// which name them explicitly.
func (s *Store) SaveTask(ctx context.Context, t Task) error {
	t.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET state=?, phase=?, iteration=?, max_iterations=?,
			blocking_severity=?, plan_session_id=?, exec_session_id=?,
			branch=?, base_ref=?, git_common_dir=?, git_admin_dir=?,
			worktree_dir=?, pr_url=?, err_msg=?, verify_command=?,
			finding_hashes=?, cost_cap_usd=?, updated_at=?
		WHERE id=?`,
		t.State, t.Phase, t.Iteration, t.MaxIterations, t.BlockingSeverity,
		t.PlanSessionID, t.ExecSessionID, t.Branch, t.BaseRef, t.GitCommonDir,
		t.GitAdminDir, t.WorktreeDir, t.PRURL,
		t.ErrMsg, t.VerifyCommand, strings.Join(t.FindingHashes, "\n"),
		t.CostCapUSD, t.UpdatedAt.Format(rfc3339), t.ID)
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
