package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/engine"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	// These tests exercise handlers and rendering, never an agent. Left at the
	// defaults they would find a real claude or codex on the developer's PATH
	// and start a paid conversation in the background — which then races the
	// test's own TempDir cleanup. Pointing at a binary that cannot exist makes
	// every background turn fail immediately and identically everywhere.
	cfg.ClaudeBin = filepath.Join(cfg.DataDir, "no-such-claude")
	cfg.CodexBin = filepath.Join(cfg.DataDir, "no-such-codex")

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	eng, err := engine.New(cfg, st,
		worktree.NewManager(filepath.Join(cfg.DataDir, "wt")),
		&worktree.FakeOpener{})
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, st, eng, filepath.Join(cfg.DataDir, "config.yaml")), st
}

// waitForArchitectTurns blocks until a conversation has recorded at least n
// turns.
//
// Starting a design replies in the background, and that goroutine writes into
// the proposal's run directory under the daemon's data dir — which is
// t.TempDir(). A test that returns while it is still running races its own
// cleanup, and RemoveAll fails on a directory the goroutine is still filling.
// Waiting for the turn it ends with is the synchronisation point.
func waitForArchitectTurns(t *testing.T, st *store.Store, proposalID int64, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		turns, err := st.ArchitectTurns(context.Background(), proposalID)
		if err != nil {
			t.Fatalf("ArchitectTurns: %v", err)
		}
		if len(turns) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("proposal %d never reached %d turns", proposalID, n)
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// store2Task returns an escalated task fixture.
func store2Task() store.Task {
	return store.Task{
		Slug: "parked", RepoPath: "/r", Goal: "g", State: "escalated",
		Phase: "plan", Iteration: 10, MaxIterations: 10,
		PlanSessionID: "sess-1", WorktreeDir: "/tmp/wt",
		ErrMsg: "oscillating", BlockingSeverity: "any",
	}
}

// initRepo makes a minimal git repo so Submit's validation passes.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "--initial-branch=main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

func TestToneSeparatesTheThreeThingsAnOperatorCaresAbout(t *testing.T) {
	// The board does not colour by state; it answers "is anything asking for
	// me", so every state has to land in one of three weights.
	cases := map[string]string{
		"done":        ToneMuted,
		"queued":      ToneMuted,
		"failed":      ToneAlert,
		"escalated":   ToneAlert,
		"planning":    ToneLive,
		"plan_review": ToneLive,
		"verifying":   ToneLive,
	}
	for state, want := range cases {
		if got := Tone(state); got != want {
			t.Errorf("Tone(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestProgressShowsPhaseAndIterationCap(t *testing.T) {
	got := Progress(store.Task{State: "plan_review", Phase: "plan", Iteration: 3, MaxIterations: 10})
	if got != "plan 3/10" {
		t.Errorf("Progress = %q, want \"plan 3/10\"", got)
	}
	if got := Progress(store.Task{State: "queued"}); got != "queued" {
		t.Errorf("Progress = %q, want \"queued\" before a phase exists", got)
	}
}

func TestBoardRendersEveryTaskWithItsCounter(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	if _, err := st.CreateTask(ctx, store.Task{
		Slug: "add-csv-export", RepoPath: "/repo", Goal: "Add CSV export to the rack view",
		State: "plan_review", Phase: "plan", Iteration: 3, MaxIterations: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.Task{
		Slug: "shared-backoff", RepoPath: "/repo2", Goal: "Shared backoff helper",
		State: "done", PRURL: "https://example.test/pr/3",
	}); err != nil {
		t.Fatal(err)
	}

	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Add CSV export to the rack view", "plan 3/10",
		"Shared backoff helper", "https://example.test/pr/3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing %q", want)
		}
	}
}

func TestBoardEscapesGoalText(t *testing.T) {
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "xss", RepoPath: "/r", Goal: `<script>alert(1)</script>`, State: "queued",
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("goal text was not escaped")
	}
}

func TestBoardWithNoTasksStillRenders(t *testing.T) {
	s, _ := newTestServer(t)
	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No tasks") {
		t.Error("empty board should say so")
	}
}

func TestTaskPageShowsAlternatingTimelineAndFindings(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	task, err := st.CreateTask(ctx, store.Task{
		Slug: "t", RepoPath: "/r", Goal: "Do the thing",
		State: "plan_review", Phase: "plan", Iteration: 2, MaxIterations: 10,
		PlanSessionID: "plan-sess", WorktreeDir: "/tmp/wt",
	})
	if err != nil {
		t.Fatal(err)
	}

	claudeStep, err := st.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishStep(ctx, claudeStep, nil); err != nil {
		t.Fatal(err)
	}

	codexStep, err := st.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	codexStep.Verdict = "changes_requested"
	if err := st.FinishStep(ctx, codexStep, []store.Finding{
		{Severity: "major", File: "main.go", Line: 12, Summary: "error discarded", Blocking: true},
		{Severity: "nit", Summary: "rename tmp", Blocking: false},
	}); err != nil {
		t.Fatal(err)
	}

	rec := get(t, s, "/task/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Do the thing", "plan 2/10", "claude", "codex",
		"changes_requested", "major", "main.go:12", "error discarded",
		"rename tmp",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("task page missing %q", want)
		}
	}
}

func TestTaskPageShowsTakeOverCommandWhenEscalated(t *testing.T) {
	s, st := newTestServer(t)
	if _, err := st.CreateTask(context.Background(), store.Task{
		Slug: "t", RepoPath: "/r", Goal: "g", State: "escalated",
		Phase: "plan", Iteration: 10, MaxIterations: 10,
		PlanSessionID: "sess-1", WorktreeDir: "/tmp/wt",
		ErrMsg: "oscillating: the same finding recurred",
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/task/1").Body.String()
	for _, want := range []string{"claude --resume sess-1", "/tmp/wt", "oscillating"} {
		if !strings.Contains(body, want) {
			t.Errorf("escalated task page missing %q", want)
		}
	}
}

func TestTaskPageNonBlockingFindingsAppearAsPunchList(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	task, err := st.CreateTask(ctx, store.Task{
		Slug: "t", RepoPath: "/r", Goal: "g", State: "done",
		Phase: "exec", Iteration: 2, MaxIterations: 10, BlockingSeverity: "major",
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := st.StartStep(ctx, store.Step{TaskID: task.ID, Phase: "exec", Iteration: 1, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishStep(ctx, step, []store.Finding{
		{Severity: "nit", Summary: "not worth blocking on", Blocking: false},
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/task/1").Body.String()
	if !strings.Contains(body, "not worth blocking on") {
		t.Error("non-blocking findings must still be visible as a punch list")
	}
}

func TestTaskPageMissingIDIs404(t *testing.T) {
	s, _ := newTestServer(t)
	if rec := get(t, s, "/task/9999"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if rec := get(t, s, "/task/notanumber"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	s, _ := newTestServer(t)
	if rec := get(t, s, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
