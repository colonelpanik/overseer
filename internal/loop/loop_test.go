package loop

import (
	"strings"
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
	if next.State != StateExecuting {
		t.Errorf("State = %q, want %q; otherwise the engine dispatches ActClaudeExec but the task stays stuck in plan_review", next.State, StateExecuting)
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
	if next.State != StateExecuting {
		t.Errorf("State = %q, want %q", next.State, StateExecuting)
	}
}

func TestStateExecutingStoresExecSessionIDNotPlanSessionIDAndRequestsCodeReview(t *testing.T) {
	// This is the StateExecuting arm of Next's own switch (the transition
	// taken after an exec/exec-resume action completes), distinct from the
	// converge() path that first enters StateExecuting from plan review.
	task := newTask()
	task.State, task.Phase, task.Iteration = StateExecuting, PhaseExec, 1

	act, next := Next(task, ok("exec-sess"))
	if act.Kind != ActCodexCodeReview {
		t.Errorf("Kind = %q, want %q", act.Kind, ActCodexCodeReview)
	}
	if next.ExecSessionID != "exec-sess" {
		t.Errorf("ExecSessionID = %q, want exec-sess", next.ExecSessionID)
	}
	if next.PlanSessionID != "" {
		t.Errorf("PlanSessionID = %q, want empty: the session belongs in ExecSessionID", next.PlanSessionID)
	}
	if next.State != StateCodeReview {
		t.Errorf("State = %q, want %q", next.State, StateCodeReview)
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

func TestOscillationIsCheckedBeforeTheCapWhenBothAreTrue(t *testing.T) {
	// The existing cap test uses iteration 3 against a cap of 10, and the
	// existing oscillation test uses iteration 3 with no cap pressure, so
	// neither ever exercises both conditions at once. Here iteration ==
	// MaxIterations AND the fingerprint has already been seen, so the
	// escalation reason must identify oscillation, not the cap -- pinning
	// the order loop.go checks them in.
	task := newTask()
	task.MaxIterations = 10
	task.State, task.Phase, task.Iteration = StatePlanReview, PhasePlan, 10
	task.PlanSessionID = "plan-sess"

	first := changesRequested("rename the helper")
	task.FindingHashes = []string{first.Verdict.Fingerprint("any")}

	act, next := Next(task, changesRequested("rename the helper"))
	if act.Kind != ActEscalate {
		t.Fatalf("Kind = %q, want %q", act.Kind, ActEscalate)
	}
	if next.State != StateEscalated {
		t.Errorf("State = %q, want %q", next.State, StateEscalated)
	}
	if !strings.Contains(act.Reason, "oscillat") {
		t.Errorf("Reason = %q, want it to identify oscillation rather than the iteration cap", act.Reason)
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
	// Built with spare capacity, the way a database scan-then-append would
	// produce it. A slice literal always has cap == len, which would mask a
	// missing slices.Clone in Next: the append it does would reallocate
	// either way, so the input would look untouched even without the
	// clone. Spare capacity is required to actually exercise the clone.
	task.FindingHashes = make([]string, 1, 4)
	task.FindingHashes[0] = "existing"

	_, _ = Next(task, changesRequested("something"))
	if task.Iteration != 1 {
		t.Errorf("input Iteration mutated to %d", task.Iteration)
	}
	if len(task.FindingHashes) != 1 {
		t.Errorf("input FindingHashes mutated to %v", task.FindingHashes)
	}
	// Re-slice to the full backing array: if Next appended into the
	// caller's array instead of a clone, the fingerprint it wrote would be
	// visible here at index 1, even though task.FindingHashes's own length
	// (checked above) still looks untouched.
	full := task.FindingHashes[:cap(task.FindingHashes)]
	if full[1] != "" {
		t.Errorf("Next wrote into the caller's backing array at index 1: %q", full[1])
	}
}
