package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func depStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mkTask(t *testing.T, st *Store, slug, state string) Task {
	t.Helper()
	task, err := st.CreateTask(context.Background(), Task{
		Slug: slug, RepoPath: "/r", Goal: "g", State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func claimedSlugs(t *testing.T, st *Store) []string {
	t.Helper()
	tasks, err := st.ClaimableTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, task := range tasks {
		out = append(out, task.Slug)
	}
	return out
}

func TestClaimableTasksHoldsBackAQueuedTaskWithAnUnmetDependency(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	first := mkTask(t, st, "first", "queued")
	second := mkTask(t, st, "second", "queued")

	if err := st.SetTaskDeps(ctx, second.ID, []int64{first.ID}); err != nil {
		t.Fatal(err)
	}
	if got := claimedSlugs(t, st); strings.Join(got, ",") != "first" {
		t.Errorf("claimable = %v, want only first while its dependency is unmet", got)
	}

	first.State = "done"
	if err := st.SaveTask(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := claimedSlugs(t, st); strings.Join(got, ",") != "second" {
		t.Errorf("claimable = %v, want second once its dependency is done", got)
	}
}

func TestClaimableTasksDoesNotYankATaskAlreadyInFlight(t *testing.T) {
	// The gate is a starting condition, not an invariant. A dependency that
	// regresses after the dependent has a worktree must not pull a running
	// task back out of the pool halfway through a phase.
	st := depStore(t)
	ctx := context.Background()
	dep := mkTask(t, st, "dep", "queued")
	live := mkTask(t, st, "live", "executing")
	if err := st.SetTaskDeps(ctx, live.ID, []int64{dep.ID}); err != nil {
		t.Fatal(err)
	}

	got := claimedSlugs(t, st)
	if len(got) != 2 {
		t.Errorf("claimable = %v, want both: the executing task is past the gate", got)
	}
}

func TestSetTaskDepsRejectsSelfReferenceAndCycles(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	a := mkTask(t, st, "a", "queued")
	b := mkTask(t, st, "b", "queued")
	c := mkTask(t, st, "c", "queued")

	if err := st.SetTaskDeps(ctx, a.ID, []int64{a.ID}); err == nil {
		t.Error("a task depending on itself should be rejected")
	}
	if err := st.SetTaskDeps(ctx, b.ID, []int64{a.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskDeps(ctx, c.ID, []int64{b.ID}); err != nil {
		t.Fatal(err)
	}
	// a -> c would close a -> c -> b -> a.
	if err := st.SetTaskDeps(ctx, a.ID, []int64{c.ID}); err == nil {
		t.Error("closing a dependency cycle should be rejected")
	}
	// The rejected write must not have partially applied.
	deps, err := st.TaskDeps(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("a's deps = %v, want none after the rejected write", deps)
	}
}

func TestSetTaskDepsReplacesRatherThanAppends(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	a := mkTask(t, st, "a", "queued")
	b := mkTask(t, st, "b", "queued")
	c := mkTask(t, st, "c", "queued")

	if err := st.SetTaskDeps(ctx, c.ID, []int64{a.ID, b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	deps, err := st.TaskDeps(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 {
		t.Errorf("deps = %v, want the duplicate collapsed", deps)
	}
	// Clearing is how the dashboard's "release anyway" works.
	if err := st.SetTaskDeps(ctx, c.ID, nil); err != nil {
		t.Fatal(err)
	}
	if deps, _ := st.TaskDeps(ctx, c.ID); len(deps) != 0 {
		t.Errorf("deps = %v, want none after clearing", deps)
	}
}

func TestMigrationAddsTheCostCapToAnOlderDatabase(t *testing.T) {
	// schema.sql creates the column on a fresh database, but CREATE TABLE IF
	// NOT EXISTS is a no-op against one an older build already made — so
	// without the ALTER, upgrading in place breaks every task query.
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT, slug TEXT NOT NULL UNIQUE,
		repo_path TEXT NOT NULL, goal TEXT NOT NULL,
		constraints TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
		phase TEXT NOT NULL DEFAULT '', iteration INTEGER NOT NULL DEFAULT 0,
		max_iterations INTEGER NOT NULL DEFAULT 10,
		blocking_severity TEXT NOT NULL DEFAULT 'any',
		plan_session_id TEXT NOT NULL DEFAULT '', exec_session_id TEXT NOT NULL DEFAULT '',
		branch TEXT NOT NULL DEFAULT '', base_ref TEXT NOT NULL DEFAULT '',
		git_common_dir TEXT NOT NULL DEFAULT '', git_admin_dir TEXT NOT NULL DEFAULT '',
		worktree_dir TEXT NOT NULL DEFAULT '', pr_url TEXT NOT NULL DEFAULT '',
		err_msg TEXT NOT NULL DEFAULT '', verify_command TEXT NOT NULL DEFAULT '',
		finding_hashes TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO tasks (slug, repo_path, goal, state, created_at, updated_at)
		VALUES ('legacy','/r','g','queued','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("opening a pre-cap database should migrate it: %v", err)
	}
	defer st.Close()

	task, err := st.GetTask(context.Background(), 1)
	if err != nil {
		t.Fatalf("reading a migrated row: %v", err)
	}
	if task.Slug != "legacy" || task.CostCapUSD != 0 {
		t.Errorf("task = %+v, want the legacy row with a zero cap", task)
	}

	// Opening again must be a no-op rather than a duplicate-column error.
	st.Close()
	if st2, err := Open(path); err != nil {
		t.Errorf("reopening a migrated database: %v", err)
	} else {
		st2.Close()
	}
}

func TestAllReviewRoundsKeepsCleanRoundsInTheSeries(t *testing.T) {
	// A round that raised nothing is the convergence signal. Dropping it —
	// which an INNER JOIN would — turns a converging task into a flat line.
	st := depStore(t)
	ctx := context.Background()
	task := mkTask(t, st, "t", "executing")

	for i, findings := range [][]Finding{
		{{Severity: "major", Summary: "first", Blocking: true},
			{Severity: "nit", Summary: "not blocking", Blocking: false}},
		{},
		{{Severity: "major", Summary: "first", Blocking: true}},
	} {
		step, err := st.StartStep(ctx, Step{
			TaskID: task.ID, Phase: "exec", Iteration: i + 1, Agent: "codex",
		})
		if err != nil {
			t.Fatal(err)
		}
		step.Verdict = "changes_requested"
		if len(findings) == 0 {
			step.Verdict = "approved"
		}
		if err := st.FinishStep(ctx, step, findings); err != nil {
			t.Fatal(err)
		}
	}
	// A Claude turn carries no verdict and must not appear as a round.
	claude, err := st.StartStep(ctx, Step{TaskID: task.ID, Phase: "exec", Iteration: 4, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishStep(ctx, claude, nil); err != nil {
		t.Fatal(err)
	}

	rounds, err := st.AllReviewRounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := rounds[task.ID]
	if len(got) != 3 {
		t.Fatalf("rounds = %d, want 3 (the clean round included, the Claude turn excluded)", len(got))
	}
	if len(got[0].Blocking) != 1 {
		t.Errorf("round 1 blocking = %v, want only the blocking finding", got[0].Blocking)
	}
	if len(got[1].Blocking) != 0 {
		t.Errorf("round 2 blocking = %v, want none", got[1].Blocking)
	}
	if len(got[2].Blocking) != 1 || got[2].Blocking[0] != "first" {
		t.Errorf("round 3 blocking = %v, want the recurrence of \"first\"", got[2].Blocking)
	}
}

func TestBlockingSummaryCountsTallyRecurrenceAcrossRounds(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	task := mkTask(t, st, "t", "executing")
	other := mkTask(t, st, "other", "executing")

	// "first" is raised three times, "second" once, and a non-blocking finding
	// never counts — the prompt marks what held the loop up, not what a
	// reviewer mentioned in passing.
	for i, findings := range [][]Finding{
		{{Severity: "major", Summary: "first", Blocking: true},
			{Severity: "nit", Summary: "cosmetic", Blocking: false}},
		{{Severity: "major", Summary: "first", Blocking: true},
			{Severity: "major", Summary: "second", Blocking: true}},
		{{Severity: "major", Summary: "first", Blocking: true}},
	} {
		step, err := st.StartStep(ctx, Step{
			TaskID: task.ID, Phase: "exec", Iteration: i + 1, Agent: "codex",
		})
		if err != nil {
			t.Fatal(err)
		}
		step.Verdict = "changes_requested"
		if err := st.FinishStep(ctx, step, findings); err != nil {
			t.Fatal(err)
		}
	}
	// Another task's identical finding must not inflate this one's count, or
	// every task would look like it was oscillating on a common objection.
	step, err := st.StartStep(ctx, Step{TaskID: other.ID, Phase: "exec", Iteration: 1, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	step.Verdict = "changes_requested"
	if err := st.FinishStep(ctx, step, []Finding{
		{Severity: "major", Summary: "first", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.BlockingSummaryCounts(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got["first"] != 3 {
		t.Errorf(`counts["first"] = %d, want 3`, got["first"])
	}
	if got["second"] != 1 {
		t.Errorf(`counts["second"] = %d, want 1`, got["second"])
	}
	if _, ok := got["cosmetic"]; ok {
		t.Errorf("a non-blocking finding was counted: %v", got)
	}
}

func TestAllReviewRoundsSeparatesTasks(t *testing.T) {
	st := depStore(t)
	ctx := context.Background()
	a := mkTask(t, st, "a", "executing")
	b := mkTask(t, st, "b", "executing")

	for _, task := range []Task{a, b} {
		step, err := st.StartStep(ctx, Step{
			TaskID: task.ID, Phase: "exec", Iteration: 1, Agent: "codex",
		})
		if err != nil {
			t.Fatal(err)
		}
		step.Verdict = "changes_requested"
		if err := st.FinishStep(ctx, step, []Finding{
			{Severity: "major", Summary: task.Slug + " finding", Blocking: true},
		}); err != nil {
			t.Fatal(err)
		}
	}

	rounds, err := st.AllReviewRounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds[a.ID]) != 1 || rounds[a.ID][0].Blocking[0] != "a finding" {
		t.Errorf("task a rounds = %+v", rounds[a.ID])
	}
	if len(rounds[b.ID]) != 1 || rounds[b.ID][0].Blocking[0] != "b finding" {
		t.Errorf("task b rounds = %+v", rounds[b.ID])
	}
}
