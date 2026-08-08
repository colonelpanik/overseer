package store

import (
	"context"
	"testing"
	"time"
)

// finishStepAt closes a step with a chosen duration and cost, so the folding
// can be checked against known numbers rather than against wall time.
func finishStepAt(t *testing.T, s *Store, st Step, d time.Duration, cost float64) {
	t.Helper()
	ctx := context.Background()
	if err := s.FinishStep(ctx, st, nil); err != nil {
		t.Fatalf("FinishStep: %v", err)
	}
	start := time.Now().UTC().Add(-d)
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE steps SET started_at = ?, ended_at = ?, cost_usd = ? WHERE id = ?`,
		start.Format(rfc3339), start.Add(d).Format(rfc3339), cost, st.ID); err != nil {
		t.Fatalf("set step timings: %v", err)
	}
}

func TestRepoStatsFoldsTasksStepsAndAnalyses(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")
	other := testRepo(t, s, "/src/gadget")

	for _, spec := range []struct{ slug, state string }{
		{"a", "done"}, {"b", "done"}, {"c", "running"}, {"d", "failed"}, {"e", "queued"},
	} {
		task, err := s.CreateTask(ctx, Task{
			Slug: spec.slug, RepoID: repo.ID, RepoPath: repo.Path,
			Goal: "g", State: spec.state,
		})
		if err != nil {
			t.Fatal(err)
		}
		step, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Agent: "code", Provider: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		finishStepAt(t, s, step, 2*time.Minute, 0.50)
	}

	// A task on another repository must not land in this one's totals.
	elsewhere, err := s.CreateTask(ctx, Task{
		Slug: "z", RepoID: other.ID, RepoPath: other.Path, Goal: "g", State: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := s.StartStep(ctx, Step{TaskID: elsewhere.ID, Phase: "exec", Agent: "code", Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	finishStepAt(t, s, step, time.Minute, 9.00)

	if _, err := s.CreateProposal(ctx, Proposal{
		RepoID: repo.ID, RepoPath: repo.Path, State: ProposalReady,
		Provider: "claude", CostUSD: 1.25,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddBacklogItem(ctx, BacklogItem{
		RepoID: repo.ID, Source: BacklogReview, Title: "an open item",
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := s.RepoStats(ctx, nil)
	if err != nil {
		t.Fatalf("RepoStats: %v", err)
	}
	got := stats[repo.ID]

	if got.Tasks != 5 || got.Done != 2 || got.Running != 1 || got.Failed != 1 {
		t.Errorf("task counts = %+v, want 5 tasks / 2 done / 1 running / 1 failed", got)
	}
	if got.Analyses != 1 {
		t.Errorf("Analyses = %d, want 1", got.Analyses)
	}
	if got.Backlog != 1 {
		t.Errorf("Backlog = %d, want 1", got.Backlog)
	}
	// Five steps plus the analysis.
	if got.Turns != 6 {
		t.Errorf("Turns = %d, want 6", got.Turns)
	}
	if got.AgentTime < 10*time.Minute {
		t.Errorf("AgentTime = %v, want at least the 10 minutes of step time", got.AgentTime)
	}
	if diff := got.Reported - 3.75; diff < -0.001 || diff > 0.001 {
		t.Errorf("Reported = %v, want 3.75 (5 × 0.50 + 1.25)", got.Reported)
	}
	if got.Metered != 0 {
		t.Errorf("Metered = %v, want 0 — no configured endpoint served any of this", got.Metered)
	}
	if stats[other.ID].Reported != 9.00 {
		t.Errorf("the other repo's spend = %v, want 9.00 kept separate", stats[other.ID].Reported)
	}
}

// The correction the whole split exists for: subscription-covered CLI usage is
// a usage signal, not a bill, and must never be added to real metered money.
func TestRepoStatsKeepsMeteredSpendApartFromReported(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	task, err := s.CreateTask(ctx, Task{
		Slug: "a", RepoID: repo.ID, RepoPath: repo.Path, Goal: "g", State: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		provider string
		cost     float64
	}{
		{"claude", 2.00},  // subscription
		{"inhouse", 0.30}, // an endpoint the operator configured
	} {
		step, err := s.StartStep(ctx, Step{
			TaskID: task.ID, Phase: "exec", Agent: "code", Provider: spec.provider,
		})
		if err != nil {
			t.Fatal(err)
		}
		finishStepAt(t, s, step, time.Minute, spec.cost)
	}

	stats, err := s.RepoStats(ctx, map[string]bool{"inhouse": true})
	if err != nil {
		t.Fatal(err)
	}
	got := stats[repo.ID]
	if got.Reported != 2.00 {
		t.Errorf("Reported = %v, want 2.00", got.Reported)
	}
	if got.Metered != 0.30 {
		t.Errorf("Metered = %v, want 0.30", got.Metered)
	}
	if diff := got.Spend() - 2.30; diff < -0.001 || diff > 0.001 {
		t.Errorf("Spend() = %v, want 2.30", got.Spend())
	}
}

// A step interrupted by a restart has no end. Counting it as time since the
// epoch would make one crash dwarf every real number on the page.
func TestRepoStatsIgnoresUnfinishedAndCorruptTimestamps(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	task, err := s.CreateTask(ctx, Task{
		Slug: "a", RepoID: repo.ID, RepoPath: repo.Path, Goal: "g", State: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Still running: no ended_at at all.
	if _, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Agent: "code"}); err != nil {
		t.Fatal(err)
	}
	// Finished, but with a timestamp nothing can parse.
	bad, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Agent: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep(ctx, bad, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE steps SET ended_at = 'not-a-time' WHERE id = ?`, bad.ID); err != nil {
		t.Fatal(err)
	}

	stats, err := s.RepoStats(ctx, nil)
	if err != nil {
		t.Fatalf("RepoStats: %v", err)
	}
	if got := stats[repo.ID].AgentTime; got < 0 || got > time.Minute {
		t.Errorf("AgentTime = %v, want ~0 rather than time since the epoch", got)
	}
}

// A draft nobody ran costs nothing and takes no agent time, however long it sat
// on the wizard's first screen.
func TestRepoStatsDoesNotCountAnUnrunDraftAsAgentTime(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	p, err := s.CreateProposal(ctx, Proposal{
		RepoID: repo.ID, RepoPath: repo.Path, State: ProposalDraft,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Created an hour ago, touched now — but never run.
	old := time.Now().UTC().Add(-time.Hour).Format(rfc3339)
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE proposals SET created_at = ? WHERE id = ?`, old, p.ID); err != nil {
		t.Fatal(err)
	}

	stats, err := s.RepoStats(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := stats[repo.ID]
	if got.Analyses != 1 {
		t.Errorf("Analyses = %d, want 1 — the draft still happened", got.Analyses)
	}
	if got.AgentTime != 0 {
		t.Errorf("AgentTime = %v, want 0 for a draft that never ran an agent", got.AgentTime)
	}
	if got.Turns != 0 {
		t.Errorf("Turns = %d, want 0", got.Turns)
	}
}

// Work from before repositories existed still cost something. The nav's total
// has to include it, or upgrading appears to erase history.
func TestRepoSpendIncludesUnattributedWork(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := testRepo(t, s, "/src/widget")

	attributed, err := s.CreateTask(ctx, Task{
		Slug: "a", RepoID: repo.ID, RepoPath: repo.Path, Goal: "g", State: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := s.StartStep(ctx, Step{TaskID: attributed.ID, Phase: "exec", Agent: "code"})
	if err != nil {
		t.Fatal(err)
	}
	finishStepAt(t, s, step, time.Minute, 1.00)

	orphan, err := s.CreateTask(ctx, Task{Slug: "b", RepoPath: "/gone", Goal: "g", State: "done"})
	if err != nil {
		t.Fatal(err)
	}
	orphanStep, err := s.StartStep(ctx, Step{TaskID: orphan.ID, Phase: "exec", Agent: "code"})
	if err != nil {
		t.Fatal(err)
	}
	finishStepAt(t, s, orphanStep, time.Minute, 2.00)

	reported, metered, err := s.RepoSpend(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := reported - 3.00; diff < -0.001 || diff > 0.001 {
		t.Errorf("reported = %v, want 3.00 including the unattributed step", reported)
	}
	if metered != 0 {
		t.Errorf("metered = %v, want 0", metered)
	}
}
