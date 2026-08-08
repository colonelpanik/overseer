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
	ID        int64
	TaskID    int64
	Phase     string
	Iteration int
	Agent     string
	// Provider is the configured provider that served this step. It is what
	// tells subscription-covered CLI usage apart from usage metered against an
	// endpoint the operator supplied, after the fact.
	Provider string
	// RunSeq is which attempt of the task this step belongs to, so a restarted
	// task's history stays separable from the attempt before it.
	RunSeq int
	// Interrupted says this step was taken away rather than having failed —
	// the operator stopped the task, or the daemon shut down. Both leave an
	// ErrMsg saying the process was killed, which is indistinguishable from a
	// crash, so the caller has to say which it was.
	Interrupted    bool
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
		INSERT INTO steps (task_id, phase, iteration, agent, provider, run_seq,
			state, started_at, transcript_path)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		st.TaskID, st.Phase, st.Iteration, st.Agent, st.Provider, st.RunSeq,
		st.State, st.StartedAt.Format(rfc3339), st.TranscriptPath)
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
	switch {
	case st.Interrupted:
		// Not "failed": the step did not fail, it was stopped. A task the
		// operator parked must not read on the timeline as one that broke.
		st.State = "interrupted"
	case st.ErrMsg != "":
		st.State = "failed"
	default:
		st.State = "done"
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

const stepColumns = `id, task_id, phase, iteration, agent, provider, run_seq,
	state, started_at, ended_at, exit_code, verdict, input_tokens,
	output_tokens, cost_usd, transcript_path, err_msg`

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
			&st.Agent, &st.Provider, &st.RunSeq, &st.State, &started, &ended,
			&st.ExitCode, &st.Verdict, &st.InputTokens, &st.OutputTokens,
			&st.CostUSD, &st.TranscriptPath, &st.ErrMsg); err != nil {
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

// AllFindings returns every finding on a task, keyed by step ID. The dashboard
// renders a whole task at once, and asking per step is one statement per step.
func (s *Store) AllFindings(ctx context.Context, taskID int64) (map[int64][]Finding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.step_id, f.severity, f.file, f.line, f.summary, f.detail, f.blocking
		FROM findings f
		JOIN steps s ON s.id = f.step_id
		WHERE s.task_id = ?
		ORDER BY f.id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query task findings: %w", err)
	}
	defer rows.Close()

	out := map[int64][]Finding{}
	for rows.Next() {
		var f Finding
		var blocking int
		if err := rows.Scan(&f.ID, &f.StepID, &f.Severity, &f.File, &f.Line,
			&f.Summary, &f.Detail, &blocking); err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		f.Blocking = blocking == 1
		out[f.StepID] = append(out[f.StepID], f)
	}
	return out, rows.Err()
}

// ReviewRound is one review step — a Codex review or a verify run — with the
// blocking findings it raised. A verdict is what makes a step a review: Claude
// turns never carry one.
type ReviewRound struct {
	TaskID    int64
	Phase     string
	Iteration int
	Agent     string
	Blocking  []string
}

// AllReviewRounds returns every task's review rounds in order, in one
// statement. The board draws a convergence sparkline on every card, and doing
// this per card would be two queries per task on every state event.
func (s *Store) AllReviewRounds(ctx context.Context) (map[int64][]ReviewRound, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.task_id, s.id, s.phase, s.iteration, s.agent, f.summary
		FROM steps s
		LEFT JOIN findings f ON f.step_id = s.id AND f.blocking = 1
		WHERE s.verdict <> ''
		ORDER BY s.task_id ASC, s.id ASC, f.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query review rounds: %w", err)
	}
	defer rows.Close()

	out := map[int64][]ReviewRound{}
	var lastStep int64
	for rows.Next() {
		var taskID, stepID int64
		var phase, agent string
		var iteration int
		var summary sql.NullString
		if err := rows.Scan(&taskID, &stepID, &phase, &iteration, &agent, &summary); err != nil {
			return nil, fmt.Errorf("scan review round: %w", err)
		}
		// The LEFT JOIN gives one row per finding, or a single null row for a
		// review that raised nothing — which is exactly the round that has to
		// stay in the series, because a clean round is the signal.
		if stepID != lastStep {
			out[taskID] = append(out[taskID], ReviewRound{
				TaskID: taskID, Phase: phase, Iteration: iteration, Agent: agent,
			})
			lastStep = stepID
		}
		if summary.Valid {
			list := out[taskID]
			last := &list[len(list)-1]
			last.Blocking = append(last.Blocking, summary.String)
		}
	}
	return out, rows.Err()
}

// AllTotals sums cost and tokens for every task in one statement. The board
// shows spend on every card, and TaskTotals per card is a query per card.
func (s *Store) AllTotals(ctx context.Context) (map[int64]Totals, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, SUM(cost_usd), SUM(input_tokens), SUM(output_tokens)
		FROM steps GROUP BY task_id`)
	if err != nil {
		return nil, fmt.Errorf("all totals: %w", err)
	}
	defer rows.Close()

	out := map[int64]Totals{}
	for rows.Next() {
		var id int64
		var cost sql.NullFloat64
		var in, o sql.NullInt64
		if err := rows.Scan(&id, &cost, &in, &o); err != nil {
			return nil, fmt.Errorf("scan totals: %w", err)
		}
		out[id] = Totals{
			CostUSD:      cost.Float64,
			InputTokens:  int(in.Int64),
			OutputTokens: int(o.Int64),
		}
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
// completed step in the given phase that actually recorded any, whatever
// produced them. The engine uses it to rebuild a resume prompt after a
// restart, since findings are otherwise only held in memory.
//
// This deliberately does not filter by agent. A verify failure is stored with
// Agent "verify", not "codex", and loop.afterVerify returns the same
// ActClaudeExecResume a Codex review does; filtering to 'codex' here made
// those findings invisible to recovery — a restart mid-verify-fix found
// nothing, fell through to a fresh Claude session that had no idea a test was
// failing, and burned an iteration to rediscover what the daemon already
// knew. It also does not simply take the latest 'done' step regardless of
// content: the step immediately before a crash could be the very Claude turn
// that was resuming a review, itself done but never reviewed, and that step
// carries no findings of its own — the EXISTS clause skips straight past it
// to the last step that actually has some.
func (s *Store) LastBlockingFindings(ctx context.Context, taskID int64, phase string) ([]Finding, error) {
	var stepID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM steps s
		WHERE s.task_id = ? AND s.phase = ? AND s.state = 'done'
			AND EXISTS (
				SELECT 1 FROM findings f WHERE f.step_id = s.id AND f.blocking = 1
			)
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

// InterruptTaskSteps closes one task's still-running steps as interrupted.
//
// Used when the operator stops a task. The step did not fail — it was taken
// away mid-run — and "interrupted" is the word Recover already uses for exactly
// that. Leaving it "running" is what makes the dashboard show a live pane that
// never resolves, and unlike a crash there is no restart coming to sweep it.
func (s *Store) InterruptTaskSteps(ctx context.Context, taskID int64, msg string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE steps SET state='interrupted', ended_at=?, err_msg=?
		WHERE task_id = ? AND state='running'`,
		time.Now().UTC().Format(rfc3339), msg, taskID)
	if err != nil {
		return 0, fmt.Errorf("interrupt steps for task %d: %w", taskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}
