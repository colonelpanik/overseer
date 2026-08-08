package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/loop"
)

func TestEndToEndTwoTasksInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test spawns subprocesses")
	}

	// A Claude that implements the plan: writes PLAN.md, then a real Go file
	// with a test, so the resulting branch is something a human could merge.
	claude := writeScript(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"claude-sess"}'
if [ ! -f PLAN.md ]; then
  printf '# Plan\n\n1. Add answer() returning 42\n2. Add a test for it\n' > PLAN.md
else
  cat > answer.go <<'GO'
package main

// answer returns the answer.
func answer() int { return 42 }
GO
  cat > answer_test.go <<'GO'
package main

import "testing"

func TestAnswer(t *testing.T) {
	if answer() != 42 {
		t.Fatalf("answer() = %d, want 42", answer())
	}
}
GO
fi
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"worked"}]},"session_id":"claude-sess"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-sess","total_cost_usd":0.02,"usage":{"input_tokens":100,"output_tokens":50}}'
`)
	codex := fakeCodex(t, `{"verdict":"approved","findings":[]}`)

	h := newHarness(t, claude, codex)
	h.eng.Cfg.MaxParallel = 2

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Two tasks against two separate repos, so parallelism is real.
	repoA := h.repo
	repoB := newRepo(t)
	for _, r := range []struct{ repo, goal string }{
		{repoA, "Add an answer function"},
		{repoB, "Add another answer function"},
	} {
		if _, err := h.eng.Submit(ctx, BatchTask{Repo: r.repo, Goal: r.goal}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	// Run the daemon loop until both tasks are terminal.
	runCtx, stopRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- h.eng.Run(runCtx) }()

	deadline := time.Now().Add(75 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("tasks did not finish within the deadline")
		}
		tasks, err := h.st.ListTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		settled := 0
		for _, task := range tasks {
			switch loop.State(task.State) {
			case loop.StateDone, loop.StateFailed, loop.StateEscalated:
				settled++
			}
		}
		if settled == 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	stopRun()
	if err := <-runDone; err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Run: %v", err)
	}

	tasks, err := h.st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.State != string(loop.StateDone) {
			t.Errorf("task %d (%s) state = %q, err %q; want done",
				task.ID, task.Slug, task.State, task.ErrMsg)
		}
		if task.PRURL == "" {
			t.Errorf("task %d has no PR URL", task.ID)
		}
	}

	if len(h.pr.Calls) != 2 {
		t.Fatalf("opened %d PRs, want 2", len(h.pr.Calls))
	}
	for _, call := range h.pr.Calls {
		if !strings.Contains(call.Body, "## Plan") {
			t.Error("PR body does not include the plan")
		}
		if call.BaseBranch != "main" {
			t.Errorf("PR base = %q, want main", call.BaseBranch)
		}
	}

	// The code the agent "wrote" must actually be on the pushed branch, and
	// it must compile and pass its own test.
	for _, repo := range []string{repoA, repoB} {
		branches := gitOut(t, repo, "branch", "--list", "overseer/*")
		if !strings.Contains(branches, "overseer/") {
			t.Errorf("%s has no overseer branch:\n%s", repo, branches)
		}
		remote := gitOut(t, repo, "ls-remote", "--heads", "origin")
		if !strings.Contains(remote, "overseer/") {
			t.Errorf("%s: branch was not pushed:\n%s", repo, remote)
		}
	}

	// Check out the produced branch and run its test for real.
	checkout := t.TempDir()
	branch := strings.TrimSpace(strings.TrimPrefix(
		strings.SplitN(gitOut(t, repoA, "branch", "--list", "overseer/*"), "\n", 2)[0], "*"))
	run(t, ".", "git", "clone", "--branch", branch, repoA, checkout)
	if _, err := os.Stat(filepath.Join(checkout, "answer.go")); err != nil {
		t.Fatalf("answer.go missing from the branch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(checkout, "PLAN.md")); err != nil {
		t.Errorf("PLAN.md missing from the branch: %v", err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
