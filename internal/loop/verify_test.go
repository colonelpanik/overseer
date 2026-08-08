package loop

import (
	"testing"

	"overseer/internal/agent"
)

func verifyTask() Task {
	t := newTask()
	t.Verify = true
	return t
}

// verifyPassed is what the engine returns when the command exits zero. A
// verify outcome always carries a verdict: a nil one means the harness could
// not produce a result, which is a failure, not a pass.
func verifyPassed() *Outcome {
	v := agent.Verdict{Verdict: "approved"}
	return &Outcome{Verdict: &v}
}

func TestPlanConvergenceLeadsToExecuteThenVerify(t *testing.T) {
	task := verifyTask()
	task.State, task.Phase, task.Iteration = StateExecuting, PhaseExec, 1

	act, next := Next(task, ok("exec-sess"))
	if act.Kind != ActVerify {
		t.Errorf("Kind = %q, want %q; verify must gate before the review", act.Kind, ActVerify)
	}
	if next.State != StateVerifying {
		t.Errorf("State = %q, want %q", next.State, StateVerifying)
	}
	if next.ExecSessionID != "exec-sess" {
		t.Error("the exec session must be recorded before verifying")
	}
}

func TestWithoutAVerifyCommandTheGateIsSkipped(t *testing.T) {
	task := newTask() // Verify false
	task.State, task.Phase, task.Iteration = StateExecuting, PhaseExec, 1

	act, next := Next(task, ok("exec-sess"))
	if act.Kind != ActCodexCodeReview {
		t.Errorf("Kind = %q, want %q when no verify command is configured",
			act.Kind, ActCodexCodeReview)
	}
	if next.State != StateCodeReview {
		t.Errorf("State = %q, want %q", next.State, StateCodeReview)
	}
}

func TestVerifyPassGoesToCodeReview(t *testing.T) {
	task := verifyTask()
	task.State, task.Phase, task.Iteration = StateVerifying, PhaseExec, 1

	act, next := Next(task, verifyPassed())
	if act.Kind != ActCodexCodeReview {
		t.Errorf("Kind = %q, want %q", act.Kind, ActCodexCodeReview)
	}
	if next.State != StateCodeReview {
		t.Errorf("State = %q, want %q", next.State, StateCodeReview)
	}
	if next.Iteration != 1 {
		t.Errorf("Iteration = %d; a passing verify must not spend one", next.Iteration)
	}
}

func TestVerifyFailureResumesExecAndSpendsAnIteration(t *testing.T) {
	task := verifyTask()
	task.State, task.Phase, task.Iteration = StateVerifying, PhaseExec, 2
	task.ExecSessionID = "exec-sess"

	v := agent.Verdict{Verdict: "changes_requested", Findings: []agent.Finding{
		{Severity: agent.SevCritical, Summary: "make test failed: TestFoo"},
	}}
	act, next := Next(task, &Outcome{Verdict: &v})
	if act.Kind != ActClaudeExecResume {
		t.Fatalf("Kind = %q, want %q", act.Kind, ActClaudeExecResume)
	}
	if act.ResumeSessionID != "exec-sess" {
		t.Errorf("ResumeSessionID = %q, want exec-sess", act.ResumeSessionID)
	}
	if len(act.Findings) != 1 {
		t.Errorf("Findings = %+v, want the failure fed back", act.Findings)
	}
	if next.State != StateExecuting {
		t.Errorf("State = %q, want %q", next.State, StateExecuting)
	}
	if next.Iteration != 3 {
		t.Errorf("Iteration = %d, want 3", next.Iteration)
	}
}

func TestVerifyFailureBlocksAtEveryThreshold(t *testing.T) {
	// A failing build must never be waved through because the operator
	// relaxed the review threshold.
	task := verifyTask()
	task.BlockingSeverity = "critical"
	task.State, task.Phase, task.Iteration = StateVerifying, PhaseExec, 1
	task.ExecSessionID = "e"

	v := agent.Verdict{Verdict: "changes_requested", Findings: []agent.Finding{
		{Severity: agent.SevCritical, Summary: "build failed"},
	}}
	if act, _ := Next(task, &Outcome{Verdict: &v}); act.Kind != ActClaudeExecResume {
		t.Errorf("Kind = %q, want the loop to continue", act.Kind)
	}
}

func TestVerifyCapAndOscillationStillApply(t *testing.T) {
	v := agent.Verdict{Verdict: "changes_requested", Findings: []agent.Finding{
		{Severity: agent.SevCritical, Summary: "same failure"},
	}}

	capped := verifyTask()
	capped.State, capped.Phase, capped.Iteration = StateVerifying, PhaseExec, 10
	capped.ExecSessionID = "e"
	if act, next := Next(capped, &Outcome{Verdict: &v}); act.Kind != ActEscalate {
		t.Errorf("at the cap: Kind = %q, want %q", act.Kind, ActEscalate)
	} else if next.State != StateEscalated {
		t.Errorf("State = %q, want %q", next.State, StateEscalated)
	}

	// A build that fails the same way twice is not progressing.
	oscillating := verifyTask()
	oscillating.State, oscillating.Phase, oscillating.Iteration = StateVerifying, PhaseExec, 3
	oscillating.ExecSessionID = "e"
	oscillating.FindingHashes = []string{v.Fingerprint("any")}
	if act, _ := Next(oscillating, &Outcome{Verdict: &v}); act.Kind != ActEscalate {
		t.Errorf("on a repeat: Kind = %q, want %q", act.Kind, ActEscalate)
	}
}

func TestVerifyWithNoVerdictFails(t *testing.T) {
	// Same invariant as a review: an unreadable result is never a pass.
	task := verifyTask()
	task.State, task.Phase, task.Iteration = StateVerifying, PhaseExec, 1

	act, next := Next(task, &Outcome{Failed: true, ErrMsg: "verify command not found"})
	if act.Kind != ActFail {
		t.Errorf("Kind = %q, want %q", act.Kind, ActFail)
	}
	if next.State != StateFailed {
		t.Errorf("State = %q, want %q", next.State, StateFailed)
	}
}

func TestPendingCoversVerifying(t *testing.T) {
	task := Task{State: StateVerifying, Phase: PhaseExec, Iteration: 2, Verify: true}
	act, okPending := Pending(task)
	if !okPending {
		t.Fatal("Pending reported nothing pending while verifying")
	}
	if act.Kind != ActVerify {
		t.Errorf("Kind = %q, want %q", act.Kind, ActVerify)
	}
}
