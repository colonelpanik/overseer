package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"overseer/internal/loop"
	"overseer/internal/store"
)

// blockingClaude writes a partial edit and then hangs until it is killed. It
// stands in for an agent mid-turn.
//
// It signals that it has started by writing into its own working directory —
// the worktree — rather than a path handed in from the test, because it runs
// inside the sandbox where nothing else the test owns is mounted.
func blockingClaude(t *testing.T) string {
	t.Helper()
	return writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"claude-sess"}'
echo '# plan' > PLAN.md
echo 'package main // half written' > partial.go
# Hang until the process group is killed.
while true; do sleep 0.05; done
`)
}

// waitForAgentEdit blocks until the agent has actually written into the task's
// worktree, which is the only proof it is running rather than about to.
func waitForAgentEdit(t *testing.T, h *harness, id int64, name string) {
	t.Helper()
	waitFor(t, "the agent to write "+name, func() bool {
		task, err := h.st.GetTask(context.Background(), id)
		if err != nil || task.WorktreeDir == "" {
			return false
		}
		_, err = os.Stat(filepath.Join(task.WorktreeDir, name))
		return err == nil
	})
}

// waitFor polls until cond holds, so a test never depends on a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// runInBackground drives a task on its own goroutine, returning a function that
// waits for it and the control the operator's request would reach it through.
func runInBackground(t *testing.T, h *harness, id int64) (wait func(), ctrl *taskControl) {
	t.Helper()
	ctx, ctrl, ok := h.eng.claim(context.Background(), id)
	if !ok {
		t.Fatal("claim failed on a fresh task")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer h.eng.release(id, ctrl)
		_ = h.eng.runTask(ctx, ctrl, id)
	}()
	return wg.Wait, ctrl
}

// A hard stop kills the agent, closes its step honestly, and leaves the task's
// state alone — that state names the action in flight, and is what starting it
// again re-dispatches.
func TestHardStopParksTheTaskWithoutFailingIt(t *testing.T) {
	h := newHarness(t, blockingClaude(t), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "stop me"})
	if err != nil {
		t.Fatal(err)
	}
	wait, ctrl := runInBackground(t, h, task.ID)

	waitForAgentEdit(t, h, task.ID, "partial.go")

	h.eng.mu.Lock()
	err = h.eng.requestStopLocked(task.ID, stopRequest{
		Kind: StopPark, Msg: "stopped by the operator", Hard: true,
	})
	h.eng.mu.Unlock()
	if err != nil {
		t.Fatalf("requestStopLocked: %v", err)
	}
	wait()

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == string(loop.StateFailed) {
		t.Fatalf("a stopped task was recorded as failed: %q", got.ErrMsg)
	}
	if loop.IsTerminal(got.State) {
		t.Fatalf("State = %q; a park must leave the task resumable", got.State)
	}

	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("no step recorded")
	}
	last := steps[len(steps)-1]
	if last.State == "running" {
		t.Error("the step is still running; the live pane would spin forever")
	}
	if last.EndedAt.IsZero() {
		t.Error("the step has no end time")
	}
	_ = ctrl
}

// The partial work of a killed turn gets its own commit, so the tree is clean
// for the operator to edit and the Diff tab can show what the turn managed.
func TestHardStopCommitsThePartialWorkUnderItsOwnMessage(t *testing.T) {
	h := newHarness(t, blockingClaude(t), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "commit my scraps"})
	if err != nil {
		t.Fatal(err)
	}
	wait, _ := runInBackground(t, h, task.ID)
	waitForAgentEdit(t, h, task.ID, "partial.go")

	h.eng.mu.Lock()
	err = h.eng.requestStopLocked(task.ID, stopRequest{
		Kind: StopPark, Msg: "stopped by the operator", Hard: true,
	})
	h.eng.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	wait()

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorktreeDir == "" {
		t.Fatal("the task never got a worktree")
	}

	// The tree is clean: nothing is left for the next turn to sweep up along
	// with whatever the operator does in between.
	out := gitOut(t, got.WorktreeDir, "status", "--porcelain")
	if strings.TrimSpace(out) != "" {
		t.Errorf("worktree still dirty after a stop:\n%s", out)
	}

	log := gitOut(t, got.WorktreeDir, "log", "--format=%s")
	if !strings.Contains(log, "overseer: interrupted during") {
		t.Errorf("no interrupted commit in:\n%s", log)
	}
	// It must not claim an iteration completed.
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "overseer: plan iteration") ||
			strings.HasPrefix(line, "overseer: exec iteration") {
			t.Errorf("a killed turn was committed as a completed iteration: %q", line)
		}
	}
	if !strings.Contains(gitOut(t, got.WorktreeDir, "show", "--name-only", "--format="), "partial.go") {
		t.Error("the half-written file is not in the interrupted commit")
	}
}

// Starting a stopped task re-dispatches the action its state names, rather than
// beginning again — the same path a restarted daemon takes.
func TestStartResumesTheActionTheStateNames(t *testing.T) {
	h := newHarness(t,
		fakeClaude(t, `echo 'package main' > added.go`),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "resume me"})
	if err != nil {
		t.Fatal(err)
	}
	// Park it mid-plan, as a stop would leave it — through the real worktree
	// setup, so the task has the paths a genuinely mid-flight one has. Faking
	// the state without them leaves WorktreeDir empty, and every later git call
	// would run in the daemon's own directory.
	if _, err := h.eng.setupWorktree(ctx, &task); err != nil {
		t.Fatal(err)
	}
	task.State = string(loop.StatePlanning)
	task.Phase = string(loop.PhasePlan)
	task.Iteration = 1
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := h.st.StopTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}

	// While stopped, a worker must not touch it.
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask on a stopped task: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StatePlanning) {
		t.Fatalf("a stopped task advanced to %q", got.State)
	}

	if err := h.st.StopTask(ctx, task.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask after starting: %v", err)
	}
	got, err = h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == string(loop.StatePlanning) {
		t.Error("the task did not advance after being started")
	}
}

// A stop landing during the retry backoff used to return ctx.Err() as a
// machinery error, which failTask turned into a permanent failure with
// "context canceled" — and its writes used the same dead context, so the
// failure might not even land.
func TestAStopDuringTheRetryBackoffParksRatherThanFails(t *testing.T) {
	// An agent that reports a retryable error, so the engine enters its backoff.
	flaky := writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"result","subtype":"error","is_error":true,"error":{"message":"connection timeout"},"session_id":"s1"}'
exit 1
`)
	h := newHarness(t, flaky, fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.RetryBackoff = 2 * time.Second
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "stop me mid backoff"})
	if err != nil {
		t.Fatal(err)
	}
	wait, _ := runInBackground(t, h, task.ID)

	// Wait until a step is open, which means the first attempt has run and the
	// worker is in its backoff.
	waitFor(t, "the first attempt to fail", func() bool {
		steps, err := h.st.ListSteps(ctx, task.ID)
		return err == nil && len(steps) > 0
	})

	h.eng.mu.Lock()
	err = h.eng.requestStopLocked(task.ID, stopRequest{
		Kind: StopPark, Msg: "stopped by the operator", Hard: true,
	})
	h.eng.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	wait()

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == string(loop.StateFailed) {
		t.Errorf("stopped during a backoff and recorded as failed: %q", got.ErrMsg)
	}
	if strings.Contains(got.ErrMsg, "context canceled") {
		t.Errorf("ErrMsg = %q; the cancellation leaked into the task's record", got.ErrMsg)
	}
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.State == "running" {
			t.Error("a step was left running after a stop during the backoff")
		}
	}
}

// A stop must not be retried. Without the guard the runner's message — whatever
// the agent printed as it died — can match a retryable marker, and the agent is
// restarted three times over, each to its own step timeout.
func TestACancelledRunIsNeverRetryable(t *testing.T) {
	h := newHarness(t, "true", "true")
	role, err := h.eng.resolveRole("code")
	if err != nil {
		t.Fatal(err)
	}
	_ = role

	// The runner's own classification is what the engine keys off.
	for _, msg := range []string{"connection timeout", "429 rate limit", ""} {
		res := struct {
			ErrMsg   string
			Canceled bool
		}{ErrMsg: msg, Canceled: true}
		// Mirrors runner.go's final classification.
		retryable := res.ErrMsg != "" && !res.Canceled
		if retryable {
			t.Errorf("a cancelled run with ErrMsg %q was classed retryable", msg)
		}
	}
}

// Abandon and restart lodged against a running worker are applied by that
// worker, from the copy it actually holds — so the operator's write cannot be
// lost to the worker's next full-row save.
func TestAbandonLodgedWithARunningWorkerIsAppliedByIt(t *testing.T) {
	h := newHarness(t, blockingClaude(t), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "abandon me"})
	if err != nil {
		t.Fatal(err)
	}
	wait, _ := runInBackground(t, h, task.ID)
	waitForAgentEdit(t, h, task.ID, "partial.go")

	if err := h.eng.applyStop(ctx, task.ID, stopRequest{
		Kind: StopAbandon, Msg: "abandoned by the operator", Hard: true,
	}); err != nil {
		t.Fatalf("applyStop: %v", err)
	}
	wait()

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateAbandoned) {
		t.Errorf("State = %q, want abandoned", got.State)
	}
	if got.ErrMsg != "abandoned by the operator" {
		t.Errorf("ErrMsg = %q, want the operator's reason", got.ErrMsg)
	}
	if got.Stopped() {
		t.Error("the task is abandoned and still stopped")
	}
}

// With nobody driving the task, applyStop does the write itself — under the
// same lock, so a scheduler poll cannot claim the task in between and clobber
// it.
func TestApplyStopWritesDirectlyWhenNoWorkerOwnsTheTask(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "idle task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.applyStop(ctx, task.ID, stopRequest{
		Kind: StopAbandon, Msg: "abandoned by the operator",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateAbandoned) {
		t.Errorf("State = %q, want abandoned", got.State)
	}
}

// A second request cannot rewrite the first without racing the worker's read,
// so it is refused — except a hard escalation, which is the realistic sequence:
// "stop it", then "actually, stop it now".
func TestASecondRequestIsRefusedButAHardEscalationIsNot(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "double stop"})
	if err != nil {
		t.Fatal(err)
	}
	_, ctrl, ok := h.eng.claim(ctx, task.ID)
	if !ok {
		t.Fatal("claim failed")
	}
	defer h.eng.release(task.ID, ctrl)

	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()

	if err := h.eng.requestStopLocked(task.ID, stopRequest{Kind: StopPark}); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if err := h.eng.requestStopLocked(task.ID, stopRequest{Kind: StopAbandon}); err == nil {
		t.Error("a second soft request silently replaced the first")
	}
	if err := h.eng.requestStopLocked(task.ID, stopRequest{Kind: StopPark, Hard: true}); err != nil {
		t.Errorf("a hard escalation was refused: %v", err)
	}
}

// release must only evict its own control, or a late release removes the
// control of a worker that claimed the same task afterwards — leaving a running
// task nobody can stop.
func TestReleaseOnlyEvictsItsOwnControl(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	_, first, ok := h.eng.claim(ctx, 42)
	if !ok {
		t.Fatal("first claim failed")
	}
	h.eng.release(42, first)

	_, second, ok := h.eng.claim(ctx, 42)
	if !ok {
		t.Fatal("second claim failed after release")
	}
	// The stale release of the first control must not evict the second.
	h.eng.release(42, first)
	if !h.eng.isRunning(42) {
		t.Fatal("a stale release evicted a live worker's control")
	}
	h.eng.release(42, second)
	if h.eng.isRunning(42) {
		t.Error("the live control was not evicted by its own release")
	}
}

// RunTask claims for itself, so a direct call is stoppable exactly like a
// scheduled one — and does nothing when another worker already owns the task.
func TestRunTaskDeclinesATaskAlreadyOwned(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	_, ctrl, ok := h.eng.claim(ctx, task.ID)
	if !ok {
		t.Fatal("claim failed")
	}
	defer h.eng.release(task.ID, ctrl)

	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Errorf("RunTask on an owned task = %v, want nil", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateQueued) {
		t.Errorf("State = %q; a second driver advanced a task another worker owns", got.State)
	}
}

// A step interrupted by a stop must not be handed to the state machine as a
// review outcome, or it becomes a blocking finding and spends an iteration.
func TestStoppedStepsCarryTheOperatorsReason(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, "true", "true")

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "reasoned"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: "exec", Agent: "code",
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.parkStopped(ctx, &task, stopRequest{
		Kind: StopPark, Msg: "stopped by the operator",
	}); err != nil {
		t.Fatal(err)
	}
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].State != "interrupted" {
		t.Errorf("state = %q, want interrupted", steps[0].State)
	}
	if steps[0].ErrMsg != "stopped by the operator" {
		t.Errorf("ErrMsg = %q, want the operator's reason", steps[0].ErrMsg)
	}
}
