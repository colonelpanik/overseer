package loop

import (
	"testing"

	"overseer/internal/agent"
)

func newTask() Task {
	return Task{
		State:            StateQueued,
		MaxIterations:    10,
		BlockingSeverity: "any",
	}
}

func approved() *Outcome {
	v := agent.Verdict{Verdict: "approved"}
	return &Outcome{Verdict: &v}
}

func changesRequested(summaries ...string) *Outcome {
	v := agent.Verdict{Verdict: "changes_requested"}
	for _, s := range summaries {
		v.Findings = append(v.Findings, agent.Finding{Severity: agent.SevMajor, Summary: s})
	}
	return &Outcome{Verdict: &v}
}

func ok(sessionID string) *Outcome { return &Outcome{SessionID: sessionID} }

func TestQueuedTaskSetsUpWorktree(t *testing.T) {
	act, next := Next(newTask(), nil)
	if act.Kind != ActSetupWorktree {
		t.Errorf("Kind = %q, want %q", act.Kind, ActSetupWorktree)
	}
	if next.State != StateWorktree {
		t.Errorf("State = %q, want %q", next.State, StateWorktree)
	}
}

func TestWorktreeReadyStartsPlanningAtIterationOne(t *testing.T) {
	task := newTask()
	task.State = StateWorktree

	act, next := Next(task, ok(""))
	if act.Kind != ActClaudePlan {
		t.Errorf("Kind = %q, want %q", act.Kind, ActClaudePlan)
	}
	if next.State != StatePlanning {
		t.Errorf("State = %q, want %q", next.State, StatePlanning)
	}
	if next.Phase != PhasePlan {
		t.Errorf("Phase = %q, want %q", next.Phase, PhasePlan)
	}
	if next.Iteration != 1 {
		t.Errorf("Iteration = %d, want 1", next.Iteration)
	}
}

func TestPlanningStoresSessionIDAndRequestsReview(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanning, PhasePlan, 1

	act, next := Next(task, ok("plan-sess"))
	if act.Kind != ActCodexPlanReview {
		t.Errorf("Kind = %q, want %q", act.Kind, ActCodexPlanReview)
	}
	if next.PlanSessionID != "plan-sess" {
		t.Errorf("PlanSessionID = %q, want plan-sess", next.PlanSessionID)
	}
	if next.State != StatePlanReview {
		t.Errorf("State = %q, want %q", next.State, StatePlanReview)
	}
}

func TestPlanReviewWithFindingsResumesClaudeAndIncrementsIteration(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 1
	task.PlanSessionID = "plan-sess"

	act, next := Next(task, changesRequested("missing rollback step"))
	if act.Kind != ActClaudePlanResume {
		t.Errorf("Kind = %q, want %q", act.Kind, ActClaudePlanResume)
	}
	if act.ResumeSessionID != "plan-sess" {
		t.Errorf("ResumeSessionID = %q, want plan-sess", act.ResumeSessionID)
	}
	if len(act.Findings) != 1 || act.Findings[0].Summary != "missing rollback step" {
		t.Errorf("Findings = %+v, want the blocking finding", act.Findings)
	}
	if next.Iteration != 2 {
		t.Errorf("Iteration = %d, want 2", next.Iteration)
	}
	if next.State != StatePlanning {
		t.Errorf("State = %q, want %q", next.State, StatePlanning)
	}
	if len(next.FindingHashes) != 1 {
		t.Errorf("FindingHashes = %v, want one recorded fingerprint", next.FindingHashes)
	}
}

func TestPlanConvergedStartsExecuteAtIterationOne(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 4
	task.PlanSessionID = "plan-sess"
	task.FindingHashes = []string{"h1", "h2", "h3"}

	act, next := Next(task, approved())
	if act.Kind != ActClaudeExec {
		t.Errorf("Kind = %q, want %q", act.Kind, ActClaudeExec)
	}
	if next.Phase != PhaseExec {
		t.Errorf("Phase = %q, want %q", next.Phase, PhaseExec)
	}
	if next.Iteration != 1 {
		t.Errorf("Iteration = %d, want 1 (counter resets per phase)", next.Iteration)
	}
	if len(next.FindingHashes) != 0 {
		t.Errorf("FindingHashes = %v, want cleared on phase change", next.FindingHashes)
	}
	if next.PlanSessionID != "plan-sess" {
		t.Error("PlanSessionID must survive the phase change for the take-over button")
	}
}

func TestExecuteUsesFreshSessionNotThePlanSession(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 1
	task.PlanSessionID = "plan-sess"

	act, _ := Next(task, approved())
	if act.ResumeSessionID != "" {
		t.Errorf("ResumeSessionID = %q; execute must start a fresh session seeded with PLAN.md", act.ResumeSessionID)
	}
}

func TestCodeReviewWithFindingsResumesExecSession(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StateCodeReview, PhaseExec, 2
	task.ExecSessionID = "exec-sess"

	act, next := Next(task, changesRequested("error discarded"))
	if act.Kind != ActClaudeExecResume {
		t.Errorf("Kind = %q, want %q", act.Kind, ActClaudeExecResume)
	}
	if act.ResumeSessionID != "exec-sess" {
		t.Errorf("ResumeSessionID = %q, want exec-sess", act.ResumeSessionID)
	}
	if next.Iteration != 3 {
		t.Errorf("Iteration = %d, want 3", next.Iteration)
	}
}

func TestCodeConvergedFinishes(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StateCodeReview, PhaseExec, 3

	act, next := Next(task, approved())
	if act.Kind != ActFinish {
		t.Errorf("Kind = %q, want %q", act.Kind, ActFinish)
	}
	if next.State != StateFinishing {
		t.Errorf("State = %q, want %q", next.State, StateFinishing)
	}
}

func TestFinishingCompletesTask(t *testing.T) {
	task := newTask()
	task.State = StateFinishing

	act, next := Next(task, ok(""))
	if act.Kind != ActNone {
		t.Errorf("Kind = %q, want %q", act.Kind, ActNone)
	}
	if next.State != StateDone {
		t.Errorf("State = %q, want %q", next.State, StateDone)
	}
}

func TestIterationCapEscalates(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 10
	task.PlanSessionID = "plan-sess"

	act, next := Next(task, changesRequested("still not right"))
	if act.Kind != ActEscalate {
		t.Fatalf("Kind = %q, want %q", act.Kind, ActEscalate)
	}
	if next.State != StateEscalated {
		t.Errorf("State = %q, want %q", next.State, StateEscalated)
	}
	if act.Reason == "" {
		t.Error("escalation must carry a reason for the dashboard")
	}
}

func TestOscillationEscalatesBeforeTheCap(t *testing.T) {
	// The same blocking findings twice means the agent is not making
	// progress. This is the mitigation for a threshold of "any".
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 3
	task.PlanSessionID = "plan-sess"

	first := changesRequested("rename the helper")
	task.FindingHashes = []string{first.Verdict.Fingerprint("any")}

	act, next := Next(task, changesRequested("rename the helper"))
	if act.Kind != ActEscalate {
		t.Fatalf("Kind = %q, want %q on a repeated findings set", act.Kind, ActEscalate)
	}
	if next.State != StateEscalated {
		t.Errorf("State = %q, want %q", next.State, StateEscalated)
	}
}

func TestDifferentFindingsDoNotTripOscillation(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 3
	task.PlanSessionID = "plan-sess"
	task.FindingHashes = []string{changesRequested("first thing").Verdict.Fingerprint("any")}

	act, _ := Next(task, changesRequested("a different thing"))
	if act.Kind != ActClaudePlanResume {
		t.Errorf("Kind = %q, want the loop to continue", act.Kind)
	}
}

func TestNilVerdictInReviewStateFails(t *testing.T) {
	// A review step that produced no parseable verdict must never be read
	// as approval.
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 1

	act, next := Next(task, ok(""))
	if act.Kind != ActFail {
		t.Fatalf("Kind = %q, want %q when a review has no verdict", act.Kind, ActFail)
	}
	if next.State != StateFailed {
		t.Errorf("State = %q, want %q", next.State, StateFailed)
	}
}

func TestFailedOutcomeFailsTheTask(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanning, PhasePlan, 1

	act, next := Next(task, &Outcome{Failed: true, ErrMsg: "not logged in"})
	if act.Kind != ActFail {
		t.Errorf("Kind = %q, want %q", act.Kind, ActFail)
	}
	if next.State != StateFailed {
		t.Errorf("State = %q, want %q", next.State, StateFailed)
	}
	if next.ErrMsg != "not logged in" {
		t.Errorf("ErrMsg = %q, want it preserved for the dashboard", next.ErrMsg)
	}
}

func TestThresholdMajorIgnoresNits(t *testing.T) {
	task := newTask()
	task.BlockingSeverity = "major"
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 1

	v := agent.Verdict{Verdict: "changes_requested", Findings: []agent.Finding{
		{Severity: agent.SevNit, Summary: "rename x"},
	}}
	act, next := Next(task, &Outcome{Verdict: &v})
	if act.Kind != ActClaudeExec {
		t.Errorf("Kind = %q, want the phase to converge past a nit", act.Kind)
	}
	if next.Phase != PhaseExec {
		t.Errorf("Phase = %q, want %q", next.Phase, PhaseExec)
	}
}

func TestTerminalStatesProduceNoAction(t *testing.T) {
	for _, state := range []State{StateDone, StateFailed, StateEscalated} {
		task := newTask()
		task.State = state
		act, next := Next(task, nil)
		if act.Kind != ActNone {
			t.Errorf("state %q: Kind = %q, want %q", state, act.Kind, ActNone)
		}
		if next.State != state {
			t.Errorf("state %q was mutated to %q", state, next.State)
		}
	}
}

func TestGrantMoreIterationsResumesTheParkedPhase(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StateEscalated, PhasePlan, 10
	task.MaxIterations = 10

	resumed := GrantMoreIterations(task, 10)
	if resumed.MaxIterations != 20 {
		t.Errorf("MaxIterations = %d, want 20", resumed.MaxIterations)
	}
	if resumed.State != StatePlanning {
		t.Errorf("State = %q, want %q so the plan loop restarts", resumed.State, StatePlanning)
	}
	if len(resumed.FindingHashes) != 0 {
		t.Error("FindingHashes must clear, or the next review trips oscillation immediately")
	}

	task.Phase = PhaseExec
	if got := GrantMoreIterations(task, 10); got.State != StateExecuting {
		t.Errorf("exec phase resumed into %q, want %q", got.State, StateExecuting)
	}
}

func TestNextIsPureAndDoesNotMutateInput(t *testing.T) {
	task := newTask()
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 1
	task.FindingHashes = []string{"existing"}

	_, _ = Next(task, changesRequested("something"))
	if task.Iteration != 1 {
		t.Errorf("input Iteration mutated to %d", task.Iteration)
	}
	if len(task.FindingHashes) != 1 {
		t.Errorf("input FindingHashes mutated to %v", task.FindingHashes)
	}
}
