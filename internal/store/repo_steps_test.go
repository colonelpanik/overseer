package store

import (
	"context"
	"testing"
)

func seedTask(t *testing.T, s *Store) Task {
	t.Helper()
	task, err := s.CreateTask(context.Background(), Task{
		Slug: "t", RepoPath: "/r", Goal: "g", State: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestStartAndFinishStepWithFindings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := seedTask(t, s)

	step, err := s.StartStep(ctx, Step{
		TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "codex",
	})
	if err != nil {
		t.Fatalf("StartStep: %v", err)
	}
	if step.ID == 0 {
		t.Fatal("StartStep did not assign an ID")
	}
	if step.State != "running" {
		t.Errorf("State = %q, want running", step.State)
	}

	step.Verdict = "changes_requested"
	step.CostUSD = 0.25
	step.InputTokens = 100
	step.OutputTokens = 20
	step.TranscriptPath = "/runs/t/plan-1-codex.jsonl"
	findings := []Finding{
		{Severity: "major", File: "a.go", Line: 12, Summary: "no error check", Blocking: true},
		{Severity: "nit", Summary: "rename x", Blocking: true},
	}
	if err := s.FinishStep(ctx, step, findings); err != nil {
		t.Fatalf("FinishStep: %v", err)
	}

	steps, err := s.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("ListSteps = %d, want 1", len(steps))
	}
	if steps[0].State != "done" {
		t.Errorf("State = %q, want done", steps[0].State)
	}
	if steps[0].EndedAt.IsZero() {
		t.Error("EndedAt not set")
	}
	if steps[0].Verdict != "changes_requested" || steps[0].CostUSD != 0.25 {
		t.Errorf("step fields not saved: %+v", steps[0])
	}

	got, err := s.ListFindings(ctx, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListFindings = %d, want 2", len(got))
	}
	if got[0].Severity != "major" || got[0].Line != 12 || !got[0].Blocking {
		t.Errorf("finding not saved: %+v", got[0])
	}
}

func TestFinishStepWithErrorMarksFailed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := seedTask(t, s)

	step, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	step.ErrMsg = "exit status 1"
	step.ExitCode = 1
	if err := s.FinishStep(ctx, step, nil); err != nil {
		t.Fatal(err)
	}

	steps, err := s.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].State != "failed" {
		t.Errorf("State = %q, want failed when ErrMsg is set", steps[0].State)
	}
}

func TestInterruptRunningSteps(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := seedTask(t, s)

	if _, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	done, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep(ctx, done, nil); err != nil {
		t.Fatal(err)
	}

	n, err := s.InterruptRunningSteps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("interrupted %d steps, want 1", n)
	}

	steps, err := s.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].State != "interrupted" {
		t.Errorf("State = %q, want interrupted", steps[0].State)
	}
	if steps[1].State != "done" {
		t.Errorf("finished step was modified: %q", steps[1].State)
	}
}

func TestTaskTotalsSumsCostAndTokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := seedTask(t, s)

	for _, c := range []struct {
		cost   float64
		in, ou int
	}{{0.10, 100, 10}, {0.25, 200, 20}} {
		step, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		step.CostUSD, step.InputTokens, step.OutputTokens = c.cost, c.in, c.ou
		if err := s.FinishStep(ctx, step, nil); err != nil {
			t.Fatal(err)
		}
	}

	tot, err := s.TaskTotals(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tot.CostUSD < 0.349 || tot.CostUSD > 0.351 {
		t.Errorf("CostUSD = %v, want ~0.35", tot.CostUSD)
	}
	if tot.InputTokens != 300 || tot.OutputTokens != 30 {
		t.Errorf("tokens = %d/%d, want 300/30", tot.InputTokens, tot.OutputTokens)
	}
}

func TestFailRunningStepsOnlyTouchesThatTask(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mine := seedTask(t, s)
	other, err := s.CreateTask(ctx, Task{Slug: "other", RepoPath: "/r", Goal: "g", State: "queued"})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{mine.ID, other.ID} {
		if _, err := s.StartStep(ctx, Step{TaskID: id, Phase: "plan", Iteration: 1, Agent: "claude"}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.FailRunningSteps(ctx, mine.ID, "disk full")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("closed %d steps, want 1", n)
	}

	mineSteps, err := s.ListSteps(ctx, mine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mineSteps[0].State != "failed" || mineSteps[0].ErrMsg != "disk full" {
		t.Errorf("step not failed with a reason: %+v", mineSteps[0])
	}

	otherSteps, err := s.ListSteps(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if otherSteps[0].State != "running" {
		t.Errorf("another task's step was modified: %q", otherSteps[0].State)
	}
}

func TestLastBlockingFindingsReturnsMostRecentReviewOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := seedTask(t, s)

	older, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep(ctx, older, []Finding{
		{Severity: "major", Summary: "old finding", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}

	newer, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "plan", Iteration: 2, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep(ctx, newer, []Finding{
		{Severity: "major", Summary: "current finding", Blocking: true},
		{Severity: "nit", Summary: "not blocking here", Blocking: false},
	}); err != nil {
		t.Fatal(err)
	}

	// A review in the other phase must not leak in.
	other, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Iteration: 1, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep(ctx, other, []Finding{
		{Severity: "major", Summary: "exec finding", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastBlockingFindings(ctx, task.ID, "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Summary != "current finding" {
		t.Errorf("Summary = %q, want \"current finding\"", got[0].Summary)
	}
}

func TestLastBlockingFindingsEmptyWhenNoReviews(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := seedTask(t, s)
	got, err := s.LastBlockingFindings(ctx, task.ID, "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d findings, want 0", len(got))
	}
}
