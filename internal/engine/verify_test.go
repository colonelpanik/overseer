package engine

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/agent"
	"overseer/internal/loop"
)

func TestNormalizeFailureOutputIsStableAcrossRuns(t *testing.T) {
	// Test output carries timings, temp paths and addresses. Fingerprinting
	// it raw would make every failure look new, and oscillation detection
	// would never fire — a stuck task would burn all ten iterations.
	a := `--- FAIL: TestThing (0.03s)
    thing_test.go:14: got 3, want 4
FAIL	example.com/pkg	0.041s
FAIL`
	b := `--- FAIL: TestThing (1.27s)
    thing_test.go:14: got 3, want 4
FAIL	example.com/pkg	1.882s
FAIL`
	na, nb := NormalizeFailureOutput(a), NormalizeFailureOutput(b)
	if len(na) == 0 {
		t.Fatal("normalisation produced nothing to fingerprint")
	}
	if strings.Join(na, "|") != strings.Join(nb, "|") {
		t.Errorf("timings changed the normalised form:\n%v\n%v", na, nb)
	}
}

func TestNormalizeFailureOutputSeparatesDifferentFailures(t *testing.T) {
	a := NormalizeFailureOutput("--- FAIL: TestAlpha (0.01s)\nFAIL")
	b := NormalizeFailureOutput("--- FAIL: TestBeta (0.01s)\nFAIL")
	if strings.Join(a, "|") == strings.Join(b, "|") {
		t.Error("different failing tests normalised to the same thing")
	}
}

func TestVerifyFindingsAreAlwaysCritical(t *testing.T) {
	findings := VerifyFindings("make test", 2, "--- FAIL: TestX (0.1s)\nFAIL")
	if len(findings) == 0 {
		t.Fatal("no findings produced for a failing verify")
	}
	for _, f := range findings {
		if f.Severity != "critical" {
			t.Errorf("severity = %q, want critical so it blocks at every threshold", f.Severity)
		}
		if !strings.Contains(f.Summary, "make test") {
			t.Errorf("summary %q does not name the command that failed", f.Summary)
		}
	}
}

func TestVerifyFindingsPutRawOutputInDetailNotSummary(t *testing.T) {
	// Summary is fingerprinted; Detail is not. Raw output belongs in Detail,
	// or the fingerprint changes every run and oscillation detection dies.
	findings := VerifyFindings("go build ./...", 1,
		"main.go:7:2: undefined: foo\nbuilt in 1.482s at /tmp/build-9182")
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	f := findings[0]

	if !strings.Contains(f.Detail, "undefined: foo") {
		t.Error("the failure output must reach the agent via Detail")
	}
	if strings.Contains(f.Summary, "/tmp/build-9182") {
		t.Error("a volatile temp path is in Summary, which is fingerprinted")
	}
	if strings.Contains(f.Summary, "1.482s") {
		t.Error("a volatile timing is in Summary, which is fingerprinted")
	}
	if !strings.Contains(f.Summary, "go build") {
		t.Error("Summary must name the command that failed")
	}
}

func TestVerifyFingerprintIsStableAcrossIdenticalFailures(t *testing.T) {
	// The end-to-end property the normalisation exists for. Two runs of the
	// same broken test differ only in timings and temp paths, and must
	// fingerprint identically or the task burns its whole iteration budget.
	first := agent.Verdict{Verdict: "changes_requested", Findings: VerifyFindings(
		"make test", 1,
		"--- FAIL: TestThing (0.03s)\n    thing_test.go:14: got 3, want 4\nFAIL\texample.com/pkg\t0.041s\n/tmp/go-build123/x")}
	second := agent.Verdict{Verdict: "changes_requested", Findings: VerifyFindings(
		"make test", 1,
		"--- FAIL: TestThing (2.71s)\n    thing_test.go:14: got 3, want 4\nFAIL\texample.com/pkg\t2.884s\n/tmp/go-build998/x")}

	if first.Fingerprint("any") != second.Fingerprint("any") {
		t.Errorf("the same failure fingerprinted differently:\n%s\n%s\nsummaries:\n%q\n%q",
			first.Fingerprint("any"), second.Fingerprint("any"),
			first.Findings[0].Summary, second.Findings[0].Summary)
	}

	// A genuinely different failure must still differ.
	other := agent.Verdict{Verdict: "changes_requested", Findings: VerifyFindings(
		"make test", 1, "--- FAIL: TestOther (0.03s)\nFAIL")}
	if first.Fingerprint("any") == other.Fingerprint("any") {
		t.Error("different failures fingerprinted the same")
	}
}

func TestReviseWithFindingsIncludesDetail(t *testing.T) {
	// Detail is excluded from the fingerprint but must still reach the agent.
	p := ReviseWithFindingsPrompt("the code", VerifyFindings(
		"make test", 1, "--- FAIL: TestX (0.1s)\n    x_test.go:3: boom"))
	if !strings.Contains(p, "boom") {
		t.Errorf("the prompt omits the failure detail:\n%s", p)
	}
}

func TestTailWriterBoundsMemory(t *testing.T) {
	w := &tailWriter{max: 100}
	// Far more than max, written in many small pieces, as a chatty command
	// would. CombinedOutput would have held all of it.
	for i := 0; i < 10000; i++ {
		if _, err := w.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	got := w.String()
	if len(got) != 100 {
		t.Errorf("retained %d bytes, want 100", len(got))
	}
	if !strings.HasSuffix(got, "0123456789") {
		t.Errorf("the tail is not the most recent output: %q", got)
	}
}

func TestTailWriterHandlesASingleOversizedWrite(t *testing.T) {
	w := &tailWriter{max: 10}
	if _, err := w.Write([]byte("abcdefghijklmnop")); err != nil {
		t.Fatal(err)
	}
	if got := w.String(); got != "ghijklmnop" {
		t.Errorf("String() = %q, want the last 10 bytes", got)
	}
}

func TestLimitedWriterStopsAndNotesTruncation(t *testing.T) {
	var sink strings.Builder
	w := &limitedWriter{w: &sink, remaining: 10}

	// Every write must report success: the command is not at fault.
	for i := 0; i < 5; i++ {
		n, err := w.Write([]byte("aaaaa"))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if n != 5 {
			t.Errorf("write %d reported %d bytes, want 5", i, n)
		}
	}
	out := sink.String()
	if !strings.Contains(out, "truncated") {
		t.Error("truncation was not noted in the transcript")
	}
	if strings.Count(out, "truncated") != 1 {
		t.Error("the truncation note repeated")
	}
	if len(out) > 200 {
		t.Errorf("wrote %d bytes past a 10-byte limit", len(out))
	}
}

func TestVerifyOutputIsBoundedForAChattyCommand(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	ctx := context.Background()
	task := h.submit(t, "chatty verify")
	wt, err := h.eng.WT.Create(ctx, h.repo, task.Slug)
	if err != nil {
		t.Fatal(err)
	}
	task.WorktreeDir = wt.Dir

	// ~2MB of output from a failing command.
	res := h.eng.execVerify(ctx, task,
		"i=0; while [ $i -lt 20000 ]; do echo 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; i=$((i+1)); done; exit 1",
		nil)
	if res.ExitCode == 0 {
		t.Fatal("expected a non-zero exit")
	}
	if len(res.Output) > maxVerifyOutput+1024 {
		t.Errorf("retained %d bytes of output; the tail must stay bounded", len(res.Output))
	}
}

func TestVerifyGatePassesAndTaskCompletes(t *testing.T) {
	h := newHarness(t, fakeClaude(t, `echo 'package main' > added.go`),
		fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Cfg.VerifyCommand = "true"
	ctx := context.Background()

	task := h.submit(t, "verify passes")
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

	// A verify step must be recorded, before the code review.
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	verifyAt, reviewAt := -1, -1
	for i, s := range steps {
		if s.Agent == "verify" && verifyAt < 0 {
			verifyAt = i
		}
		if s.Agent == "codex" && s.Phase == "exec" && reviewAt < 0 {
			reviewAt = i
		}
	}
	if verifyAt < 0 {
		t.Fatal("no verify step recorded")
	}
	if reviewAt < 0 || verifyAt > reviewAt {
		t.Errorf("verify (%d) must precede the code review (%d)", verifyAt, reviewAt)
	}
}

func TestFailingVerifyIsFedBackAndBlocksThePR(t *testing.T) {
	// A Claude that never fixes anything, and a verify that always fails.
	// The task must escalate, and must not open a PR.
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Cfg.VerifyCommand = "echo '--- FAIL: TestAlways (0.01s)'; exit 1"
	ctx := context.Background()

	task := h.submit(t, "verify always fails")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == string(loop.StateDone) {
		t.Fatal("a task whose tests never pass reached done")
	}
	if len(h.pr.Calls) != 0 {
		t.Error("a PR was opened for code that does not pass its own tests")
	}
	// The same failure every time is an oscillation, caught early.
	if got.State != string(loop.StateEscalated) {
		t.Errorf("State = %q, want escalated", got.State)
	}

	// The failure output must have been recorded for the operator.
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawFinding bool
	for _, s := range steps {
		if s.Agent != "verify" {
			continue
		}
		findings, err := h.st.ListFindings(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range findings {
			sawFinding = true
			if f.Severity != "critical" || !f.Blocking {
				t.Errorf("verify finding not recorded as blocking critical: %+v", f)
			}
		}
	}
	if !sawFinding {
		t.Error("no verify findings stored")
	}
}

func TestVerifyRecoversAfterTheAgentFixesIt(t *testing.T) {
	// Fails once, then passes — the loop should carry on to done. The
	// counter file is a relative path, landing in the process's working
	// directory (the task worktree, which is mounted read-write for verify)
	// rather than an arbitrary host temp dir: under the default sandbox in
	// this environment, bubblewrap replaces /tmp with an empty, per-invocation
	// tmpfs, so a coordination file dropped outside a directory the sandbox
	// spec actually mounts is invisible to the next invocation and never
	// persists across runs. Task 16 hit the identical trap (see
	// TestPauseStopsATaskThatIsAlreadyMidFlight's comment); the fix here is
	// the same one applied there.
	marker := "n"
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Cfg.VerifyCommand = "n=0; [ -f " + marker + " ] && n=$(cat " + marker + "); " +
		"n=$((n+1)); echo $n > " + marker + "; " +
		"if [ $n -le 1 ]; then echo '--- FAIL: TestOnce (0.01s)'; exit 1; fi; exit 0"
	ctx := context.Background()

	task := h.submit(t, "verify recovers")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q (err %q), want done once verify passes", got.State, got.ErrMsg)
	}
}

func TestNoVerifyCommandKeepsTheOldBehaviour(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Cfg.VerifyCommand = ""
	ctx := context.Background()

	task := h.submit(t, "no verify configured")
	if err := h.eng.RunTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(loop.StateDone) {
		t.Fatalf("State = %q, want done", got.State)
	}
	steps, err := h.st.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Agent == "verify" {
			t.Error("a verify step ran with no command configured")
		}
	}
}

func TestPerTaskVerifyOverridesTheDaemonDefault(t *testing.T) {
	h := newHarness(t, fakeClaude(t, ""), fakeCodex(t, `{"verdict":"approved","findings":[]}`))
	h.eng.Cfg.VerifyCommand = "true"
	ctx := context.Background()

	task, err := h.eng.Submit(ctx, BatchTask{
		Repo: h.repo, Goal: "custom verify", Verify: "exit 0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.VerifyCommand != "exit 0" {
		t.Errorf("VerifyCommand = %q, want the per-task value", task.VerifyCommand)
	}
}
