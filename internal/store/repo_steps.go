package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Step is one agent invocation: a Claude turn or a Codex review.
type Step struct {
	ID             int64
	TaskID         int64
	Phase          string
	Iteration      int
	Agent          string
	State          string
	StartedAt      time.Time
	EndedAt        time.Time
	ExitCode       int
	Verdict        string
	InputTokens    int
	OutputTokens   int
	CostUSD        float64
	TranscriptPath string
	ErrMsg         string
}

// Finding is one item from a Codex review, or a synthesised verify failure.
type Finding struct {
	ID       int64
	StepID   int64
	Severity string
	File     string
	Line     int
	Summary  string
	// Detail is volatile supplementary output, kept out of Summary because the
	// oscillation fingerprint hashes Summary.
	Detail   string
	Blocking bool
}

// Totals aggregates cost and tokens across a task's steps.
type Totals struct {
	CostUSD      float64
	InputTokens  int
	OutputTokens int
}

// StartStep records the beginning of an agent invocation.
func (s *Store) StartStep(ctx context.Context, st Step) (Step, error) {
	st.State = "running"
	st.StartedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO steps (task_id, phase, iteration, agent, state, started_at,
			transcript_path)
		VALUES (?,?,?,?,?,?,?)`,
		st.TaskID, st.Phase, st.Iteration, st.Agent, st.State,
		st.StartedAt.Format(rfc3339), st.TranscriptPath)
	if err != nil {
		return Step{}, fmt.Errorf("insert step: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Step{}, fmt.Errorf("step id: %w", err)
	}
	st.ID = id
	return st, nil
}

// FinishStep closes out a step and stores its findings in one transaction.
// A step with ErrMsg set is recorded as failed; otherwise done.
func (s *Store) FinishStep(ctx context.Context, st Step, findings []Finding) error {
	st.EndedAt = time.Now().UTC()
	st.State = "done"
	if st.ErrMsg != "" {
		st.State = "failed"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE steps SET state=?, ended_at=?, exit_code=?, verdict=?,
			input_tokens=?, output_tokens=?, cost_usd=?, transcript_path=?,
			err_msg=?
		WHERE id=?`,
		st.State, st.EndedAt.Format(rfc3339), st.ExitCode, st.Verdict,
		st.InputTokens, st.OutputTokens, st.CostUSD, st.TranscriptPath,
		st.ErrMsg, st.ID); err != nil {
		return fmt.Errorf("update step: %w", err)
	}

	for _, f := range findings {
		blocking := 0
		if f.Blocking {
			blocking = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO findings (step_id, severity, file, line, summary, detail, blocking)
			VALUES (?,?,?,?,?,?,?)`,
			st.ID, f.Severity, f.File, f.Line, f.Summary, f.Detail, blocking); err != nil {
			return fmt.Errorf("insert finding: %w", err)
		}
	}
	return tx.Commit()
}

const stepColumns = `id, task_id, phase, iteration, agent, state, started_at,
	ended_at, exit_code, verdict, input_tokens, output_tokens, cost_usd,
	transcript_path, err_msg`

// ListSteps returns a task's steps in chronological order.
func (s *Store) ListSteps(ctx context.Context, taskID int64) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+stepColumns+` FROM steps WHERE task_id = ? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query steps: %w", err)
	}
	defer rows.Close()

	var out []Step
	for rows.Next() {
		var st Step
		var started, ended string
		if err := rows.Scan(&st.ID, &st.TaskID, &st.Phase, &st.Iteration,
			&st.Agent, &st.State, &started, &ended, &st.ExitCode, &st.Verdict,
			&st.InputTokens, &st.OutputTokens, &st.CostUSD,
			&st.TranscriptPath, &st.ErrMsg); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		st.StartedAt, err = time.Parse(rfc3339, started)
		if err != nil {
			return nil, fmt.Errorf("parse started_at: %w", err)
		}
		if ended != "" {
			st.EndedAt, err = time.Parse(rfc3339, ended)
			if err != nil {
				return nil, fmt.Errorf("parse ended_at: %w", err)
			}
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListFindings returns the findings recorded against one step.
func (s *Store) ListFindings(ctx context.Context, stepID int64) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, step_id, severity, file, line, summary, detail, blocking
		FROM findings WHERE step_id = ? ORDER BY id ASC`, stepID)
	if err != nil {
		return nil, fmt.Errorf("query findings: %w", err)
	}
	defer rows.Close()

	var out []Finding
	for rows.Next() {
		var f Finding
		var blocking int
		if err := rows.Scan(&f.ID, &f.StepID, &f.Severity, &f.File, &f.Line,
			&f.Summary, &f.Detail, &blocking); err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		f.Blocking = blocking == 1
		out = append(out, f)
	}
	return out, rows.Err()
}

// InterruptRunningSteps marks every step still in "running" as interrupted.
// Called once at daemon startup: a running step cannot have survived a
// restart, and leaving it running would make the dashboard lie.
func (s *Store) InterruptRunningSteps(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE steps SET state='interrupted', ended_at=?
		WHERE state='running'`, time.Now().UTC().Format(rfc3339))
	if err != nil {
		return 0, fmt.Errorf("interrupt steps: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

// TaskTotals sums cost and token usage across a task's steps.
func (s *Store) TaskTotals(ctx context.Context, taskID int64) (Totals, error) {
	var t Totals
	var cost sql.NullFloat64
	var in, out sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT SUM(cost_usd), SUM(input_tokens), SUM(output_tokens)
		FROM steps WHERE task_id = ?`, taskID).Scan(&cost, &in, &out)
	if err != nil {
		return t, fmt.Errorf("task totals: %w", err)
	}
	t.CostUSD = cost.Float64
	t.InputTokens = int(in.Int64)
	t.OutputTokens = int(out.Int64)
	return t, nil
}

// LastBlockingFindings returns the blocking findings from the most recent
// review step in the given phase. The engine uses it to rebuild a resume
// prompt after a restart, since findings are otherwise only held in memory.
func (s *Store) LastBlockingFindings(ctx context.Context, taskID int64, phase string) ([]Finding, error) {
	var stepID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM steps
		WHERE task_id = ? AND phase = ? AND agent = 'codex' AND state = 'done'
		ORDER BY id DESC LIMIT 1`, taskID, phase).Scan(&stepID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("last review step: %w", err)
	}

	all, err := s.ListFindings(ctx, stepID)
	if err != nil {
		return nil, err
	}
	var blocking []Finding
	for _, f := range all {
		if f.Blocking {
			blocking = append(blocking, f)
		}
	}
	return blocking, nil
}

// FailRunningSteps closes one task's still-running steps as failed. Used when
// a harness error aborts a task: leaving steps "running" would make the
// dashboard lie, and leaving the task non-terminal would have the scheduler
// re-claim it on every poll.
func (s *Store) FailRunningSteps(ctx context.Context, taskID int64, msg string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE steps SET state='failed', ended_at=?, err_msg=?
		WHERE task_id = ? AND state='running'`,
		time.Now().UTC().Format(rfc3339), msg, taskID)
	if err != nil {
		return 0, fmt.Errorf("fail running steps: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}
