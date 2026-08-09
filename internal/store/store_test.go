package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetTask(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	created, err := s.CreateTask(ctx, Task{
		Slug:             "add-csv-export",
		RepoPath:         "/repo",
		Goal:             "Add CSV export",
		State:            "queued",
		MaxIterations:    10,
		BlockingSeverity: "any",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("CreateTask did not assign an ID")
	}
	if created.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}

	got, err := s.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Slug != "add-csv-export" || got.Goal != "Add CSV export" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestTimestampsRoundTripThroughGetTask(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	created, err := s.CreateTask(ctx, Task{Slug: "ts-round-trip", RepoPath: "/r", Goal: "g", State: "queued"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := s.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	if got.CreatedAt.IsZero() {
		t.Error("GetTask: CreatedAt is zero after round trip")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("GetTask: UpdatedAt is zero after round trip")
	}

	// The stored format (RFC3339Nano) round-trips exactly, but we compare
	// with a tolerance rather than requiring exact time.Time equality,
	// since exact equality on time.Time is fragile (monotonic reading,
	// location) even when the wall-clock instant is identical.
	if diff := got.CreatedAt.Sub(created.CreatedAt); diff < -time.Second || diff > time.Second {
		t.Errorf("GetTask.CreatedAt = %v, want within 1s of written value %v (diff %v)",
			got.CreatedAt, created.CreatedAt, diff)
	}
	if diff := got.UpdatedAt.Sub(created.UpdatedAt); diff < -time.Second || diff > time.Second {
		t.Errorf("GetTask.UpdatedAt = %v, want within 1s of written value %v (diff %v)",
			got.UpdatedAt, created.UpdatedAt, diff)
	}
}

func TestGetTaskCorruptTimestampReturnsError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	task, err := s.CreateTask(ctx, Task{Slug: "bad-timestamp", RepoPath: "/r", Goal: "g", State: "queued"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if _, err := s.DB().ExecContext(ctx,
		`UPDATE tasks SET created_at = ? WHERE id = ?`, "not-a-timestamp", task.ID,
	); err != nil {
		t.Fatalf("corrupt created_at: %v", err)
	}

	if _, err := s.GetTask(ctx, task.ID); err == nil {
		t.Fatal("GetTask: want error for corrupt created_at, got nil")
	}
}

func TestGetTaskMissingReturnsErrNotFound(t *testing.T) {
	if _, err := newTestStore(t).GetTask(context.Background(), 4242); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSaveTaskRoundTripsFindingHashes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	task, err := s.CreateTask(ctx, Task{Slug: "s", RepoPath: "/r", Goal: "g", State: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	task.State = "plan_review"
	task.Phase = "plan"
	task.Iteration = 3
	task.FindingHashes = []string{"aaa", "bbb"}
	task.PlanSessionID = "sess-1"
	if err := s.SaveTask(ctx, task); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FindingHashes) != 2 || got.FindingHashes[0] != "aaa" || got.FindingHashes[1] != "bbb" {
		t.Errorf("FindingHashes = %v, want [aaa bbb]", got.FindingHashes)
	}
	if got.Iteration != 3 || got.PlanSessionID != "sess-1" {
		t.Errorf("fields not saved: %+v", got)
	}
}

func TestEmptyFindingHashesIsNilNotOneEmptyString(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task, err := s.CreateTask(ctx, Task{Slug: "s", RepoPath: "/r", Goal: "g", State: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FindingHashes) != 0 {
		t.Errorf("FindingHashes = %v, want empty", got.FindingHashes)
	}
}

func TestSlugMustBeUnique(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateTask(ctx, Task{Slug: "dup", RepoPath: "/r", Goal: "g", State: "queued"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, Task{Slug: "dup", RepoPath: "/r", Goal: "g", State: "queued"}); err == nil {
		t.Fatal("expected uniqueness violation for duplicate slug")
	}
}

func TestClaimableTasksExcludesTerminalStates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for slug, state := range map[string]string{
		"a": "queued",
		"b": "planning",
		"c": "done",
		"d": "failed",
		"e": "escalated",
	} {
		if _, err := s.CreateTask(ctx, Task{Slug: slug, RepoPath: "/r", Goal: "g", State: state}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ClaimableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("claimable = %d tasks, want 2 (queued, planning)", len(got))
	}
}

func TestBaseRefRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	task, err := s.CreateTask(ctx, Task{Slug: "b", RepoPath: "/r", Goal: "g", State: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	task.BaseRef = "origin/main"
	if err := s.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseRef != "origin/main" {
		t.Errorf("BaseRef = %q, want origin/main", got.BaseRef)
	}
}

func TestSubjectRoundTripsThroughCreateAndGetTask(t *testing.T) {
	st := newTestStore(t)
	created, err := st.CreateTask(context.Background(), Task{
		Slug: "cache-the-rack-inventory-query", RepoPath: "/tmp/repo",
		Subject: "Cache the rack inventory query", Goal: wordyGoal,
		State: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "Cache the rack inventory query" {
		t.Errorf("Subject = %q, want the subject it was created with", got.Subject)
	}
	if got.Goal != wordyGoal {
		t.Errorf("Goal = %q, want the whole goal kept as the body", got.Goal)
	}
}

func TestSaveTaskLeavesTheSubjectAlone(t *testing.T) {
	// Subject follows goal, which SaveTask deliberately does not write: every
	// caller holds a row read before the operator touched anything, so writing
	// it here would revert an edit landing mid-step.
	st := newTestStore(t)
	created, err := st.CreateTask(context.Background(), Task{
		Slug: "s", RepoPath: "/tmp/repo", Subject: "The real subject",
		Goal: "The real goal.", State: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := created
	stale.Subject = "a stale copy"
	stale.Goal = "a stale goal"
	if err := st.SaveTask(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "The real subject" {
		t.Errorf("Subject = %q, want SaveTask to have left it alone", got.Subject)
	}
}

func TestRestartTaskWritesTheSubject(t *testing.T) {
	// RestartTask does write goal — "restart it, but this time..." is the
	// usual reason to restart — so it has to write the subject with it, or the
	// task would be listed under a title describing work it is no longer doing.
	st := newTestStore(t)
	created, err := st.CreateTask(context.Background(), Task{
		Slug: "s", RepoPath: "/tmp/repo", Subject: "The old subject",
		Goal: "The old goal.", State: "escalated",
	})
	if err != nil {
		t.Fatal(err)
	}
	next := created
	next.State = "queued"
	next.Subject = "The new subject"
	next.Goal = "The new goal."
	if err := st.RestartTask(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "The new subject" || got.Goal != "The new goal." {
		t.Errorf("subject/goal = %q / %q, want both replaced", got.Subject, got.Goal)
	}
}

func TestHeadlineFallsBackToTheGoal(t *testing.T) {
	// Every row written before the column existed has an empty subject, and
	// nothing backfills them: they derive one at read time instead.
	old := Task{Goal: wordyGoal}
	if got, want := old.Headline(), Subject(wordyGoal); got != want {
		t.Errorf("Headline = %q, want %q", got, want)
	}
	with := Task{Subject: "Cache the rack inventory query", Goal: wordyGoal}
	if got := with.Headline(); got != "Cache the rack inventory query" {
		t.Errorf("Headline = %q, want the stored subject", got)
	}
	if got := (ProposalTask{Goal: wordyGoal}).Headline(); got != Subject(wordyGoal) {
		t.Errorf("ProposalTask.Headline = %q, want a derived subject", got)
	}
}

func TestTheSubjectColumnIsAddedToAnOlderDatabase(t *testing.T) {
	// A database written by a build without the column. schema.sql's CREATE
	// TABLE IF NOT EXISTS is a no-op against it, so migrate() is the only
	// thing that can add the column — and if it does not, every read of the
	// task table fails with "no such column".
	dir := t.TempDir()
	path := filepath.Join(dir, "overseer.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"tasks", "proposal_tasks", "backlog"} {
		if _, err := first.DB().Exec("ALTER TABLE " + table + " DROP COLUMN subject"); err != nil {
			t.Fatalf("take the column back off %s: %v", table, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen a database missing the column: %v", err)
	}
	defer again.Close()
	if _, err := again.CreateTask(context.Background(), Task{
		Slug: "s", RepoPath: "/tmp/repo", Subject: "A subject", Goal: "A goal.",
		State: "queued",
	}); err != nil {
		t.Fatalf("write a task after the migration: %v", err)
	}

	// The backlog's column, through the writer that would fail without it.
	repo := testRepo(t, again, "/src/widget")
	if _, err := again.AddBacklogItem(context.Background(), BacklogItem{
		RepoID: repo.ID, Source: BacklogAnalysis,
		Subject: "A subject", Title: "A title.",
	}); err != nil {
		t.Fatalf("write a backlog item after the migration: %v", err)
	}
}
