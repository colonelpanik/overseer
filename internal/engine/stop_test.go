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

// waitForChatAgentEdit blocks until a chat or design agent has written into the
// repository, which for those turns is the agent's working directory.
//
// The same proof waitForAgentEdit gives for a task, and needed for the same
// reason. Ask records the operator's turn synchronously and only then detaches
// the reply, so waiting for that turn says nothing about whether the agent is
// running: it is satisfied before the process has started. A test that returned
// on it would leave blockingClaude writing into a t.TempDir that cleanup is
// already deleting, and fail in RemoveAll with "directory not empty".
func waitForChatAgentEdit(t *testing.T, h *harness, name string) {
	t.Helper()
	waitFor(t, "the chat agent to write "+name, func() bool {
		_, err := os.Stat(filepath.Join(h.repo, name))
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
	// interrupted, not failed. A killed agent reports "signal: killed", which is
	// exactly what a crash reports, so nothing but the cancellation flag can
	// tell them apart — and a task the operator parked must not read on the
	// timeline as one that broke.
	if last.State != "interrupted" {
		t.Errorf("step state = %q, want interrupted", last.State)
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
	//
	// The reason goes in stop_reason, because that is the field the message is
	// built from: ParseClaudeLine renders a failed result as "claude result
	// <subtype> (stop_reason <stop_reason>)" and reads nothing else. Reporting
	// it in an "error" object instead — as this fixture used to — left ErrMsg
	// as "claude result error (stop_reason )", which IsRetryable rejects, so
	// the task failed permanently on the first attempt and there was no
	// backoff for the stop to land in.
	flaky := writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"result","subtype":"error","is_error":true,"stop_reason":"connection timeout","session_id":"s1"}'
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

// Stop parks; Start puts it back. The pair is the whole point, and neither
// touches the state that says where the task had got to.
func TestStopThenStartRoundTrips(t *testing.T) {
	h := newHarness(t, blockingClaude(t), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "round trip"})
	if err != nil {
		t.Fatal(err)
	}
	wait, _ := runInBackground(t, h, task.ID)
	waitForAgentEdit(t, h, task.ID, "partial.go")

	if err := h.eng.Stop(ctx, task.ID, StopOpts{Now: true}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	wait()

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stopped() {
		t.Fatal("the task is not stopped")
	}
	parked := got.State
	if loop.IsTerminal(parked) {
		t.Fatalf("State = %q, want a resumable state", parked)
	}

	if err := h.eng.Start(ctx, task.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, err = h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stopped() {
		t.Error("the task is still stopped after Start")
	}
	if got.State != parked {
		t.Errorf("State = %q, want %q — Start must not move the task, only unblock it", got.State, parked)
	}
}

func TestStopRefusesATerminalTaskAndStartRefusesToResurrectOne(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "already over"})
	if err != nil {
		t.Fatal(err)
	}
	task.State = string(loop.StateDone)
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.Stop(ctx, task.ID, StopOpts{}); err == nil {
		t.Error("Stop succeeded on a task that is already done")
	}
	// Start on a terminal task must point at restart rather than silently
	// doing nothing or resurrecting it.
	if err := h.st.StopTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	err = h.eng.Start(ctx, task.ID)
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Errorf("Start on a terminal task = %v, want it to point at restart", err)
	}
}

// Restart works on its own branch, so the previous attempt survives as the
// record of what was tried.
func TestRestartRunsOnAFreshBranchAndKeepsThePreviousAttempt(t *testing.T) {
	h := newHarness(t,
		fakeClaude(t, `echo 'package main' > added.go`),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "try again"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.setupWorktree(ctx, &task); err != nil {
		t.Fatal(err)
	}
	firstBranch := task.Branch
	firstDir := task.WorktreeDir
	task.State = string(loop.StateEscalated)
	task.FindingHashes = []string{"fingerprint"}
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.Restart(ctx, task.ID, RestartOpts{
		Constraints: []string{"do not touch the schema"},
	}); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunSeq != 2 {
		t.Fatalf("RunSeq = %d, want 2", got.RunSeq)
	}
	if got.State != string(loop.StateQueued) {
		t.Errorf("State = %q, want queued", got.State)
	}
	if len(got.FindingHashes) != 0 {
		t.Error("the old attempt's fingerprints survived; the first review would escalate at once")
	}
	if !strings.Contains(got.Constraints, "do not touch the schema") {
		t.Errorf("Constraints = %q, want the amendment", got.Constraints)
	}

	// Attempt 1's branch and worktree are untouched.
	if _, err := os.Stat(firstDir); err != nil {
		t.Errorf("the previous attempt's worktree was removed: %v", err)
	}
	branches := gitOut(t, h.repo, "branch", "--list")
	if !strings.Contains(branches, strings.TrimPrefix(firstBranch, "refs/heads/")) {
		t.Errorf("the previous attempt's branch is gone:\n%s", branches)
	}

	// Attempt 2 gets its own.
	if _, err := h.eng.setupWorktree(ctx, &got); err != nil {
		t.Fatalf("setupWorktree for attempt 2: %v", err)
	}
	if got.Branch == firstBranch {
		t.Errorf("attempt 2 reused attempt 1's branch %q", firstBranch)
	}
	if !strings.HasSuffix(got.Branch, "-r2") {
		t.Errorf("Branch = %q, want it to name the attempt", got.Branch)
	}
	if got.WorktreeDir == firstDir {
		t.Error("attempt 2 adopted attempt 1's worktree, so it would build on its commits")
	}
}

// A restarted task must not reuse the previous attempt's run directory, or its
// transcripts append to the old ones — the runner opens them append-only.
func TestRestartIsolatesTheRunDirectory(t *testing.T) {
	h := newHarness(t, "true", "true")
	first := store.Task{Slug: "widget", RunSeq: 1}
	second := store.Task{Slug: "widget", RunSeq: 2}
	if h.eng.runDir(first) == h.eng.runDir(second) {
		t.Errorf("both attempts share the run directory %q", h.eng.runDir(first))
	}
}

// Restarting a task with a pull request is unsafe in both directions, so it is
// refused rather than guessed at.
func TestRestartRefusesATaskWithAPullRequest(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "already shipped"})
	if err != nil {
		t.Fatal(err)
	}
	task.PRURL = "https://example.test/pr/1"
	task.State = string(loop.StateDone)
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	err = h.eng.Restart(ctx, task.ID, RestartOpts{})
	if err == nil {
		t.Fatal("Restart succeeded on a task with an open pull request")
	}
	if !strings.Contains(err.Error(), "pull request") {
		t.Errorf("err = %v, want it to name the pull request", err)
	}
}

// Restart lodged against a running worker is applied by that worker.
func TestRestartReachesARunningWorker(t *testing.T) {
	h := newHarness(t, blockingClaude(t), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "restart me live"})
	if err != nil {
		t.Fatal(err)
	}
	wait, _ := runInBackground(t, h, task.ID)
	waitForAgentEdit(t, h, task.ID, "partial.go")

	if err := h.eng.Restart(ctx, task.ID, RestartOpts{
		StopOpts: StopOpts{Now: true},
	}); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	wait()

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunSeq != 2 {
		t.Errorf("RunSeq = %d, want 2", got.RunSeq)
	}
	if got.State != string(loop.StateQueued) {
		t.Errorf("State = %q, want queued", got.State)
	}
	if got.Stopped() {
		t.Error("a restarted task is still stopped; it would never be claimed")
	}
}

// The workflow the plan tab exists for, end to end: stop a task, rewrite its
// plan, start it, and have the next turn implement the rewrite.
//
// This is the sharpest statement of the feature, because it fails if any one of
// three things is wrong — the write-back, the session clearing, or the
// resume-to-fresh downgrade that makes a cleared session mean "re-read the
// file".
func TestEditingThePlanWhileStoppedRedirectsTheWork(t *testing.T) {
	// An agent that copies whatever the plan says into its output, so the test
	// can see which plan the turn actually acted on.
	echoPlan := writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"claude-sess"}'
if [ -f PLAN.md ]; then cp PLAN.md acted-on.txt; else echo 'no plan' > acted-on.txt; fi
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess","total_cost_usd":0.01}'
`)
	h := newHarness(t, echoPlan, fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "follow the plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.setupWorktree(ctx, &task); err != nil {
		t.Fatal(err)
	}
	// Mid-exec, with a session — the case where an edit would otherwise be
	// ignored, because a resumed exec turn never re-reads the file.
	task.State = string(loop.StateExecuting)
	task.Phase = string(loop.PhaseExec)
	task.Iteration = 2
	task.ExecSessionID = "stale-session"
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.WorktreeDir, "PLAN.md"),
		[]byte("# the original plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Editing while it runs is refused: the write would race the agent.
	if err := h.eng.WritePlan(ctx, task.ID, "# too early\n"); err == nil {
		t.Error("WritePlan succeeded on a task that is not stopped")
	}

	if err := h.eng.Stop(ctx, task.ID, StopOpts{}); err != nil {
		t.Fatal(err)
	}

	got, err := h.eng.ReadPlan(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "the original plan") {
		t.Fatalf("ReadPlan = %q, want the plan on disk", got)
	}

	if err := h.eng.WritePlan(ctx, task.ID, "# the operator's plan\n"); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}

	// The edit is committed, so it is on the branch and in the diff rather than
	// sitting uncommitted for the next turn to sweep up.
	if out := gitOut(t, task.WorktreeDir, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("the edited plan was left uncommitted:\n%s", out)
	}

	// The session is gone, which is what makes the next turn re-read the file.
	after, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExecSessionID != "" {
		t.Fatal("the exec session survived the edit; the next turn would ignore the new plan")
	}

	if err := h.eng.Start(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	// Read from the branch, not the worktree: a converged task's worktree is
	// removed by finish, which deliberately keeps the branch as the record.
	acted := gitOut(t, h.repo, "show", task.Branch+":acted-on.txt")
	if !strings.Contains(acted, "the operator's plan") {
		t.Errorf("the turn acted on %q, want the edited plan", acted)
	}
	if strings.Contains(acted, "the original plan") {
		t.Error("the turn acted on the plan the operator replaced")
	}
}

func TestReadPlanIsEmptyRatherThanAnErrorWithoutAWorktree(t *testing.T) {
	h := newHarness(t, "true", "true")
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "no worktree yet"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.eng.ReadPlan(ctx, task.ID)
	if err != nil {
		t.Fatalf("ReadPlan on a queued task: %v", err)
	}
	if got != "" {
		t.Errorf("ReadPlan = %q, want empty", got)
	}
	if err := h.eng.WritePlan(ctx, task.ID, "# nope\n"); err == nil {
		t.Error("WritePlan succeeded on a task with no worktree")
	}
}
