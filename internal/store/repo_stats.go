package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RepoStats is what a repository has cost and produced.
//
// AgentTime and Turns lead because they are always true. The two spend figures
// are kept apart on purpose: the default claude and codex providers run against
// the operator's subscription, so what those CLIs report is what the usage
// *would* cost through the API — a usage signal, not a bill. Only a provider
// configured with its own base_url and key is actually metered to the operator.
// Adding the two together would produce a number that is neither.
type RepoStats struct {
	Tasks    int
	Running  int
	Done     int
	Failed   int
	Analyses int
	Backlog  int
	// Turns is completed agent invocations: steps plus analyses.
	Turns int
	// AgentTime is summed step and analysis wall time.
	AgentTime time.Duration
	// Reported is subscription-covered CLI usage, priced as if it had gone
	// through the API. Metered is usage against an endpoint the operator
	// supplied, which is real money.
	Reported float64
	Metered  float64
}

// Spend is the total of both figures, for the rare place that needs one number
// — never for a label that says "cost".
func (r RepoStats) Spend() float64 { return r.Reported + r.Metered }

// RepoStats folds every repository's totals in one pass per source.
//
// The folding happens in Go rather than SQL because started_at and ended_at are
// RFC3339Nano strings: SQLite's julianday() would parse some of them, silently
// return null for the rest, and produce a plausible-looking wrong number.
//
// metered names the providers whose usage is real money — those the operator
// configured with their own endpoint. Everything else is subscription-covered.
func (s *Store) RepoStats(ctx context.Context, metered map[string]bool) (map[int64]RepoStats, error) {
	out := map[int64]RepoStats{}

	if err := s.foldTaskStates(ctx, out); err != nil {
		return nil, err
	}
	if err := s.foldStepUsage(ctx, out, metered); err != nil {
		return nil, err
	}
	if err := s.foldProposalUsage(ctx, out, metered); err != nil {
		return nil, err
	}

	counts, err := s.OpenBacklogCounts(ctx)
	if err != nil {
		return nil, err
	}
	for id, n := range counts {
		st := out[id]
		st.Backlog = n
		out[id] = st
	}
	return out, nil
}

func (s *Store) foldTaskStates(ctx context.Context, out map[int64]RepoStats) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo_id, state, COUNT(*) FROM tasks WHERE repo_id <> 0
		GROUP BY repo_id, state`)
	if err != nil {
		return fmt.Errorf("count repo tasks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var state string
		var n int
		if err := rows.Scan(&id, &state, &n); err != nil {
			return fmt.Errorf("scan repo task count: %w", err)
		}
		st := out[id]
		st.Tasks += n
		switch state {
		case "running":
			st.Running += n
		case "done":
			st.Done += n
		case "failed":
			st.Failed += n
		}
		out[id] = st
	}
	return rows.Err()
}

func (s *Store) foldStepUsage(ctx context.Context, out map[int64]RepoStats, metered map[string]bool) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.repo_id, s.provider, s.cost_usd, s.started_at, s.ended_at
		FROM steps s JOIN tasks t ON t.id = s.task_id
		WHERE t.repo_id <> 0`)
	if err != nil {
		return fmt.Errorf("query repo steps: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var provider, started, ended string
		var cost float64
		if err := rows.Scan(&id, &provider, &cost, &started, &ended); err != nil {
			return fmt.Errorf("scan repo step: %w", err)
		}
		st := out[id]
		st.Turns++
		st.AgentTime += span(started, ended)
		addSpend(&st, provider, cost, metered)
		out[id] = st
	}
	return rows.Err()
}

func (s *Store) foldProposalUsage(ctx context.Context, out map[int64]RepoStats, metered map[string]bool) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo_id, provider, cost_usd, created_at, updated_at
		FROM proposals WHERE repo_id <> 0`)
	if err != nil {
		return fmt.Errorf("query repo proposals: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var provider, created, updated string
		var cost float64
		if err := rows.Scan(&id, &provider, &cost, &created, &updated); err != nil {
			return fmt.Errorf("scan repo proposal: %w", err)
		}
		st := out[id]
		st.Analyses++
		// An analysis that cost nothing never ran an agent — a draft, or one
		// still on the wizard's first screen — so it contributes no turn and no
		// time. Counting the wall clock between creating a draft and coming
		// back to it hours later as agent time would be a lie.
		if cost > 0 {
			st.Turns++
			st.AgentTime += span(created, updated)
		}
		addSpend(&st, provider, cost, metered)
		out[id] = st
	}
	return rows.Err()
}

func addSpend(st *RepoStats, provider string, cost float64, metered map[string]bool) {
	if metered[provider] {
		st.Metered += cost
		return
	}
	st.Reported += cost
}

// span is the elapsed time between two stored timestamps. An unparseable or
// missing end — a step still running, or one interrupted by a restart — counts
// as zero rather than as time since the epoch.
func span(started, ended string) time.Duration {
	if started == "" || ended == "" {
		return 0
	}
	from, err := time.Parse(rfc3339, started)
	if err != nil {
		return 0
	}
	to, err := time.Parse(rfc3339, ended)
	if err != nil {
		return 0
	}
	if to.Before(from) {
		return 0
	}
	return to.Sub(from)
}

// RepoSpend totals reported and metered usage across every repository, for the
// nav's running figure.
func (s *Store) RepoSpend(ctx context.Context, metered map[string]bool) (reported, meteredTotal float64, err error) {
	stats, err := s.RepoStats(ctx, metered)
	if err != nil {
		return 0, 0, err
	}
	for _, st := range stats {
		reported += st.Reported
		meteredTotal += st.Metered
	}
	// Work that predates repositories, or a task whose repo row was removed,
	// still cost something. Counting it keeps the nav's total honest.
	var orphan sql.NullFloat64
	err = s.db.QueryRowContext(ctx, `
		SELECT SUM(cost_usd) FROM (
			SELECT s.cost_usd FROM steps s JOIN tasks t ON t.id = s.task_id WHERE t.repo_id = 0
			UNION ALL
			SELECT cost_usd FROM proposals WHERE repo_id = 0
		)`).Scan(&orphan)
	if err != nil {
		return 0, 0, fmt.Errorf("orphan spend: %w", err)
	}
	return reported + orphan.Float64, meteredTotal, nil
}
