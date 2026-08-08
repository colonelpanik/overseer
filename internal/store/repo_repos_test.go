package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertRepoIsIdempotentOnPath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	first, err := s.UpsertRepo(ctx, Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	second, err := s.UpsertRepo(ctx, Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatalf("UpsertRepo again: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("second upsert made a new repo: %d then %d", first.ID, second.ID)
	}
	if first.Slug != "widget" {
		t.Errorf("Slug = %q, want widget", first.Slug)
	}

	all, err := s.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("ListRepos = %d repos, want 1", len(all))
	}
}

func TestUpsertRepoSuffixesCollidingSlug(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a, err := s.UpsertRepo(ctx, Repo{Path: "/a/widget"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.UpsertRepo(ctx, Repo{Path: "/b/widget"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Slug == b.Slug {
		t.Fatalf("both repos got slug %q — a slug has to identify one repo", a.Slug)
	}
	if b.Slug != "widget-2" {
		t.Errorf("second slug = %q, want widget-2", b.Slug)
	}
}

// A probe that comes back blank must not wipe settings the operator typed.
// This is the whole reason UpsertRepo merges rather than overwrites: it runs on
// every submit, and most of those calls know nothing but the path.
func TestUpsertRepoDoesNotOverwriteWithEmptyFields(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	r, err := s.UpsertRepo(ctx, Repo{Path: "/src/widget", Detected: "Go · go test ./...", OriginURL: "https://example.test/w.git"})
	if err != nil {
		t.Fatal(err)
	}
	r.VerifyCommand = "make check"
	r.BlockingSeverity = "high"
	r.CostCapUSD = 4
	if err := s.SaveRepo(ctx, r); err != nil {
		t.Fatal(err)
	}

	// The call every Submit makes: path only.
	if _, err := s.UpsertRepo(ctx, Repo{Path: "/src/widget"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.RepoByPath(ctx, "/src/widget")
	if err != nil {
		t.Fatal(err)
	}
	if got.VerifyCommand != "make check" {
		t.Errorf("VerifyCommand = %q, want it preserved", got.VerifyCommand)
	}
	if got.BlockingSeverity != "high" || got.CostCapUSD != 4 {
		t.Errorf("defaults lost: %+v", got)
	}
	if got.Detected != "Go · go test ./..." {
		t.Errorf("Detected = %q, want it preserved", got.Detected)
	}
	if got.OriginURL != "https://example.test/w.git" {
		t.Errorf("OriginURL = %q, want it preserved", got.OriginURL)
	}
}

func TestUpsertRepoRefreshesWhatItLearns(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.UpsertRepo(ctx, Repo{Path: "/src/widget", Detected: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertRepo(ctx, Repo{Path: "/src/widget", Detected: "Go · go test ./...", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RepoByPath(ctx, "/src/widget")
	if err != nil {
		t.Fatal(err)
	}
	if got.Detected != "Go · go test ./..." {
		t.Errorf("Detected = %q, want the newer probe", got.Detected)
	}
	if got.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", got.DefaultBranch)
	}
}

func TestUpsertRepoRejectsEmptyPath(t *testing.T) {
	if _, err := newTestStore(t).UpsertRepo(context.Background(), Repo{Path: "  "}); err == nil {
		t.Fatal("want an error for a repo with no path")
	}
}

func TestRepoLookupsMissReturnErrNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.GetRepo(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRepo err = %v, want ErrNotFound", err)
	}
	if _, err := s.RepoByPath(ctx, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RepoByPath err = %v, want ErrNotFound", err)
	}
	if _, err := s.RepoBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RepoBySlug err = %v, want ErrNotFound", err)
	}
}

func TestArchivedRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	r, err := s.UpsertRepo(ctx, Repo{Path: "/src/old"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Archived() {
		t.Fatal("a fresh repo is not archived")
	}
	r.ArchivedAt = time.Now().UTC()
	if err := s.SaveRepo(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRepo(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived() {
		t.Error("archived_at did not round-trip")
	}
}

func TestSlugForPath(t *testing.T) {
	for path, want := range map[string]string{
		"/home/kal/code/overseer":  "overseer",
		"/home/kal/code/overseer/": "overseer",
		"/home/kal/My Project":     "my-project",
		"/home/kal/_.-":            "repo",
		"/":                        "repo",
	} {
		if got := slugForPath(path); got != want {
			t.Errorf("slugForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// preRepoDB writes a database in the shape an older build left behind: no
// repos table, no repo_id, and rows that name their repository only by path.
func preRepoDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC().Format(rfc3339)
	stmts := []string{
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT, slug TEXT NOT NULL UNIQUE,
			repo_path TEXT NOT NULL, goal TEXT NOT NULL,
			constraints TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
			phase TEXT NOT NULL DEFAULT '', iteration INTEGER NOT NULL DEFAULT 0,
			max_iterations INTEGER NOT NULL DEFAULT 10,
			blocking_severity TEXT NOT NULL DEFAULT 'any',
			plan_session_id TEXT NOT NULL DEFAULT '',
			exec_session_id TEXT NOT NULL DEFAULT '',
			branch TEXT NOT NULL DEFAULT '', base_ref TEXT NOT NULL DEFAULT '',
			git_common_dir TEXT NOT NULL DEFAULT '',
			git_admin_dir TEXT NOT NULL DEFAULT '',
			worktree_dir TEXT NOT NULL DEFAULT '', pr_url TEXT NOT NULL DEFAULT '',
			err_msg TEXT NOT NULL DEFAULT '', verify_command TEXT NOT NULL DEFAULT '',
			finding_hashes TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE proposals (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo_path TEXT NOT NULL DEFAULT '', source_url TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL, focus TEXT NOT NULL DEFAULT '',
			notes TEXT NOT NULL DEFAULT '', max_tasks INTEGER NOT NULL DEFAULT 12,
			model TEXT NOT NULL DEFAULT '', detected TEXT NOT NULL DEFAULT '',
			cost_usd REAL NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			transcript_path TEXT NOT NULL DEFAULT '', err_msg TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO tasks (slug, repo_path, goal, state, created_at, updated_at)
			VALUES ('one', '/a/widget', 'g', 'done', '` + now + `', '` + now + `')`,
		`INSERT INTO tasks (slug, repo_path, goal, state, created_at, updated_at)
			VALUES ('two', '/a/widget', 'g', 'done', '` + now + `', '` + now + `')`,
		// A second repository whose directory has the same basename: the
		// migration has to give it its own row and its own slug.
		`INSERT INTO tasks (slug, repo_path, goal, state, created_at, updated_at)
			VALUES ('three', '/b/widget', 'g', 'queued', '` + now + `', '` + now + `')`,
		`INSERT INTO proposals (repo_path, state, created_at, updated_at)
			VALUES ('/a/widget', 'ready', '` + now + `', '` + now + `')`,
		`INSERT INTO proposals (repo_path, state, created_at, updated_at)
			VALUES ('/c/gadget', 'ready', '` + now + `', '` + now + `')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed pre-repo db: %v\n%s", err, stmt)
		}
	}
}

func TestMigrationBackfillsReposFromPaths(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	preRepoDB(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open pre-repo db: %v", err)
	}
	defer s.Close()

	repos, err := s.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 {
		t.Fatalf("backfill made %d repos, want 3 (/a/widget, /b/widget, /c/gadget)", len(repos))
	}

	bySlug := map[string]Repo{}
	for _, r := range repos {
		bySlug[r.Slug] = r
	}
	if bySlug["widget"].Path == bySlug["widget-2"].Path {
		t.Error("colliding basenames were not given distinct repos")
	}
	if _, ok := bySlug["gadget"]; !ok {
		t.Errorf("a proposal-only path did not produce a repo: %v", bySlug)
	}

	// Every row is linked, and linked to the repo matching its own path.
	tasks, err := s.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.RepoID == 0 {
			t.Fatalf("task %s left unlinked", task.Slug)
		}
		r, err := s.GetRepo(ctx, task.RepoID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Path != task.RepoPath {
			t.Errorf("task %s linked to repo %q, want %q", task.Slug, r.Path, task.RepoPath)
		}
	}

	var unlinkedProposals int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proposals WHERE repo_id = 0`).Scan(&unlinkedProposals); err != nil {
		t.Fatal(err)
	}
	if unlinkedProposals != 0 {
		t.Errorf("%d proposals left unlinked", unlinkedProposals)
	}
}

// Reopening a migrated database must not create a second set of repos, which is
// what a backfill without its guard would do on every daemon start.
func TestMigrationBackfillIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	preRepoDB(t, path)

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := first.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	after, err := second.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(after) != len(before) {
		t.Fatalf("reopening changed the repo list: %d then %d", len(before), len(after))
	}
	for i := range after {
		if after[i].ID != before[i].ID || after[i].Slug != before[i].Slug {
			t.Errorf("repo %d changed across reopen: %+v then %+v", i, before[i], after[i])
		}
	}
}
