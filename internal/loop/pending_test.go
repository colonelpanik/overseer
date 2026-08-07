package loop

import "testing"

func TestPendingReturnsTheActionAMidFlightStateAwaits(t *testing.T) {
	cases := []struct {
		state     State
		phase     Phase
		iteration int
		want      ActionKind
	}{
		{StatePlanning, PhasePlan, 1, ActClaudePlan},
		{StatePlanning, PhasePlan, 4, ActClaudePlanResume},
		{StatePlanReview, PhasePlan, 2, ActCodexPlanReview},
		{StateExecuting, PhaseExec, 1, ActClaudeExec},
		{StateExecuting, PhaseExec, 3, ActClaudeExecResume},
		{StateCodeReview, PhaseExec, 2, ActCodexCodeReview},
		{StateWorktree, PhasePlan, 0, ActSetupWorktree},
		{StateFinishing, PhaseExec, 1, ActFinish},
	}
	for _, c := range cases {
		task := Task{State: c.state, Phase: c.phase, Iteration: c.iteration,
			MaxIterations: 10, BlockingSeverity: "any"}
		act, ok := Pending(task)
		if !ok {
			t.Errorf("state %q: Pending reported nothing pending", c.state)
			continue
		}
		if act.Kind != c.want {
			t.Errorf("state %q iteration %d: Kind = %q, want %q",
				c.state, c.iteration, act.Kind, c.want)
		}
	}
}

func TestPendingIsFalseForQueuedAndTerminalStates(t *testing.T) {
	for _, state := range []State{StateQueued, StateDone, StateFailed, StateEscalated} {
		if _, ok := Pending(Task{State: state}); ok {
			t.Errorf("state %q: Pending returned true; nothing is in flight", state)
		}
	}
}

func TestPendingResumeCarriesTheSessionID(t *testing.T) {
	task := Task{State: StatePlanning, Phase: PhasePlan, Iteration: 3, PlanSessionID: "p1"}
	act, _ := Pending(task)
	if act.ResumeSessionID != "p1" {
		t.Errorf("ResumeSessionID = %q, want p1", act.ResumeSessionID)
	}

	task = Task{State: StateExecuting, Phase: PhaseExec, Iteration: 2, ExecSessionID: "e1"}
	act, _ = Pending(task)
	if act.ResumeSessionID != "e1" {
		t.Errorf("ResumeSessionID = %q, want e1", act.ResumeSessionID)
	}
}
