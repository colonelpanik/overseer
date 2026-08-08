package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// waitForFile polls for path to exist, failing the test if timeout elapses
// first. Used instead of a fixed sleep to know for certain a slow in-flight
// worker has actually started before the test forces the store to fail.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// TestRunWaitsForInFlightWorkersOnAPersistentStoreError is the direct
// regression test for Finding F4: Run's ClaimableTasks error path used to
// return immediately, without wg.Wait(), unlike the ctx.Done() path a few
// lines below it. That let cmdServe see the returned error, close the store,
// and race a worker still mid-turn.
//
// A slow fake agent marks when it starts and when it finishes. Once it has
// genuinely started, the test closes the store's DB out from under Run,
// forcing every subsequent ClaimableTasks call to fail until the
// consecutive-failure cap gives up. Run must not return before the slow
// worker's "finished" marker exists — proving it waited for the in-flight
// RunTask goroutine rather than abandoning it.
func TestRunWaitsForInFlightWorkersOnAPersistentStoreError(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.StepTimeout = 30 * time.Second
	cfg.MaxParallel = 1

	slug := "slow-worker"
	runDir := filepath.Join(cfg.RunsDir(), slug)
	started := filepath.Join(runDir, "started")
	finished := filepath.Join(runDir, "finished")

	const sleepSeconds = 1
	claude := writeScript(t, "claude", fmt.Sprintf(`
mkdir -p %q
touch %q
sleep %d
touch %q
echo '{"type":"system","subtype":"init","session_id":"claude-sess"}'
echo '# plan' > PLAN.md
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"session_id":"claude-sess"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess","total_cost_usd":0.01,"usage":{"input_tokens":5,"output_tokens":2}}'
`, runDir, started, sleepSeconds, finished))
	cfg.ClaudeBin = claude
	cfg.CodexBin = fakeCodex(t, `{"verdict":"approved","findings":[]}`)

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	pr := &worktree.FakeOpener{URL: "https://example.test/pr/1"}
	eng, err := New(cfg, st, worktree.NewManager(cfg.WorktreesDir()), pr)
	if err != nil {
		t.Fatal(err)
	}
	eng.PollInterval = 20 * time.Millisecond

	repo := newRepo(t)
	if _, err := eng.Submit(context.Background(), BatchTask{Repo: repo, Goal: "slow worker"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- eng.Run(ctx) }()

	waitForFile(t, started, 5*time.Second)

	// Force every subsequent ClaimableTasks call to fail, standing in for a
	// disk that has gone bad mid-run.
	if err := st.DB().Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	closedAt := time.Now()
	var runErr error
	select {
	case runErr = <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the store started failing permanently")
	}
	elapsed := time.Since(closedAt)

	if runErr == nil {
		t.Fatal("Run should report the persistent store failure, not exit silently")
	}
	if _, err := os.Stat(finished); err != nil {
		t.Errorf("Run returned before the in-flight worker finished (no %s): %v", finished, err)
	}
	// The slow agent sleeps for sleepSeconds; if Run returned without
	// wg.Wait(), it would do so within milliseconds of the DB closing,
	// well under that. Waiting for (most of) the sleep is the signature of
	// having actually blocked on the in-flight worker.
	if elapsed < (sleepSeconds*time.Second)*9/10 {
		t.Errorf("Run returned after only %s; it must wait for the in-flight worker (~%ds sleep) "+
			"before returning on a persistent store error", elapsed, sleepSeconds)
	}
}
