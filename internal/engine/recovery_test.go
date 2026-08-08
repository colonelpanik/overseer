package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/loop"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// TestRecoveryAfterVerifyFailureResumesWithTheFinding is the end-to-end
// regression test for Finding F3.
//
// Scenario: an exec turn ran, its verify command failed (recorded as a step
// with Agent "verify", exactly as runVerify does), and the daemon crashed
// before dispatching the resulting ActClaudeExecResume. A fresh engine is
// then built against the very same store — simulating `overseer serve`
// restarting — and asked to run the task. The resumed turn must carry the
// verify failure into the SAME Claude session (--resume plus the finding
// text), not silently start a brand new one.
//
// With the old `agent = 'codex'` predicate, LastBlockingFindings finds
// nothing for a step recorded under "verify", so the engine's own fallback
// ("nothing to feed back: fall through to a fresh turn") kicks in and the
// resumed call loses both the --resume flag and the finding text.
func TestRecoveryAfterVerifyFailureResumesWithTheFinding(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.StepTimeout = 30 * time.Second

	// The real claude binary is replaced with a script that records exactly
	// what it was invoked with — into the task's run directory, which is
	// mounted read-write into the sandbox and (unlike the worktree) is never
	// removed on success, so it survives long enough for this test to read it
	// back after RunTask returns.
	runDir := filepath.Join(cfg.RunsDir(), "verify-recovery")
	captured := filepath.Join(runDir, "captured-args.txt")
	claude := writeScript(t, "claude", fmt.Sprintf(`
mkdir -p %q
echo "$@" > %q
echo '{"type":"system","subtype":"init","session_id":"claude-sess-2"}'
echo 'package main' > fixed.go
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"fixed it"}]},"session_id":"claude-sess-2"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess-2","total_cost_usd":0.01,"usage":{"input_tokens":5,"output_tokens":2}}'
`, runDir, captured))
	cfg.ClaudeBin = claude
	cfg.CodexBin = fakeCodex(t, `{"verdict":"approved","findings":[]}`)

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	pr := &worktree.FakeOpener{URL: "https://example.test/pr/1"}
	ctx := context.Background()
	repo := newRepo(t)

	eng1, err := New(cfg, st, worktree.NewManager(cfg.WorktreesDir()), pr)
	if err != nil {
		t.Fatal(err)
	}

	task, err := st.CreateTask(ctx, store.Task{
		Slug: "verify-recovery", RepoPath: repo, Goal: "fix the failing test",
		State: string(loop.StateExecuting), Phase: string(loop.PhaseExec),
		Iteration: 2, MaxIterations: 10, BlockingSeverity: "any",
		ExecSessionID: "exec-sess-1", VerifyCommand: "true",
	})
	if err != nil {
		t.Fatal(err)
	}

	wt, err := eng1.WT.Create(ctx, repo, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	task.WorktreeDir, task.Branch, task.BaseRef = wt.Dir, wt.Branch, wt.BaseRef
	if err := st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	// Record the verify failure exactly as runVerify would: Agent "verify",
	// a blocking critical finding, distinctive text we can look for later.
	const findingMarker = "TestAlwaysBrokenUntilFixed"
	step, err := st.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: "exec", Iteration: 1, Agent: "verify",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishStep(ctx, step, []store.Finding{
		{Severity: "critical", Summary: "`go test` failed (exit 1). Failures:\n" + findingMarker,
			Detail: "boom", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}

	// "Restart": a brand new Engine built against the same store and the same
	// worktree root, exactly as `overseer serve` would after a crash.
	eng2, err := New(cfg, st, worktree.NewManager(cfg.WorktreesDir()), pr)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng2.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := eng2.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask after recovery: %v", err)
	}

	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done once the resumed turn fixes it", got.State, got.ErrMsg)
	}

	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("claude was never invoked, or did not get far enough to record its args: %v", err)
	}
	args := string(raw)
	if !strings.Contains(args, "--resume") || !strings.Contains(args, "exec-sess-1") {
		t.Errorf("resumed turn did not carry --resume exec-sess-1 (fell through to a fresh session): %q", args)
	}
	if !strings.Contains(args, findingMarker) {
		t.Errorf("resumed prompt did not carry the verify failure: %q", args)
	}
}
