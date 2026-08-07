package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/loop"
)

func TestAuthFailurePausesTheWholeRun(t *testing.T) {
	claude := writeScript(t, "claude", `echo 'not logged in' >&2
exit 1`)
	h := newHarness(t, claude, fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task := h.submit(t, "First task")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	if reason := h.eng.PauseReason(); reason == "" {
		t.Fatal("an auth failure must pause the run")
	} else if !strings.Contains(reason, "not logged in") {
		t.Errorf("PauseReason = %q, want the underlying message", reason)
	}

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateFailed) {
		t.Errorf("State = %q, want failed", got.State)
	}
}

func TestPausedRunDoesNotStartQueuedTasks(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	h.eng.Pause("no credentials")
	task := h.submit(t, "Should not start")

	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateQueued) {
		t.Errorf("State = %q, want the task left queued while paused", got.State)
	}
}

func TestPauseStopsATaskThatIsAlreadyMidFlight(t *testing.T) {
	// With MaxParallel above 1, one worker can hit an authentication failure
	// while another task is mid-step. Checking the pause only when RunTask
	// starts would let that second task keep dispatching doomed calls.
	//
	// This Claude pauses the run from inside its first invocation, standing in
	// for another worker doing so concurrently.
	marker := filepath.Join(t.TempDir(), "calls")
	claude := writeScript(t, "claude", `
n=0
[ -f `+marker+` ] && n=$(cat `+marker+`)
n=$((n+1)); echo $n > `+marker+`
echo '{"type":"system","subtype":"init","session_id":"claude-sess"}'
echo '# plan' > PLAN.md
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess","total_cost_usd":0,"usage":{"input_tokens":1,"output_tokens":1}}'
`)
	h := newHarness(t, claude, fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	// Pause as soon as the first step completes.
	h.eng.OnChange = func(int64) {
		if h.eng.PauseReason() == "" {
			if raw, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(raw)) != "" {
				h.eng.Pause("another worker hit an auth failure")
			}
		}
	}

	task := h.submit(t, "mid-flight pause")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == string(loop.StateDone) {
		t.Fatal("the task ran to completion despite the run being paused mid-flight")
	}
	if len(h.pr.Calls) != 0 {
		t.Error("a paused run opened a PR")
	}

	// The task must be left resumable, not failed.
	if got.State == string(loop.StateFailed) {
		t.Errorf("pausing failed the task (%q); it should stay resumable", got.ErrMsg)
	}

	// And it must finish once the pause is cleared.
	h.eng.OnChange = nil
	h.eng.Resume()
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask after Resume: %v", err)
	}
	got, err = h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done after Resume", got.State, got.ErrMsg)
	}
}

func TestMissingAgentBinaryFailsTheTaskInsteadOfRetryingForever(t *testing.T) {
	// A start failure must become a persisted failure. Left as a harness
	// error the task stays non-terminal, ClaimableTasks keeps returning it,
	// and the scheduler retries every poll while accumulating a running step
	// row each time.
	h := newHarness(t, filepath.Join(t.TempDir(), "no-such-claude"),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task := h.submit(t, "missing binary")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateFailed) {
		t.Fatalf("State = %q, want failed", got.State)
	}
	if got.ErrMsg == "" {
		t.Error("no reason recorded for the operator")
	}

	// The task must no longer be claimable, or the scheduler will spin.
	claimable, err := h.st.ClaimableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claimable {
		if c.ID == task.ID {
			t.Fatal("a failed task is still claimable; the scheduler would retry it forever")
		}
	}

	// No step may be left running.
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.State == "running" {
			t.Errorf("step %d left running", s.ID)
		}
	}
	if len(steps) > 2 {
		t.Errorf("recorded %d steps for a single failed start; retries are accumulating", len(steps))
	}
}

func TestResumeClearsThePause(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	h.eng.Pause("no credentials")
	h.eng.Resume()
	if h.eng.PauseReason() != "" {
		t.Fatal("Resume did not clear the pause")
	}

	task := h.submit(t, "Runs after resume")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done", got.State, got.ErrMsg)
	}
}

func TestRunReturnsPromptlyWhilePaused(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Pause("no credentials")
	h.submit(t, "Queued while paused")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := h.eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
