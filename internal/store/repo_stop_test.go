package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"overseer/internal/loop"
)

func queuedTask(t *testing.T, s *Store, slug, state string) Task {
	t.Helper()
	task, err := s.CreateTask(context.Background(), Task{
		Slug: slug, RepoPath: "/r", Goal: "g", State: state,
	})
	if err != nil {
		t.Fatalf("CreateTask(%s): %v", slug, err)
	}
	return task
}

func TestStopTaskRoundTripsAndGatesClaiming(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := queuedTask(t, s, "stoppable", "planning")

	if err := s.StopTask(ctx, task.ID, true); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stopped() {
		t.Fatal("the task does not read as stopped")
	}
	// The state must survive: it names the action that was in flight, and that
	// is what loop.Pending re-dispatches when the task is started again.
	if got.State != "planning" {
		t.Errorf("State = %q, want planning — a stop must not overwrite it", got.State)
	}

	claimable, err := s.ClaimableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claimable {
		if c.ID == task.ID {
			t.Error("a stopped task is still being offered to workers")
		}
	}

	if err := s.StopTask(ctx, task.ID, false); err != nil {
		t.Fatal(err)
	}
	claimable, err = s.ClaimableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range claimable {
		found = found || c.ID == task.ID
	}
	if !found {
		t.Error("a started task is not claimable again")
	}
}

// The whole reason stopped_at is a column and not a state: a worker's full-row
// write is made from a copy read before the operator pressed anything, and it
// must not be able to revert the stop.
func TestSaveTaskCannotRevertAStop(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := queuedTask(t, s, "racy", "executing")

	// The worker's copy, read before the stop.
	stale, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.StopTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	// The worker finishes its action and writes what it computed.
	stale.Iteration = 4
	if err := s.SaveTask(ctx, stale); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stopped() {
		t.Fatal("a stale SaveTask reverted the operator's stop")
	}
	if got.Iteration != 4 {
		t.Errorf("Iteration = %d, want the worker's write to have landed", got.Iteration)
	}
}

// A task must never be both terminal and stopped: the board would render two
// contradictory things about the same row.
func TestSaveTaskClearingStopEndsTheStop(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := queuedTask(t, s, "abandoned-while-stopped", "executing")

	if err := s.StopTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	task.State = string(loop.StateAbandoned)
	if err := s.SaveTaskClearingStop(ctx, task); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stopped() {
		t.Error("the task is abandoned and still stopped")
	}
	if got.State != string(loop.StateAbandoned) {
		t.Errorf("State = %q, want abandoned", got.State)
	}
}

// abandoned is terminal, so a worker must never pick it up again — and the
// query must learn that from the state machine rather than its own literals.
func TestClaimableTasksExcludesEveryTerminalState(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, state := range loop.TerminalStateNames() {
		queuedTask(t, s, "terminal-"+state, state)
	}
	queuedTask(t, s, "live-one", "planning")

	claimable, err := s.ClaimableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimable) != 1 {
		t.Fatalf("claimable = %d tasks, want only the non-terminal one: %+v", len(claimable), claimable)
	}
	if claimable[0].Slug != "live-one" {
		t.Errorf("claimed %q, want live-one", claimable[0].Slug)
	}
}

func TestRestartTaskResetsRunStateAndBumpsTheAttempt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	task := queuedTask(t, s, "restartable", "escalated")
	task.Phase = "exec"
	task.Iteration = 7
	task.PlanSessionID = "plan-sess"
	task.ExecSessionID = "exec-sess"
	task.FindingHashes = []string{"aaa", "bbb"}
	task.ErrMsg = "oscillating"
	task.Branch = "overseer/restartable"
	task.WorktreeDir = "/wt/restartable"
	task.BlockingSeverity = "major"
	task.VerifyCommand = "make check"
	task.CostCapUSD = 4
	if err := s.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := s.StopTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}

	task.State = "queued"
	task.Phase = ""
	task.Iteration = 0
	task.MaxIterations = 10
	task.Constraints = "do not touch the schema"
	if err := s.RestartTask(ctx, task); err != nil {
		t.Fatalf("RestartTask: %v", err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunSeq != 2 {
		t.Errorf("RunSeq = %d, want 2", got.RunSeq)
	}
	if got.RunSlug() != "restartable-r2" {
		t.Errorf("RunSlug = %q, want restartable-r2", got.RunSlug())
	}
	// Everything that would make attempt 2 behave like attempt 1 is gone.
	if len(got.FindingHashes) != 0 {
		t.Errorf("FindingHashes = %v; the first review of the new run would trip oscillation at once", got.FindingHashes)
	}
	if got.PlanSessionID != "" || got.ExecSessionID != "" {
		t.Errorf("session ids survived a restart: %q / %q", got.PlanSessionID, got.ExecSessionID)
	}
	if got.Branch != "" || got.WorktreeDir != "" {
		t.Errorf("worktree paths survived: %q / %q", got.Branch, got.WorktreeDir)
	}
	if got.ErrMsg != "" || got.Stopped() {
		t.Errorf("restart left the task stopped or erroring: %+v", got)
	}
	// The operator's settings are not run state and must survive.
	if got.BlockingSeverity != "major" || got.VerifyCommand != "make check" || got.CostCapUSD != 4 {
		t.Errorf("restart discarded the operator's settings: %+v", got)
	}
	if got.Constraints != "do not touch the schema" {
		t.Errorf("Constraints = %q, want the amendment", got.Constraints)
	}
}

// The first attempt keeps the bare slug, so nothing about an unrestarted task
// changes and every branch already on disk keeps its name.
func TestRunSlugIsTheBareSlugForTheFirstAttempt(t *testing.T) {
	if got := (Task{Slug: "widget", RunSeq: 1}).RunSlug(); got != "widget" {
		t.Errorf("RunSlug = %q, want widget", got)
	}
	if got := (Task{Slug: "widget"}).RunSlug(); got != "widget" {
		t.Errorf("RunSlug with an unset RunSeq = %q, want widget", got)
	}
	if got := (Task{Slug: "widget", RunSeq: 3}).RunSlug(); got != "widget-r3" {
		t.Errorf("RunSlug = %q, want widget-r3", got)
	}
}

func TestStepsCarryTheirAttempt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task := queuedTask(t, s, "stepped", "executing")

	step, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Agent: "code", RunSeq: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep(ctx, step, nil); err != nil {
		t.Fatal(err)
	}
	steps, err := s.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].RunSeq != 2 {
		t.Fatalf("steps = %+v, want one carrying run_seq 2", steps)
	}
}

func TestInterruptTaskStepsClosesOnlyThatTasksRunningSteps(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mine := queuedTask(t, s, "mine", "executing")
	theirs := queuedTask(t, s, "theirs", "executing")

	for _, task := range []Task{mine, theirs} {
		if _, err := s.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Agent: "code"}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.InterruptTaskSteps(ctx, mine.ID, "stopped by the operator")
	if err != nil {
		t.Fatalf("InterruptTaskSteps: %v", err)
	}
	if n != 1 {
		t.Errorf("closed %d steps, want 1", n)
	}

	got, err := s.ListSteps(ctx, mine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != "interrupted" {
		t.Errorf("state = %q, want interrupted — the step was taken away, not failed", got[0].State)
	}
	if got[0].ErrMsg != "stopped by the operator" {
		t.Errorf("ErrMsg = %q, want the reason", got[0].ErrMsg)
	}
	if got[0].EndedAt.IsZero() {
		t.Error("ended_at not set; the dashboard would show it running forever")
	}

	other, err := s.ListSteps(ctx, theirs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other[0].State != "running" {
		t.Errorf("another task's step = %q, want it untouched", other[0].State)
	}
}

func TestStopAndRestartOnAMissingTask(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.StopTask(ctx, 404, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("StopTask err = %v, want ErrNotFound", err)
	}
	if err := s.RestartTask(ctx, Task{ID: 404}); !errors.Is(err, ErrNotFound) {
		t.Errorf("RestartTask err = %v, want ErrNotFound", err)
	}
}

func TestSettingsRoundTripAndClear(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// A key never written reads as empty, so a fresh database needs no seeding.
	got, err := s.Setting(ctx, SettingStopAll)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("unset setting = %q, want empty", got)
	}

	if err := s.SetSetting(ctx, SettingStopAll, "stopped by the operator"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Setting(ctx, SettingStopAll); got != "stopped by the operator" {
		t.Errorf("setting = %q, want the reason", got)
	}

	// Writing twice updates rather than failing on the primary key.
	if err := s.SetSetting(ctx, SettingStopAll, "second reason"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ = s.Setting(ctx, SettingStopAll); got != "second reason" {
		t.Errorf("setting = %q, want the second reason", got)
	}

	if err := s.SetSetting(ctx, SettingStopAll, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Setting(ctx, SettingStopAll); got != "" {
		t.Errorf("cleared setting = %q, want empty", got)
	}
}

// A database written before stops existed must open with every task startable
// and on its first attempt — not stopped, and not on attempt zero, either of
// which would make existing work invisible to the scheduler.
func TestMigrationLeavesOldTasksStartableOnTheirFirstAttempt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	preRepoDB(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open pre-stop db: %v", err)
	}
	defer s.Close()

	tasks, err := s.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("the fixture produced no tasks")
	}
	for _, task := range tasks {
		if task.Stopped() {
			t.Errorf("task %s came back stopped after the migration", task.Slug)
		}
		if task.RunSeq != 1 {
			t.Errorf("task %s has run_seq %d, want 1", task.Slug, task.RunSeq)
		}
		if task.RunSlug() != task.Slug {
			t.Errorf("task %s changed its run slug to %q; every branch on disk would be orphaned",
				task.Slug, task.RunSlug())
		}
	}
}
