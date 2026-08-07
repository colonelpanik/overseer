package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
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
