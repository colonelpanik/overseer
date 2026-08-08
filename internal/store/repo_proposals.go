package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Proposal states.
const (
	// ProposalDraft is the wizard's first screen: a source has been chosen but
	// nothing has run.
	ProposalDraft = "draft"
	// ProposalCloning is a URL import fetching the repository.
	ProposalCloning = "cloning"
	// ProposalAnalysing has an agent reading the repository.
	ProposalAnalysing = "analysing"
	// ProposalReady has a task list waiting to be reviewed.
	ProposalReady = "ready"
	// ProposalQueued means the selection was turned into real tasks.
	ProposalQueued = "queued"
	// ProposalDiscarded was thrown away by the operator.
	ProposalDiscarded = "discarded"
	// ProposalFailed could not be cloned, analysed or parsed.
	ProposalFailed = "failed"
)

// Proposal is one repository analysis: what was asked for, what it cost, and
// what state the wizard is in.
type Proposal struct {
	ID        int64
	RepoPath  string
	SourceURL string
	State     string
	// Focus is the operator's chosen focus areas, one per line.
	Focus []string
	// Notes is free text steering, passed to the agent verbatim.
	Notes    string
	MaxTasks int
	Model    string
	// Detected is what the repository probe found: language, test command,
	// default branch. Shown on the first screen so the operator can see the
	// wizard understood the repo before paying for an analysis.
	Detected       string
	CostUSD        float64
	InputTokens    int
	OutputTokens   int
	TranscriptPath string
	ErrMsg         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ProposalTask is one proposed task, before it becomes a real one.
type ProposalTask struct {
	ID         int64
	ProposalID int64
	Ord        int
	// Key is the model's local name for this task. DependsOn refers to these,
	// not to slugs: the real slug is not known until Submit creates the task,
	// and it may pick up a collision suffix.
	Key         string
	Goal        string
	Constraints []string
	Verify      string
	Severity    string
	CostCap     float64
	DependsOn   []string
	// Rationale is why the analysis proposed this, and Evidence is the
	// file:line list backing it up. Both exist so a proposal can be judged
	// rather than taken on faith.
	Rationale     string
	Evidence      []string
	Selected      bool
	CreatedTaskID int64
}

const proposalColumns = `id, repo_path, source_url, state, focus, notes,
	max_tasks, model, detected, cost_usd, input_tokens, output_tokens,
	transcript_path, err_msg, created_at, updated_at`

// CreateProposal inserts p and returns it with its ID and timestamps.
func (s *Store) CreateProposal(ctx context.Context, p Proposal) (Proposal, error) {
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	if p.State == "" {
		p.State = ProposalDraft
	}
	if p.MaxTasks == 0 {
		p.MaxTasks = 12
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO proposals (repo_path, source_url, state, focus, notes,
			max_tasks, model, detected, cost_usd, input_tokens, output_tokens,
			transcript_path, err_msg, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.RepoPath, p.SourceURL, p.State, strings.Join(p.Focus, "\n"), p.Notes,
		p.MaxTasks, p.Model, p.Detected, p.CostUSD, p.InputTokens, p.OutputTokens,
		p.TranscriptPath, p.ErrMsg,
		p.CreatedAt.Format(rfc3339), p.UpdatedAt.Format(rfc3339))
	if err != nil {
		return Proposal{}, fmt.Errorf("insert proposal: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Proposal{}, fmt.Errorf("proposal id: %w", err)
	}
	p.ID = id
	return p, nil
}

func scanProposal(sc interface{ Scan(...any) error }) (Proposal, error) {
	var p Proposal
	var focus, created, updated string
	err := sc.Scan(&p.ID, &p.RepoPath, &p.SourceURL, &p.State, &focus, &p.Notes,
		&p.MaxTasks, &p.Model, &p.Detected, &p.CostUSD, &p.InputTokens,
		&p.OutputTokens, &p.TranscriptPath, &p.ErrMsg, &created, &updated)
	if err != nil {
		return Proposal{}, err
	}
	if focus != "" {
		p.Focus = strings.Split(focus, "\n")
	}
	p.CreatedAt, err = time.Parse(rfc3339, created)
	if err != nil {
		return Proposal{}, fmt.Errorf("parse proposal created_at: %w", err)
	}
	p.UpdatedAt, err = time.Parse(rfc3339, updated)
	if err != nil {
		return Proposal{}, fmt.Errorf("parse proposal updated_at: %w", err)
	}
	return p, nil
}

// GetProposal loads one proposal by ID.
func (s *Store) GetProposal(ctx context.Context, id int64) (Proposal, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, id)
	p, err := scanProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("proposal %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("get proposal: %w", err)
	}
	return p, nil
}

// SaveProposal writes every mutable field and bumps updated_at.
func (s *Store) SaveProposal(ctx context.Context, p Proposal) error {
	p.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE proposals SET repo_path=?, source_url=?, state=?, focus=?,
			notes=?, max_tasks=?, model=?, detected=?, cost_usd=?,
			input_tokens=?, output_tokens=?, transcript_path=?, err_msg=?,
			updated_at=?
		WHERE id=?`,
		p.RepoPath, p.SourceURL, p.State, strings.Join(p.Focus, "\n"), p.Notes,
		p.MaxTasks, p.Model, p.Detected, p.CostUSD, p.InputTokens,
		p.OutputTokens, p.TranscriptPath, p.ErrMsg,
		p.UpdatedAt.Format(rfc3339), p.ID)
	if err != nil {
		return fmt.Errorf("update proposal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("proposal %d: %w", p.ID, ErrNotFound)
	}
	return nil
}

// ListProposals returns proposals still worth showing, newest first. Queued
// and discarded ones are excluded: they are history, and the wizard is about
// what is in front of the operator now.
func (s *Store) ListProposals(ctx context.Context) ([]Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+proposalColumns+`
		FROM proposals
		WHERE state NOT IN ('queued', 'discarded')
		ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("query proposals: %w", err)
	}
	defer rows.Close()

	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan proposal: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProposalHistory is one past analysis plus how much of it has been acted on.
type ProposalHistory struct {
	Proposal
	// Tasks is how many tasks the analysis proposed, Queued how many have
	// become real tasks. The gap is what is still available to act on, which
	// is the whole reason a finished analysis is worth keeping.
	Tasks  int
	Queued int
}

// AllProposals returns every analysis, newest first, with its task counts.
//
// Unlike ListProposals this includes the finished ones: an analysis whose
// tasks were queued is exactly the thing an operator comes back to when they
// want the rest of the list, and one that was discarded is still a record of
// what was looked at and what it cost.
func (s *Store) AllProposals(ctx context.Context, limit int) ([]ProposalHistory, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+proposalColumns+`,
			(SELECT COUNT(*) FROM proposal_tasks t WHERE t.proposal_id = proposals.id),
			(SELECT COUNT(*) FROM proposal_tasks t WHERE t.proposal_id = proposals.id AND t.created_task_id <> 0)
		FROM proposals
		ORDER BY id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query proposal history: %w", err)
	}
	defer rows.Close()

	var out []ProposalHistory
	for rows.Next() {
		var h ProposalHistory
		// scanProposal reads the shared column list; the two counts are
		// appended after it, so they are scanned by a wrapper that forwards
		// the leading destinations through.
		sc := &appendScanner{rows: rows, extra: []any{&h.Tasks, &h.Queued}}
		p, err := scanProposal(sc)
		if err != nil {
			return nil, fmt.Errorf("scan proposal history: %w", err)
		}
		h.Proposal = p
		out = append(out, h)
	}
	return out, rows.Err()
}

// appendScanner lets scanProposal be reused for a query that selects extra
// trailing columns, rather than duplicating the column list and its scan.
type appendScanner struct {
	rows  interface{ Scan(...any) error }
	extra []any
}

func (a *appendScanner) Scan(dest ...any) error {
	return a.rows.Scan(append(dest, a.extra...)...)
}

// ProposalSpend is what every analysis has cost in total. The dashboard's run
// spend has to include it, or the figure under-reports real money.
func (s *Store) ProposalSpend(ctx context.Context) (float64, error) {
	var cost sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `SELECT SUM(cost_usd) FROM proposals`).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("proposal spend: %w", err)
	}
	return cost.Float64, nil
}

// FailStrandedProposals parks analyses left mid-run by a daemon restart.
//
// An analysis is a goroutine, not a claimable task, so nothing picks it back
// up. Left alone the proposal says "analysing" forever and the wizard shows a
// spinner that never resolves — the same lie InterruptRunningSteps exists to
// prevent for task steps.
func (s *Store) FailStrandedProposals(ctx context.Context, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE proposals SET state = ?, err_msg = ?, updated_at = ?
		WHERE state IN (?, ?)`,
		ProposalFailed, reason, time.Now().UTC().Format(rfc3339),
		ProposalAnalysing, ProposalCloning)
	if err != nil {
		return 0, fmt.Errorf("fail stranded proposals: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

const proposalTaskColumns = `id, proposal_id, ord, key, goal, constraints,
	verify, severity, cost_cap, depends_on, rationale, evidence, selected,
	created_task_id`

// ReplaceProposalTasks swaps in a fresh task list, which is what a regenerate
// does. It is one transaction: a half-replaced list would show the operator a
// mix of two different analyses.
func (s *Store) ReplaceProposalTasks(ctx context.Context, proposalID int64, tasks []ProposalTask) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin proposal tasks tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM proposal_tasks WHERE proposal_id = ?`, proposalID); err != nil {
		return fmt.Errorf("clear proposal tasks: %w", err)
	}
	for i, t := range tasks {
		selected := 0
		if t.Selected {
			selected = 1
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO proposal_tasks (proposal_id, ord, key, goal, constraints,
				verify, severity, cost_cap, depends_on, rationale, evidence,
				selected, created_task_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			proposalID, i, t.Key, t.Goal, strings.Join(t.Constraints, "\n"),
			t.Verify, t.Severity, t.CostCap, strings.Join(t.DependsOn, "\n"),
			t.Rationale, strings.Join(t.Evidence, "\n"), selected, t.CreatedTaskID)
		if err != nil {
			return fmt.Errorf("insert proposal task %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit proposal tasks: %w", err)
	}
	return nil
}

// ProposalTasks returns one proposal's tasks in display order.
func (s *Store) ProposalTasks(ctx context.Context, proposalID int64) ([]ProposalTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+proposalTaskColumns+`
		FROM proposal_tasks WHERE proposal_id = ? ORDER BY ord ASC`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("query proposal tasks: %w", err)
	}
	defer rows.Close()

	var out []ProposalTask
	for rows.Next() {
		var t ProposalTask
		var constraints, dependsOn, evidence string
		var selected int
		if err := rows.Scan(&t.ID, &t.ProposalID, &t.Ord, &t.Key, &t.Goal,
			&constraints, &t.Verify, &t.Severity, &t.CostCap, &dependsOn,
			&t.Rationale, &evidence, &selected, &t.CreatedTaskID); err != nil {
			return nil, fmt.Errorf("scan proposal task: %w", err)
		}
		t.Constraints = splitLines(constraints)
		t.DependsOn = splitLines(dependsOn)
		t.Evidence = splitLines(evidence)
		t.Selected = selected == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetProposalTask loads one proposed task, checked against its proposal so a
// hand-edited URL cannot reach a row belonging to a different analysis.
func (s *Store) GetProposalTask(ctx context.Context, proposalID, id int64) (ProposalTask, error) {
	tasks, err := s.ProposalTasks(ctx, proposalID)
	if err != nil {
		return ProposalTask{}, err
	}
	for _, t := range tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return ProposalTask{}, fmt.Errorf("proposal task %d: %w", id, ErrNotFound)
}

// SaveProposalTask writes the fields the review step lets an operator edit.
// Key and Ord are not among them: they are what depends_on and the display
// order are built from.
func (s *Store) SaveProposalTask(ctx context.Context, t ProposalTask) error {
	selected := 0
	if t.Selected {
		selected = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE proposal_tasks SET goal=?, constraints=?, verify=?, severity=?,
			cost_cap=?, selected=?, created_task_id=?
		WHERE id=? AND proposal_id=?`,
		t.Goal, strings.Join(t.Constraints, "\n"), t.Verify, t.Severity,
		t.CostCap, selected, t.CreatedTaskID, t.ID, t.ProposalID)
	if err != nil {
		return fmt.Errorf("update proposal task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("proposal task %d: %w", t.ID, ErrNotFound)
	}
	return nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
