// Package loop is overseer's control flow, expressed as a pure function.
//
// Next takes a task's current state plus the outcome of the last action and
// returns the next action together with the task's new state. It performs no
// I/O and spawns no processes, so the entire plan/review/execute/review flow
// is testable in milliseconds.
package loop

import (
	"fmt"
	"slices"

	"overseer/internal/agent"
)

// State is where a task sits in its lifecycle.
type State string

const (
	StateQueued     State = "queued"
	StateWorktree   State = "worktree"
	StatePlanning   State = "planning"
	StatePlanReview State = "plan_review"
	StateExecuting  State = "executing"
	// StateVerifying runs the project's own check — the only objective
	// signal in a loop otherwise made of model judgement.
	StateVerifying  State = "verifying"
	StateCodeReview State = "code_review"
	StateFinishing  State = "finishing"
	StateDone       State = "done"
	StateEscalated  State = "escalated"
	// StateFailed means the machinery or the agent failed. It is deliberately
	// not where an operator's decision lands: a task somebody stopped or
	// abandoned reading as "failed" makes the board's one urgent signal mean
	// two different things.
	StateFailed State = "failed"
	// StateAbandoned is an operator saying this task is over. Terminal, and
	// distinct from failed for the reason above.
	StateAbandoned State = "abandoned"
)

// TerminalStates are the states a worker will never advance.
//
// This is the one definition. It used to be stated twice — here and as string
// literals in the store's claimable-tasks query — and once more as a
// hand-written predicate in the web layer, so adding a state meant finding
// three places and the compiler helped with none of them.
var TerminalStates = []State{StateDone, StateFailed, StateEscalated, StateAbandoned}

// TerminalStateNames is TerminalStates as plain strings, for the store and the
// web layer, which hold a task's state as a string rather than a loop.State.
func TerminalStateNames() []string {
	out := make([]string, len(TerminalStates))
	for i, s := range TerminalStates {
		out[i] = string(s)
	}
	return out
}

// IsTerminal reports whether a state is one a worker will never advance.
func IsTerminal(state string) bool { return isTerminal(State(state)) }

// Phase is which of the two loops a task is in. The iteration counter is
// per phase.
type Phase string

const (
	PhasePlan Phase = "plan"
	PhaseExec Phase = "exec"
)

// ActionKind is what the engine should do next.
type ActionKind string

const (
	ActSetupWorktree    ActionKind = "setup_worktree"
	ActClaudePlan       ActionKind = "claude_plan"
	ActClaudePlanResume ActionKind = "claude_plan_resume"
	ActCodexPlanReview  ActionKind = "codex_plan_review"
	ActClaudeExec       ActionKind = "claude_exec"
	ActClaudeExecResume ActionKind = "claude_exec_resume"
	ActVerify           ActionKind = "verify"
	ActCodexCodeReview  ActionKind = "codex_code_review"
	ActFinish           ActionKind = "finish"
	ActEscalate         ActionKind = "escalate"
	ActFail             ActionKind = "fail"
	// ActNone means there is nothing to do: the task is terminal.
	ActNone ActionKind = "none"
)

// Task is the subset of task state the state machine reads and writes. The
// engine maps it to and from store.Task.
type Task struct {
	State            State
	Phase            Phase
	Iteration        int
	MaxIterations    int
	BlockingSeverity string
	PlanSessionID    string
	ExecSessionID    string
	// Verify is true when a verify command is configured. Without one the
	// gate is skipped and convergence keeps its weaker, review-only meaning.
	Verify bool
	// FindingHashes holds one fingerprint per completed iteration of the
	// current phase. It is cleared on every phase change.
	FindingHashes []string
	ErrMsg        string
}

// Action is what the engine should do next.
type Action struct {
	Kind ActionKind
	// ResumeSessionID is set for the resume actions, so the review reaches
	// the same agent session that produced the work.
	ResumeSessionID string
	// Findings are the blocking findings to render into the resume prompt.
	Findings []agent.Finding
	// Reason explains an escalation or failure to the human.
	Reason string
}

// Outcome is the result of the previously dispatched action.
type Outcome struct {
	// Failed is set when the action could not be completed at all.
	Failed bool
	ErrMsg string
	// SessionID is the agent session the action established, if any.
	SessionID string
	// Verdict is set only by review actions. A nil Verdict in a review
	// state is a failure, never an approval.
	Verdict *agent.Verdict
}

// Next returns the action to dispatch and the task's resulting state.
// It never mutates t.
func Next(t Task, last *Outcome) (Action, Task) {
	next := t
	next.FindingHashes = slices.Clone(t.FindingHashes)

	if isTerminal(t.State) {
		return Action{Kind: ActNone}, next
	}
	if last != nil && last.Failed {
		next.State = StateFailed
		next.ErrMsg = last.ErrMsg
		return Action{Kind: ActFail, Reason: last.ErrMsg}, next
	}

	switch t.State {
	case StateQueued:
		next.State = StateWorktree
		return Action{Kind: ActSetupWorktree}, next

	case StateWorktree:
		next.State = StatePlanning
		next.Phase = PhasePlan
		next.Iteration = 1
		next.FindingHashes = nil
		return Action{Kind: ActClaudePlan}, next

	case StatePlanning:
		if last != nil && last.SessionID != "" {
			next.PlanSessionID = last.SessionID
		}
		next.State = StatePlanReview
		return Action{Kind: ActCodexPlanReview}, next

	case StateExecuting:
		if last != nil && last.SessionID != "" {
			next.ExecSessionID = last.SessionID
		}
		if t.Verify {
			next.State = StateVerifying
			return Action{Kind: ActVerify}, next
		}
		next.State = StateCodeReview
		return Action{Kind: ActCodexCodeReview}, next

	case StatePlanReview:
		return afterReview(t, next, last, PhasePlan)

	case StateVerifying:
		return afterVerify(t, next, last)

	case StateCodeReview:
		return afterReview(t, next, last, PhaseExec)

	case StateFinishing:
		next.State = StateDone
		return Action{Kind: ActNone}, next
	}

	next.State = StateFailed
	next.ErrMsg = fmt.Sprintf("unhandled state %q", t.State)
	return Action{Kind: ActFail, Reason: next.ErrMsg}, next
}

// afterReview decides what a completed review means for the task.
func afterReview(t, next Task, last *Outcome, phase Phase) (Action, Task) {
	// A review with no parseable verdict is a failure. Treating it as
	// approval would silently ship unreviewed code.
	if last == nil || last.Verdict == nil {
		next.State = StateFailed
		next.ErrMsg = "review produced no parseable verdict"
		return Action{Kind: ActFail, Reason: next.ErrMsg}, next
	}

	blocking := last.Verdict.Blocking(t.BlockingSeverity)
	if len(blocking) == 0 {
		return converge(next, phase)
	}

	fingerprint := last.Verdict.Fingerprint(t.BlockingSeverity)
	if slices.Contains(t.FindingHashes, fingerprint) {
		next.State = StateEscalated
		reason := fmt.Sprintf(
			"oscillating: the same %d blocking finding(s) recurred at iteration %d",
			len(blocking), t.Iteration)
		next.ErrMsg = reason
		return Action{Kind: ActEscalate, Reason: reason, Findings: blocking}, next
	}

	if t.Iteration >= t.MaxIterations {
		next.State = StateEscalated
		reason := fmt.Sprintf(
			"hit the %d-iteration cap in the %s loop with %d blocking finding(s) outstanding",
			t.MaxIterations, phase, len(blocking))
		next.ErrMsg = reason
		return Action{Kind: ActEscalate, Reason: reason, Findings: blocking}, next
	}

	next.FindingHashes = append(next.FindingHashes, fingerprint)
	next.Iteration = t.Iteration + 1

	if phase == PhasePlan {
		next.State = StatePlanning
		return Action{
			Kind:            ActClaudePlanResume,
			ResumeSessionID: t.PlanSessionID,
			Findings:        blocking,
		}, next
	}
	next.State = StateExecuting
	return Action{
		Kind:            ActClaudeExecResume,
		ResumeSessionID: t.ExecSessionID,
		Findings:        blocking,
	}, next
}

// afterVerify decides what a completed verify run means.
//
// A pass costs no iteration: the work is unchanged, and the review has not
// run yet. A failure spends one, because fixing it needs another agent turn.
func afterVerify(t, next Task, last *Outcome) (Action, Task) {
	if last == nil || last.Verdict == nil {
		// Nothing to read is never a pass, exactly as for a review.
		next.State = StateFailed
		next.ErrMsg = "verify produced no result"
		return Action{Kind: ActFail, Reason: next.ErrMsg}, next
	}

	// Verify findings are always critical, so they block at every threshold:
	// a failing build must not be waved through because the operator relaxed
	// the review threshold.
	blocking := last.Verdict.Blocking("any")
	if len(blocking) == 0 {
		next.State = StateCodeReview
		return Action{Kind: ActCodexCodeReview}, next
	}

	fingerprint := last.Verdict.Fingerprint("any")
	if slices.Contains(t.FindingHashes, fingerprint) {
		next.State = StateEscalated
		reason := fmt.Sprintf("verify keeps failing the same way at iteration %d", t.Iteration)
		next.ErrMsg = reason
		return Action{Kind: ActEscalate, Reason: reason, Findings: blocking}, next
	}
	if t.Iteration >= t.MaxIterations {
		next.State = StateEscalated
		reason := fmt.Sprintf("hit the %d-iteration cap with verify still failing", t.MaxIterations)
		next.ErrMsg = reason
		return Action{Kind: ActEscalate, Reason: reason, Findings: blocking}, next
	}

	next.FindingHashes = append(next.FindingHashes, fingerprint)
	next.Iteration = t.Iteration + 1
	next.State = StateExecuting
	return Action{
		Kind:            ActClaudeExecResume,
		ResumeSessionID: t.ExecSessionID,
		Findings:        blocking,
	}, next
}

// converge advances a task past a phase that Codex signed off on.
func converge(next Task, phase Phase) (Action, Task) {
	if phase == PhasePlan {
		next.Phase = PhaseExec
		next.Iteration = 1
		next.FindingHashes = nil
		next.State = StateExecuting
		// Deliberately no ResumeSessionID: execution starts a fresh
		// session seeded with PLAN.md, keeping the implementation context
		// clean.
		return Action{Kind: ActClaudeExec}, next
	}
	next.State = StateFinishing
	return Action{Kind: ActFinish}, next
}

// GrantMoreIterations un-parks an escalated task with a larger budget. The
// fingerprint history is cleared, otherwise the very next review would trip
// oscillation detection again.
func GrantMoreIterations(t Task, n int) Task {
	out := t
	out.MaxIterations = t.MaxIterations + n
	out.FindingHashes = nil
	out.ErrMsg = ""
	if t.Phase == PhaseExec {
		out.State = StateExecuting
	} else {
		out.State = StatePlanning
	}
	return out
}

func isTerminal(s State) bool {
	for _, t := range TerminalStates {
		if s == t {
			return true
		}
	}
	return false
}

// Pending returns the action a mid-flight task is waiting on, and whether
// there is one.
//
// A restarted daemon cannot know whether the step it was running completed,
// so it re-dispatches rather than assuming success. Agent steps are safe to
// repeat: a re-run plan turn overwrites PLAN.md, and a re-run review just
// costs another review. The caller must reload the findings for a resume
// action from the store, because they were only ever held in memory.
func Pending(t Task) (Action, bool) {
	switch t.State {
	case StateWorktree:
		return Action{Kind: ActSetupWorktree}, true

	case StatePlanning:
		if t.Iteration <= 1 {
			return Action{Kind: ActClaudePlan}, true
		}
		return Action{Kind: ActClaudePlanResume, ResumeSessionID: t.PlanSessionID}, true

	case StatePlanReview:
		return Action{Kind: ActCodexPlanReview}, true

	case StateExecuting:
		if t.Iteration <= 1 {
			return Action{Kind: ActClaudeExec}, true
		}
		return Action{Kind: ActClaudeExecResume, ResumeSessionID: t.ExecSessionID}, true

	case StateVerifying:
		return Action{Kind: ActVerify}, true

	case StateCodeReview:
		return Action{Kind: ActCodexCodeReview}, true

	case StateFinishing:
		return Action{Kind: ActFinish}, true
	}
	return Action{Kind: ActNone}, false
}
