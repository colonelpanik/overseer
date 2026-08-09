package store

import (
	"context"
	"database/sql"
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

func TestOpeningADatabaseWrittenBeforeChatsAddsTheProposalLink(t *testing.T) {
	// CREATE TABLE IF NOT EXISTS is a no-op against a table that already
	// exists, so a column added after a release reaches an existing database
	// only through addedColumns. Getting that wrong breaks every upgrade, and
	// only for people who already use overseer.
	dbPath := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	// The proposals table as it stood before the chat existed.
	_, err = old.Exec(`CREATE TABLE proposals (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL DEFAULT 0,
		repo_path TEXT NOT NULL DEFAULT '',
		source_url TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		focus TEXT NOT NULL DEFAULT '',
		notes TEXT NOT NULL DEFAULT '',
		max_tasks INTEGER NOT NULL DEFAULT 12,
		model TEXT NOT NULL DEFAULT '',
		detected TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT 'analyse',
		design TEXT NOT NULL DEFAULT '',
		architect_session TEXT NOT NULL DEFAULT '',
		cost_usd REAL NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		transcript_path TEXT NOT NULL DEFAULT '',
		err_msg TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL)`)
	if err != nil {
		t.Fatalf("create old proposals: %v", err)
	}
	if _, err := old.Exec(
		`INSERT INTO proposals (state, created_at, updated_at) VALUES ('ready', ?, ?)`,
		time.Now().UTC().Format(rfc3339), time.Now().UTC().Format(rfc3339)); err != nil {
		t.Fatalf("insert old proposal: %v", err)
	}
	old.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open an older database: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	// The pre-existing row still reads, with the new column at its default.
	p, err := s.GetProposal(ctx, 1)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if p.ChatID != 0 {
		t.Errorf("ChatID = %d, want 0", p.ChatID)
	}
	// And the chat tables, which are new rather than altered, are there.
	repo, err := s.UpsertRepo(ctx, Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if _, err := s.CreateChat(ctx, repo.ID); err != nil {
		t.Errorf("CreateChat on an upgraded database: %v", err)
	}
}
