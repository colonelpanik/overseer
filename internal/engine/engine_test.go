package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/loop"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// fakeClaude writes PLAN.md (and optionally a code file) then emits a
// stream-json result.
func fakeClaude(t *testing.T, extra string) string {
	t.Helper()
	return writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"claude-sess"}'
echo '# plan' > PLAN.md
`+extra+`
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"session_id":"claude-sess"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess","total_cost_usd":0.01,"usage":{"input_tokens":5,"output_tokens":2}}'
`)
}

// fakeCodex emits a thread id, writes the verdict to the file named by
// --output-last-message, and completes the turn.
func fakeCodex(t *testing.T, verdictJSON string) string {
	t.Helper()
	return writeScript(t, "codex", `
last=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then last="$a"; fi
  prev="$a"
done
echo '{"type":"thread.started","thread_id":"codex-thread"}'
if [ -n "$last" ]; then printf '%s' '`+verdictJSON+`' > "$last"; fi
echo '{"type":"item.completed","item":{"type":"agent_message","text":"emitted"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":9,"output_tokens":3}}'
`)
}

func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// newRepo creates a git repo with a bare origin, mirroring the worktree
// package's fixture.
func newRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	work := filepath.Join(base, "work")
	for _, args := range [][]string{
		{"init", "--bare", "--initial-branch=main", bare},
		{"init", "--initial-branch=main", work},
	} {
		run(t, base, "git", args...)
	}
	run(t, work, "git", "config", "user.name", "test")
	run(t, work, "git", "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", ".")
	run(t, work, "git", "commit", "-m", "initial")
	run(t, work, "git", "remote", "add", "origin", bare)
	run(t, work, "git", "push", "-u", "origin", "main")
	return work
}

func run(t *testing.T, dir, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
}

type harness struct {
	eng  *Engine
	st   *store.Store
	pr   *worktree.FakeOpener
	repo string
}

func newHarness(t *testing.T, claudeBin, codexBin string) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ClaudeBin = claudeBin
	cfg.CodexBin = codexBin
	cfg.StepTimeout = 30 * time.Second
	cfg.MaxParallel = 2

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
	return &harness{eng: eng, st: st, pr: pr, repo: newRepo(t)}
}

func (h *harness) submit(t *testing.T, goal string) store.Task {
	t.Helper()
	task, err := h.st.CreateTask(context.Background(), store.Task{
		Slug: worktree.Slugify(goal), RepoPath: h.repo, Goal: goal,
		State: string(loop.StateQueued), MaxIterations: 10, BlockingSeverity: "any",
		// Mirrors Submit's fallback to the daemon default, so tests that set
		// h.eng.Cfg.VerifyCommand before calling submit exercise the gate.
		VerifyCommand: h.eng.Cfg.VerifyCommand,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRunTaskConvergesFirstTimeAndOpensDraftPR(t *testing.T) {
	h := newHarness(t,
		fakeClaude(t, `echo 'package main' > added.go`),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task := h.submit(t, "Add a thing")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done", got.State, got.ErrMsg)
	}
	if got.PRURL != "https://example.test/pr/1" {
		t.Errorf("PRURL = %q", got.PRURL)
	}
	if len(h.pr.Calls) != 1 {
		t.Fatalf("PR opened %d times, want 1", len(h.pr.Calls))
	}
	if h.pr.Calls[0].BaseBranch != "main" {
		t.Errorf("PR base = %q, want main", h.pr.Calls[0].BaseBranch)
	}

	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// plan, plan review, exec, code review
	if len(steps) != 4 {
		t.Fatalf("recorded %d steps, want 4: %+v", len(steps), steps)
	}
	// Steps record the ROLE, not the CLI: with roles free to pick either
	// agent, "claude" stopped meaning "the coder".
	wantAgents := []string{"code", "review", "code", "review"}
	for i, want := range wantAgents {
		if steps[i].Agent != want {
			t.Errorf("step %d agent = %q, want %q", i, steps[i].Agent, want)
		}
	}
	if steps[0].TranscriptPath == "" {
		t.Error("step 0 has no transcript path")
	}
	if _, err := os.Stat(steps[0].TranscriptPath); err != nil {
		t.Errorf("transcript not on disk: %v", err)
	}
}

func TestRunTaskLoopsUntilCodexStopsFindingThings(t *testing.T) {
	// A Codex that objects twice, then approves. The counter file makes the
	// script stateful across invocations. It lives next to the last-message
	// file rather than in an arbitrary host temp dir, because that is the one
	// path this script is guaranteed to have write access to under the
	// sandbox: it is passed in on argv, and the engine mounts its directory
	// (the task's run dir) writable for exactly this reason.
	codex := writeScript(t, "codex", `
last=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then last="$a"; fi
  prev="$a"
done
counter="$(dirname "$last")/counter"
n=0
[ -f "$counter" ] && n=$(cat "$counter")
n=$((n+1)); echo $n > "$counter"
echo '{"type":"thread.started","thread_id":"codex-thread"}'
if [ "$n" -le 2 ]; then
  printf '%s' '{"verdict":"changes_requested","findings":[{"severity":"major","summary":"finding number '"$n"'","file":null,"line":null}]}' > "$last"
else
  printf '%s' '{"verdict":"approved","findings":[]}' > "$last"
fi
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	h := newHarness(t, fakeClaude(t, ""), codex)
	ctx := context.Background()

	task := h.submit(t, "Loop a few times")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done", got.State, got.ErrMsg)
	}
	// Findings 1 and 2 are distinct, so the plan loop runs three reviews
	// before converging; the code loop then converges on its first.
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) < 6 {
		t.Errorf("recorded %d steps, want at least 6 for a 3-round plan loop", len(steps))
	}
}

func TestRunTaskEscalatesOnRepeatedFindings(t *testing.T) {
	// Always the same finding: oscillation detection must fire well before
	// the iteration cap.
	h := newHarness(t, fakeClaude(t, ""),
		fakeCodex(t, `{"verdict":"changes_requested","findings":[{"severity":"major","summary":"same every time","file":null,"line":null}]}`))
	ctx := context.Background()

	task := h.submit(t, "Never converges")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateEscalated) {
		t.Fatalf("State = %q, want escalated", got.State)
	}
	if !strings.Contains(got.ErrMsg, "oscillat") {
		t.Errorf("ErrMsg = %q, want it to explain the oscillation", got.ErrMsg)
	}
	if len(h.pr.Calls) != 0 {
		t.Error("an escalated task must not open a PR")
	}
}

func TestRunTaskFailsWhenCodexReturnsProse(t *testing.T) {
	// Unparseable output must never be read as approval.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `Looks good to me!`))
	ctx := context.Background()

	task := h.submit(t, "Prose verdict")
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
	if len(h.pr.Calls) != 0 {
		t.Error("a task with an unparseable verdict must not open a PR")
	}
}

func TestRunTaskFailsOnNonRetryableAgentError(t *testing.T) {
	claude := writeScript(t, "claude", `echo 'not logged in' >&2
exit 1`)
	h := newHarness(t, claude, fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task := h.submit(t, "Auth failure")
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
	if !strings.Contains(got.ErrMsg, "not logged in") {
		t.Errorf("ErrMsg = %q, want the agent's stderr", got.ErrMsg)
	}
}

func TestRunTaskRetriesRetryableErrorWithoutSpendingAnIteration(t *testing.T) {
	// The counter file is a relative path, landing in the process's working
	// directory — the task worktree, which is writable for claude both with
	// and without the sandbox — rather than an arbitrary host temp dir a
	// sandboxed agent would have no access to.
	//
	// The failing first attempt prints its own "result" event with non-zero
	// usage before exiting 1: the failure is still detected from the exit
	// code and stderr text (retryable, via "429 too many requests"), but the
	// attempt genuinely spent tokens on the way to failing. If the engine
	// ever regresses to recording only the last attempt's usage, this
	// attempt's 40/20 tokens and $0.05 would silently vanish from the step
	// total instead of being added to the second attempt's 5/2 and $0.01.
	claude := writeScript(t, "claude", `
n=0
[ -f n ] && n=$(cat n)
n=$((n+1)); echo $n > n
if [ "$n" = "1" ]; then
  echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess","total_cost_usd":0.05,"usage":{"input_tokens":40,"output_tokens":20}}'
  echo '429 Too Many Requests' >&2
  exit 1
fi
echo '{"type":"system","subtype":"init","session_id":"claude-sess"}'
echo '# plan' > PLAN.md
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess","total_cost_usd":0.01,"usage":{"input_tokens":5,"output_tokens":2}}'
`)
	h := newHarness(t, claude, fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.RetryBackoff = time.Millisecond
	ctx := context.Background()

	task := h.submit(t, "Transient failure")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done after a retry", got.State, got.ErrMsg)
	}

	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 || steps[0].Agent != "code" || steps[0].Phase != "plan" {
		t.Fatalf("steps[0] = %+v, want the retried plan step", steps)
	}
	plan := steps[0]
	const wantInput, wantOutput = 40 + 5, 20 + 2
	const wantCost = 0.05 + 0.01
	if plan.InputTokens != wantInput || plan.OutputTokens != wantOutput {
		t.Errorf("plan step tokens = (%d,%d), want (%d,%d) — the failing attempt's usage "+
			"must be summed in, not dropped in favour of the last attempt alone",
			plan.InputTokens, plan.OutputTokens, wantInput, wantOutput)
	}
	if diff := plan.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("plan step cost = %v, want %v summed across both attempts", plan.CostUSD, wantCost)
	}
}

func TestRunTaskCommitsEachClaudeTurn(t *testing.T) {
	h := newHarness(t,
		fakeClaude(t, `echo 'package main' > added.go`),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task := h.submit(t, "Commit check")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The branch must be ahead of main, otherwise the PR would be empty.
	cmd := exec.Command("git", "rev-list", "--count", "origin/main..HEAD")
	cmd.Dir = got.WorktreeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The worktree is removed on success; assert against the branch in
		// the source repo instead.
		cmd = exec.Command("git", "rev-list", "--count", "main.."+got.Branch)
		cmd.Dir = h.repo
		out, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git rev-list: %v\n%s", err, out)
		}
	}
	if strings.TrimSpace(string(out)) == "0" {
		t.Error("branch has no commits; the engine did not commit the agent's work")
	}
}

func TestRecoverReDispatchesInterruptedTask(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	// Simulate a crash mid-plan: the task is in planning with a running step.
	task := h.submit(t, "Interrupted")
	task.State = string(loop.StatePlanning)
	task.Phase = string(loop.PhasePlan)
	task.Iteration = 1
	wt, err := h.eng.WT.Create(ctx, h.repo, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	// A real task cannot reach "planning" without ActSetupWorktree having
	// already persisted BaseRef alongside WorktreeDir and Branch.
	task.WorktreeDir, task.Branch, task.BaseRef = wt.Dir, wt.Branch, wt.BaseRef
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: "plan", Iteration: 1, Agent: "claude",
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].State != "interrupted" {
		t.Errorf("step 0 state = %q, want interrupted", steps[0].State)
	}

	// Recovery must leave the task runnable, and running it must finish it.
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask after Recover: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done", got.State, got.ErrMsg)
	}
}

func TestRecoveryAfterFinishDoesNotFailACompletedTask(t *testing.T) {
	// The window: finish() opened the PR and removed the worktree, then the
	// daemon exited before "done" was persisted. Recovery re-dispatches
	// ActFinish. It must reach done, not fail on the missing worktree, and
	// must not open a second PR.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task := h.submit(t, "Crashed at the finish line")
	wt, err := h.eng.WT.Create(ctx, h.repo, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	task.State = string(loop.StateFinishing)
	task.Phase = string(loop.PhaseExec)
	task.Iteration = 1
	task.WorktreeDir, task.Branch, task.BaseRef = wt.Dir, wt.Branch, wt.BaseRef
	task.PRURL = "https://example.test/pr/already-open"
	if err := h.st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	// Simulate the cleanup that already happened before the crash.
	if err := h.eng.WT.Remove(ctx, wt); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q); a task whose PR already exists must reach done",
			got.State, got.ErrMsg)
	}
	if got.PRURL != "https://example.test/pr/already-open" {
		t.Errorf("PRURL = %q, want the original PR", got.PRURL)
	}
	if len(h.pr.Calls) != 0 {
		t.Errorf("opened %d additional PRs, want 0", len(h.pr.Calls))
	}
}

func TestConcurrentTasksAttachFindingsToTheirOwnSteps(t *testing.T) {
	// Two tasks reviewed at once. Each Codex review returns a finding that
	// names its own repo, so a step carrying the other task's finding is
	// detectable. Run with -race to also catch the unsynchronised write.
	codex := writeScript(t, "codex", `
last=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then last="$a"; fi
  prev="$a"
done
echo '{"type":"thread.started","thread_id":"codex-thread"}'
# The prompt contains the goal, so echo part of it back as the summary.
printf '%s' '{"verdict":"changes_requested","findings":[{"severity":"major","summary":"finding for '"$(basename "$PWD")"'","file":null,"line":null}]}' > "$last"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	h := newHarness(t, fakeClaude(t, ""), codex)
	h.eng.Cfg.MaxParallel = 2
	ctx := context.Background()

	repoB := newRepo(t)
	a, err := h.eng.Submit(ctx, BatchTask{Repo: h.repo, Goal: "task alpha"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.eng.Submit(ctx, BatchTask{Repo: repoB, Goal: "task beta"})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, id := range []int64{a.ID, b.ID} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if err := h.eng.RunTask(ctx, id); err != nil {
				t.Errorf("RunTask(%d): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	// Every finding recorded against a task must name that task's worktree.
	for _, task := range []store.Task{a, b} {
		steps, err := h.st.ListSteps(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, step := range steps {
			if step.Agent != "review" {
				continue
			}
			findings, err := h.st.ListFindings(ctx, step.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range findings {
				found++
				if !strings.Contains(f.Summary, task.Slug) {
					t.Errorf("task %d (%s) step %d holds a finding from another task: %q",
						task.ID, task.Slug, step.ID, f.Summary)
				}
			}
			if step.Verdict == "" {
				t.Errorf("task %d step %d has no verdict recorded", task.ID, step.ID)
			}
		}
		if found == 0 {
			t.Errorf("task %d recorded no findings at all", task.ID)
		}
	}
}

func TestOnChangeFiresForEveryTransition(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	var n int
	h.eng.OnChange = func(int64) { n++ }

	task := h.submit(t, "Notify")
	if err := h.eng.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if n < 5 {
		t.Errorf("OnChange fired %d times, want at least one per transition", n)
	}
}

func TestFailTaskClosesOutAGenuineHarnessError(t *testing.T) {
	// No test elsewhere makes dispatch return a real Go error: Runner.Run
	// converts every process-level failure into Result.ErrMsg instead, so a
	// gutted failTask (e.g. reduced to a bare `return nil`) would still pass
	// the whole suite. This test forces the one kind of error dispatch can
	// still surface: a harness-level failure to even write the transcript.
	//
	// The first plan step always writes to "<rundir>/plan-1-code.jsonl" — the
	// transcript is named for the ROLE. Pre-creating a directory at that exact
	// path makes Runner.Run's os.OpenFile fail with "is a directory" — a
	// genuine error, not an agent failure — after the step has already been
	// recorded as "running" in the store. That is exactly the situation
	// failTask exists to clean up: without it, the step would stay "running"
	// forever and the task would stay in a non-terminal state, so the
	// scheduler would re-claim and retry it every poll indefinitely.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()

	task := h.submit(t, "Harness error")
	runDir := filepath.Join(h.eng.Cfg.RunsDir(), task.Slug)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(runDir, "plan-1-code.jsonl")
	if err := os.Mkdir(transcript, 0o755); err != nil {
		t.Fatal(err)
	}

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
	if !strings.Contains(got.ErrMsg, "transcript") {
		t.Errorf("ErrMsg = %q, want it to record the harness failure", got.ErrMsg)
	}

	claimable, err := h.st.ClaimableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range claimable {
		if ct.ID == task.ID {
			t.Errorf("task %d still claimable after a harness error; scheduler would retry it forever", task.ID)
		}
	}

	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("expected a step to have been started before the harness error hit")
	}
	for _, s := range steps {
		if s.State == "running" {
			t.Errorf("step %d left in state %q, want it closed out", s.ID, s.State)
		}
	}
}
