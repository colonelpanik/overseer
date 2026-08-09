// Package engine wires the pure loop state machine to the real world: it
// dispatches each action to an agent or to git, records the outcome, and
// feeds it back into the state machine.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"overseer/internal/agent"
	"overseer/internal/config"
	"overseer/internal/loop"
	"overseer/internal/sandbox"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// maxRetries bounds retries of transient agent failures. Retries do not
// consume a loop iteration.
const maxRetries = 3

// Engine advances tasks. One Engine serves every task; RunTask is safe to
// call concurrently for distinct task IDs.
type Engine struct {
	Cfg   config.Config
	Store *store.Store
	WT    *worktree.Manager
	PR    worktree.PROpener

	Claude *agent.Runner
	Codex  *agent.Runner

	// SchemaPath is the materialised verdict schema passed to Codex.
	SchemaPath string

	// Sandbox confines the agent subprocesses.
	Sandbox sandbox.Wrapper
	// SandboxNote describes the active mode, for logs and the dashboard.
	SandboxNote string

	// RetryBackoff is the base delay between retries; tests shorten it.
	RetryBackoff time.Duration

	// PollInterval is how often Run checks for claimable tasks; tests shorten
	// it. Zero (the default returned by New) means 2s.
	PollInterval time.Duration

	// OnChange is called after every persisted state transition, so the web
	// layer can push an SSE update. It must not block.
	OnChange func(taskID int64)

	// running maps a task ID to the control its worker is reachable through. A
	// non-nil entry means a worker goroutine owns the task, between claim and
	// release — so it is both the guard against two workers driving the same
	// task and the channel an operator's stop reaches that worker through.
	//
	// One map, not two: a separate registry with the same key and the same
	// lifetime could only ever disagree with this one, and the disagreement
	// would be a task that is running but unstoppable.
	mu      sync.Mutex
	running map[int64]*taskControl

	// pauseReason is non-empty when the whole run is halted, which happens
	// on an authentication failure: every task would fail identically.
	pauseReason string

	// providers and roles override Cfg's copies when the operator has changed
	// them from the dashboard. Guarded by mu because worker goroutines read
	// them on every agent invocation.
	providers map[string]config.Provider
	roles     map[string]config.Role
}

// New builds an Engine and materialises the verdict schema on disk.
func New(cfg config.Config, st *store.Store, wtm *worktree.Manager, pr worktree.PROpener) (*Engine, error) {
	schemaPath, err := agent.WriteVerdictSchema(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	wrapper, note, err := sandbox.Select(cfg.Sandbox, cfg.BwrapBin)
	if err != nil {
		return nil, err
	}
	// Created once at startup, not lazily per task: an optional mount is
	// silently skipped when its source is absent, and a skipped mount here
	// would mean the cache lives only on the sandbox's ephemeral tmpfs for
	// that one invocation — technically working, but defeating the point,
	// since nothing would persist across iterations or tasks.
	goBuild, goMod := goCacheDirs(cfg.DataDir)
	if err := sandbox.EnsureDirs(goBuild, goMod); err != nil {
		return nil, err
	}
	return &Engine{
		Cfg:          cfg,
		Store:        st,
		WT:           wtm,
		PR:           pr,
		Claude:       agent.NewClaudeRunner(cfg.ClaudeBin),
		Codex:        agent.NewCodexRunner(cfg.CodexBin),
		SchemaPath:   schemaPath,
		Sandbox:      wrapper,
		SandboxNote:  note,
		RetryBackoff: 5 * time.Second,
		PollInterval: 2 * time.Second,
		running:      map[int64]*taskControl{},
	}, nil
}

// logf reports something the operator should see but which is nobody's error
// to return: a best-effort cleanup that did not land, or a task failure already
// recorded in the database.
func (e *Engine) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "overseer: "+format+"\n", args...)
}

// maxConsecutiveClaimFailures bounds how many times in a row Run tolerates a
// ClaimableTasks error before giving up and returning it. A single failure is
// treated as transient — a momentary disk hiccup must not take the whole
// daemon down while tasks are mid-turn — but a store that never recovers is a
// genuine reason to stop rather than spin forever logging the same error.
const maxConsecutiveClaimFailures = 5

// Pause halts dispatch for every task until Resume is called.
func (e *Engine) Pause(reason string) {
	e.mu.Lock()
	e.pauseReason = reason
	e.mu.Unlock()
}

// Resume clears a pause.
func (e *Engine) Resume() {
	e.mu.Lock()
	e.pauseReason = ""
	e.mu.Unlock()
}

// PauseReason returns why the run is paused, or "" when it is not.
func (e *Engine) PauseReason() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pauseReason
}

// Recover prepares a restarted daemon: any step still marked running cannot
// have survived, so it is marked interrupted. The task itself is left in its
// current state, and RunTask re-dispatches the pending action.
func (e *Engine) Recover(ctx context.Context) error {
	n, err := e.Store.InterruptRunningSteps(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "overseer: marked %d interrupted step(s) from a previous run\n", n)
	}

	// An analysis runs in a goroutine, not a claimable task, so a restart
	// leaves its proposal saying "analysing" with nothing behind it. Without
	// this the wizard shows a spinner that never resolves and the operator has
	// no way to tell a long analysis from a dead one.
	m, err := e.Store.FailStrandedProposals(ctx,
		"the daemon restarted while this analysis was running")
	if err != nil {
		return err
	}
	if m > 0 {
		fmt.Fprintf(os.Stderr, "overseer: marked %d stranded analysis/analyses from a previous run\n", m)
	}

	// A global stop is a decision, not a condition, so it survives the restart.
	// Individual stops need nothing here: they are a column on the task, and
	// the claim query already reads it.
	if err := e.RestoreStopAll(ctx); err != nil {
		return err
	}
	if reason := e.PauseReason(); reason != "" {
		fmt.Fprintf(os.Stderr, "overseer: run is stopped (%s); nothing will dispatch until it is cleared\n", reason)
	}
	return nil
}

// Run polls for claimable tasks and drives them, up to MaxParallel at once.
// It returns when ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	sem := make(chan struct{}, max(1, e.Cfg.MaxParallel))
	var wg sync.WaitGroup
	interval := e.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveClaimFailures := 0

	for {
		if e.PauseReason() != "" {
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			case <-ticker.C:
			}
			continue
		}
		tasks, err := e.Store.ClaimableTasks(ctx)
		if err != nil {
			consecutiveClaimFailures++
			fmt.Fprintf(os.Stderr, "overseer: list claimable tasks (attempt %d/%d): %v\n",
				consecutiveClaimFailures, maxConsecutiveClaimFailures, err)
			if consecutiveClaimFailures >= maxConsecutiveClaimFailures {
				// A genuinely broken store, not a blip: give up, but only
				// after every task already mid-turn has actually finished.
				// Returning immediately here — as this used to — raced the
				// caller's deferred st.Close() against workers still calling
				// e.Store.SaveTask, which is the exact defect fix wave B
				// closed one call frame higher, in cmdServe itself.
				wg.Wait()
				return fmt.Errorf("list claimable tasks failed %d times in a row, giving up: %w",
					consecutiveClaimFailures, err)
			}
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			case <-ticker.C:
			}
			continue
		}
		consecutiveClaimFailures = 0
		for _, t := range tasks {
			taskCtx, ctrl, ok := e.claim(ctx, t.ID)
			if !ok {
				continue
			}
			wg.Add(1)
			// The goroutine closes over the task's own context, so the
			// error-suppression below covers a hard stop as well as a shutdown:
			// pressing Stop must not print "task 7: context canceled".
			go func(id int64, ctx context.Context, ctrl *taskControl) {
				defer wg.Done()
				defer e.release(id, ctrl)
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				if err := e.runTask(ctx, ctrl, id); err != nil && ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "overseer: task %d: %v\n", id, err)
				}
			}(t.ID, taskCtx, ctrl)
		}

		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-ticker.C:
		}
	}
}

// claim registers id as owned by the caller and returns the task's own
// context — a child of parent, cancelled by a hard stop — with the control an
// operator's request reaches this worker through.
func (e *Engine) claim(parent context.Context, id int64) (context.Context, *taskControl, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running[id] != nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	ctrl := &taskControl{stop: make(chan struct{}), cancel: cancel}
	e.running[id] = ctrl
	return ctx, ctrl, true
}

func (e *Engine) release(id int64, ctrl *taskControl) {
	e.mu.Lock()
	// Only evict our own entry. RunTask claims for itself when called directly,
	// so a late release must not remove the control of a worker that claimed
	// the same task afterwards.
	if e.running[id] == ctrl {
		delete(e.running, id)
	}
	e.mu.Unlock()
	ctrl.cancel()
}

// isRunning reports whether a worker goroutine currently owns taskID, i.e.
// holds an in-memory copy of it inside RunTask between claim and release.
func (e *Engine) isRunning(id int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id] != nil
}

// RunTask drives one task from its current state to a terminal state.
//
// It claims the task itself, so a direct caller is stoppable exactly like a
// scheduled one, and returns nil without doing anything when another worker
// already owns it.
func (e *Engine) RunTask(ctx context.Context, taskID int64) error {
	ctx, ctrl, ok := e.claim(ctx, taskID)
	if !ok {
		return nil
	}
	defer e.release(taskID, ctrl)
	return e.runTask(ctx, ctrl, taskID)
}

// runTask is the body, for callers that have already claimed the task.
func (e *Engine) runTask(ctx context.Context, ctrl *taskControl, taskID int64) error {
	// The stop is checked before the pause, here and at every other boundary.
	// The other way round, stopping a task while the run is globally paused
	// returns early and drops the request on the floor with no record of it.
	if req, ok := stopRequested(ctrl); ok {
		task, err := e.Store.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		return e.parkStopped(ctx, &task, req)
	}
	// Checked before the task is even loaded: a queued task must be left
	// alone while the run is paused, not advanced into its first state
	// transition only to be stopped at the dispatch guard below. The state
	// machine's own Next call has no notion of "paused" — it would already
	// have moved a queued task to "worktree" and persisted that before the
	// dispatch guard ever ran.
	if reason := e.PauseReason(); reason != "" {
		return nil
	}

	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	// A stop that landed between the scheduler's poll and this claim is
	// invisible to the claim query, which was answered before it was written.
	// This costs nothing: the row was just read anyway.
	if task.Stopped() {
		return nil
	}

	// After a restart the task may be parked mid-step. Re-dispatch what it
	// was waiting on so the first Next call has a real outcome.
	var last *loop.Outcome
	if pending, ok := loop.Pending(toLoop(task)); ok {
		action := pending
		if action.Kind == loop.ActClaudePlanResume || action.Kind == loop.ActClaudeExecResume {
			phase := task.Phase
			stored, err := e.Store.LastBlockingFindings(ctx, task.ID, phase)
			if err != nil {
				return err
			}
			action.Findings = toAgentFindings(stored)
			// Fall through to a fresh turn when there is nothing to resume
			// with. Two ways that happens: no findings to feed back, or no
			// session to feed them into.
			//
			// The second is how an edited plan takes effect. A resumed exec
			// turn is prompted with the findings alone and never re-reads
			// PLAN.md — it runs on the session's memory of it — so editing the
			// plan would change nothing. Saving an edit clears the session id,
			// and a fresh turn is seeded from the file, which is what the state
			// machine intends by starting execution without a resume in the
			// first place.
			if len(action.Findings) == 0 || action.ResumeSessionID == "" {
				if action.Kind == loop.ActClaudePlanResume {
					action.Kind = loop.ActClaudePlan
				} else {
					action.Kind = loop.ActClaudeExec
				}
				action.ResumeSessionID = ""
			}
		}
		// The same guards the main loop applies. Recovery is a dispatch like
		// any other: another worker may have paused the run since this task's
		// entry check, and a harness error here must be persisted or the task
		// stays claimable and the scheduler repeats this exact action every
		// poll.
		if req, ok := stopRequested(ctrl); ok {
			return e.parkStopped(ctx, &task, req)
		}
		if reason := e.PauseReason(); reason != "" {
			return nil
		}
		outcome, err := e.dispatch(ctx, &task, action)
		if done, err := e.afterDispatch(ctx, &task, ctrl, err); done {
			return err
		}
		last = outcome
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		action, next := loop.Next(toLoop(task), last)
		applyLoop(&task, next)
		if err := e.Store.SaveTask(ctx, task); err != nil {
			return err
		}
		e.notify(task.ID)

		if action.Kind == loop.ActNone || action.Kind == loop.ActFail || action.Kind == loop.ActEscalate {
			return nil
		}

		// Re-check the stop and the pause before every dispatch, not only on
		// entry. For the pause: another worker may have hit an authentication
		// failure while this task was mid-step, and with MaxParallel above 1
		// this task would otherwise keep dispatching doomed calls despite the
		// advertised global pause. The task keeps its current state and resumes
		// from its pending action once the operator clears it.
		if req, ok := stopRequested(ctrl); ok {
			return e.parkStopped(ctx, &task, req)
		}
		if reason := e.PauseReason(); reason != "" {
			return nil
		}

		outcome, err := e.dispatch(ctx, &task, action)
		if done, err := e.afterDispatch(ctx, &task, ctrl, err); done {
			return err
		}
		last = outcome
	}
}

// afterDispatch decides what a completed dispatch means, before its outcome is
// allowed anywhere near the state machine. It reports whether runTask should
// return, and with what.
//
// The order matters and is the whole point. A stop lodged while the action was
// in flight wins over whatever the action reported: a SIGKILLed agent is
// indistinguishable from a failed one — the runner reports "signal: killed", or
// whatever the agent printed just before it died — so feeding that outcome to
// loop.Next would mark the task permanently failed for having been stopped on
// purpose. The same applies to err: a store write with the cancelled context
// fails, dispatch reports that as a harness error, and failTask would turn it
// into the same wrong terminal state.
func (e *Engine) afterDispatch(ctx context.Context, task *store.Task, ctrl *taskControl, err error) (bool, error) {
	if req, ok := stopRequested(ctrl); ok {
		return true, e.parkStopped(ctx, task, req)
	}
	// No request, but the context is dead: the daemon is shutting down. The
	// only other thing that cancels this context is a hard stop, and that
	// always closes ctrl.stop first, so the case above has already caught it.
	// Leave the task exactly as it is — the next daemon's loop.Pending
	// re-dispatches the action its state names.
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if err != nil {
		// A genuine harness failure — an unwritable transcript, a database
		// error — would otherwise leave the task in a non-terminal state that
		// ClaimableTasks keeps returning, and the scheduler would retry it
		// every poll forever.
		return true, e.failTask(ctx, task, err)
	}
	return false, nil
}

// failTask records a harness failure and closes any step left running, so a
// task can never be re-claimed and retried indefinitely.
func (e *Engine) failTask(ctx context.Context, task *store.Task, cause error) error {
	msg := cause.Error()
	e.logf("task %d failed: %v", task.ID, cause)

	// Detached for the same reason FinishStep is: recording a failure with the
	// context that caused it is how a failure goes unrecorded.
	ctx = context.WithoutCancel(ctx)
	if _, err := e.Store.FailRunningSteps(ctx, task.ID, msg); err != nil {
		e.logf("task %d: close running steps: %v", task.ID, err)
	}
	task.State = string(loop.StateFailed)
	task.ErrMsg = msg
	if err := e.Store.SaveTask(ctx, *task); err != nil {
		return fmt.Errorf("record failure %q: %w", msg, err)
	}
	e.notify(task.ID)
	return nil
}

// dispatch performs one action and returns its outcome. A failing agent
// yields an Outcome with Failed set, not an error; dispatch's error return is
// for failures of the machinery itself.
func (e *Engine) dispatch(ctx context.Context, task *store.Task, action loop.Action) (*loop.Outcome, error) {
	switch action.Kind {
	case loop.ActSetupWorktree:
		return e.setupWorktree(ctx, task)

	case loop.ActClaudePlan:
		return e.runClaude(ctx, task, "plan", PlanPrompt(task.Goal, task.Constraints), "")

	case loop.ActClaudePlanResume:
		return e.runClaude(ctx, task, "plan",
			ReviseWithFindingsPrompt("PLAN.md", action.Findings), action.ResumeSessionID)

	case loop.ActClaudeExec:
		return e.runClaude(ctx, task, "exec", ExecPrompt(task.Goal), "")

	case loop.ActClaudeExecResume:
		return e.runClaude(ctx, task, "exec",
			ReviseWithFindingsPrompt("the code", action.Findings), action.ResumeSessionID)

	case loop.ActVerify:
		return e.runVerify(ctx, task)

	case loop.ActCodexPlanReview:
		return e.runCodex(ctx, task, "plan", PlanReviewPrompt(task.Goal))

	case loop.ActCodexCodeReview:
		wt := e.worktreeOf(*task)
		return e.runCodex(ctx, task, "exec", CodeReviewPrompt(task.Goal, wt.BaseRef))

	case loop.ActFinish:
		return e.finish(ctx, task)
	}
	return &loop.Outcome{Failed: true,
		ErrMsg: fmt.Sprintf("engine cannot dispatch action %q", action.Kind)}, nil
}

func (e *Engine) setupWorktree(ctx context.Context, task *store.Task) (*loop.Outcome, error) {
	wt, err := e.WT.Create(ctx, task.RepoPath, task.RunSlug())
	if err != nil {
		return &loop.Outcome{Failed: true, ErrMsg: err.Error()}, nil
	}
	task.WorktreeDir = wt.Dir
	task.Branch = wt.Branch
	task.BaseRef = wt.BaseRef
	task.GitCommonDir = wt.CommonDir
	task.GitAdminDir = wt.AdminDir
	if err := e.Store.SaveTask(ctx, *task); err != nil {
		return nil, err
	}
	e.notify(task.ID)
	return &loop.Outcome{}, nil
}

// runClaude runs one Claude turn, then commits whatever it produced. The
// engine commits rather than instructing the agent to, so the branch state
// is deterministic and a forgetful turn cannot lose work.
func (e *Engine) runClaude(ctx context.Context, task *store.Task, phase, prompt, resume string) (*loop.Outcome, error) {
	role, err := e.resolveRole(config.RoleCode)
	if err != nil {
		return &loop.Outcome{Failed: true, ErrMsg: err.Error()}, nil
	}
	res, _, err := e.runAgent(ctx, task, phase, role, role.args(prompt, resume, "", ""))
	if err != nil {
		return nil, err
	}
	if res.ErrMsg != "" {
		return &loop.Outcome{Failed: true, ErrMsg: res.ErrMsg}, nil
	}

	msg := fmt.Sprintf("overseer: %s iteration %d", phase, task.Iteration)
	if _, err := e.WT.Commit(ctx, e.worktreeOf(*task), msg); err != nil {
		return &loop.Outcome{Failed: true, ErrMsg: "commit: " + err.Error()}, nil
	}
	return &loop.Outcome{SessionID: res.SessionID}, nil
}

// runCodex runs one review and parses its verdict from the last-message file.
func (e *Engine) runCodex(ctx context.Context, task *store.Task, phase, prompt string) (*loop.Outcome, error) {
	role, err := e.resolveRole(config.RoleReview)
	if err != nil {
		return &loop.Outcome{Failed: true, ErrMsg: err.Error()}, nil
	}

	// Only `codex exec` takes --output-schema and --output-last-message. A
	// reviewer running through `claude` gets the same contract stated in its
	// prompt, and its verdict is read from the final message. ParseVerdict is
	// the guarantee in both cases.
	var schemaPath, lastPath string
	if role.structured() {
		schemaPath = e.SchemaPath
		lastPath = filepath.Join(e.runDir(*task),
			fmt.Sprintf("%s-%d-verdict.json", phase, task.Iteration))
	} else {
		prompt = withInlineSchema(prompt)
	}

	res, step, err := e.runAgent(ctx, task, phase, role, role.args(prompt, "", schemaPath, lastPath))
	if err != nil {
		return nil, err
	}
	if res.ErrMsg != "" {
		return &loop.Outcome{Failed: true, ErrMsg: res.ErrMsg}, nil
	}

	verdict, parseErr := agent.ParseVerdict(reviewOutput(lastPath, res.FinalText))
	if parseErr != nil {
		// One stricter re-ask before giving up. A second failure fails the
		// task: unparseable output is never approval.
		retryPrompt := prompt + "\n\nYour previous response could not be parsed: " +
			parseErr.Error() + "\nRespond with the JSON object required by the " +
			"output schema and nothing else."
		res, step, err = e.runAgent(ctx, task, phase, role,
			role.args(retryPrompt, "", schemaPath, lastPath))
		if err != nil {
			return nil, err
		}
		verdict, parseErr = agent.ParseVerdict(reviewOutput(lastPath, res.FinalText))
		if parseErr != nil {
			return &loop.Outcome{Failed: true,
				ErrMsg: role.Agent + " returned no parseable verdict: " + parseErr.Error()}, nil
		}
	}

	if err := e.recordFindings(ctx, task, step, verdict); err != nil {
		return nil, err
	}
	return &loop.Outcome{SessionID: res.SessionID, Verdict: &verdict}, nil
}

// runAgent records a step, runs the agent with retries for transient
// failures, and closes the step out. It returns the closed step so the
// caller can attach a verdict to it.
//
// The step is returned rather than stored on the Engine: with MaxParallel
// above 1, several tasks run runAgent concurrently, and a field on the
// shared Engine would let one task's review overwrite another's step
// between the call returning and the verdict being recorded.
func (e *Engine) runAgent(ctx context.Context, task *store.Task, phase string,
	role resolved, args []string) (agent.Result, store.Step, error) {

	// The step records the ROLE, not the CLI. A timeline that said "codex"
	// for a review and "codex" for the implementation would be unreadable
	// once both roles can run through the same binary, and every reader of
	// the steps table — the two-lane timeline, the convergence chart, the
	// oscillation fingerprint — keys off this value.
	name := role.Role
	transcript := filepath.Join(e.runDir(*task),
		fmt.Sprintf("%s-%d-%s.jsonl", phase, task.Iteration, name))

	// Required writable mounts must exist before wrapping: bubblewrap aborts
	// on a missing --bind source. Optional paths use --bind-try instead and
	// are not created here.
	if err := sandbox.EnsureDirs(e.runDir(*task)); err != nil {
		return agent.Result{}, store.Step{}, err
	}
	// State and mounts are keyed by the CLI, which is what actually reads
	// them, not by the role.
	if err := e.prepareAgentState(*task, role.Agent); err != nil {
		return agent.Result{}, store.Step{}, err
	}

	step, err := e.Store.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: phase, Iteration: task.Iteration,
		Agent: name, Provider: role.Provider, RunSeq: task.RunSeq,
		TranscriptPath: transcript,
	})
	if err != nil {
		return agent.Result{}, store.Step{}, err
	}
	e.notify(task.ID)

	// Usage is accumulated across attempts. A retried step really did spend
	// the tokens of the attempt that failed, and dropping them would make
	// every cost figure on the dashboard an under-report.
	var res agent.Result
	var spent agent.Result

retry:
	for attempt := 1; ; attempt++ {
		res, err = role.Runner.Run(ctx, agent.RunSpec{
			Args:           args,
			Dir:            task.WorktreeDir,
			TranscriptPath: transcript,
			Timeout:        e.Cfg.StepTimeout,
			Attempt:        attempt,
			Sandbox:        e.Sandbox,
			SandboxSpec:    e.sandboxSpec(*task, role.Agent, role.Writable),
			Env:            e.agentEnv(role),
			OnEvent:        e.progressNotifier(task.ID),
		})
		if err != nil {
			return agent.Result{}, store.Step{}, err
		}
		spent.CostUSD += res.CostUSD
		spent.InputTokens += res.InputTokens
		spent.OutputTokens += res.OutputTokens

		// res.Canceled is redundant with !res.Retryable now that the runner
		// clears Retryable on a cancel, and is stated anyway: retrying a run
		// the operator stopped would restart the agent three times over, each
		// attempt running to its own step timeout. That is the failure this
		// guard exists to make impossible to reintroduce.
		if res.ErrMsg == "" || res.Canceled || !res.Retryable || attempt >= maxRetries {
			break
		}
		delay := e.RetryBackoff * time.Duration(1<<(attempt-1))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			// Deliberately not "return res, step, ctx.Err()", as this used to
			// be. That is a *machinery* error to dispatch, which sends it into
			// failTask and marks the task permanently failed with "context
			// canceled" — and failTask's own writes use the same cancelled
			// context, so the failure may not even land. It also returned
			// before FinishStep below, leaving the step row "running" forever
			// with no daemon restart coming to sweep it. Fall out instead and
			// close the step honestly.
			res.Canceled = true
			if res.ErrMsg == "" {
				res.ErrMsg = "stopped before retry"
			}
			break retry
		}
	}

	if res.ErrMsg != "" && agent.IsAuthFailure(res.ErrMsg) {
		e.Pause(fmt.Sprintf("%s is not authenticated: %s", name, res.ErrMsg))
	}

	// The verdict is attached by recordFindings; here we only close the step.
	// Usage is the total across attempts, not just the last one.
	res.CostUSD = spent.CostUSD
	res.InputTokens = spent.InputTokens
	res.OutputTokens = spent.OutputTokens

	step.ExitCode = res.ExitCode
	step.CostUSD = res.CostUSD
	step.InputTokens = res.InputTokens
	step.OutputTokens = res.OutputTokens
	step.ErrMsg = res.ErrMsg
	step.TranscriptPath = transcript
	// A killed agent reports "signal: killed", which is what a crash reports
	// too. res.Canceled is the only thing that knows the difference.
	step.Interrupted = res.Canceled
	// Detached from ctx on purpose. A stopped or shut-down step really did end,
	// and we know its exit code and its usage; writing that with the context
	// that was just cancelled leaves the row "running" and the dashboard's live
	// pane spinning forever. Recover's sweep stays the safety net for a hard
	// kill, not the normal path, and the shutdown grace period budgets exactly
	// this bookkeeping.
	if err := e.Store.FinishStep(context.WithoutCancel(ctx), step, nil); err != nil {
		return agent.Result{}, store.Step{}, err
	}
	e.notify(task.ID)
	return res, step, nil
}

// notifyInterval is how often a running agent reports progress to the
// dashboard. The page reloads on every notification, so this trades staleness
// against churn: two seconds keeps a live transcript visibly moving without
// reloading the page on every line an agent prints.
const notifyInterval = 2 * time.Second

// progressNotifier returns an OnEvent hook that pokes the dashboard while an
// agent is still running.
//
// Without it a turn is silent from start to finish: the board would show a
// step as "running" and the live pane would hold whatever the transcript said
// when the page last loaded, for however many minutes the turn takes. The
// analysis wizard made that especially visible, since one analysis is a single
// long run with nothing else happening to trigger a reload.
//
// It runs on the runner's reader goroutine and must not block. Broadcast never
// does, and the throttle is a compare-and-swap rather than a lock.
func (e *Engine) progressNotifier(id int64) func(agent.Event) {
	var last atomic.Int64
	return func(agent.Event) {
		now := time.Now().UnixNano()
		prev := last.Load()
		if now-prev < int64(notifyInterval) {
			return
		}
		// One notification wins the interval; the losers skip rather than
		// queue, because a dropped poke costs nothing — the next event is
		// along in a moment and the page re-reads everything anyway.
		if !last.CompareAndSwap(prev, now) {
			return
		}
		e.notify(id)
	}
}

// recordFindings attaches a verdict and its findings to the step the caller
// just closed.
func (e *Engine) recordFindings(ctx context.Context, task *store.Task, step store.Step, v agent.Verdict) error {
	step.Verdict = v.Verdict
	var findings []store.Finding
	for _, f := range v.Findings {
		// Ask Blocking about this finding alone; keying a set by summary
		// would mark a nit blocking whenever a major shared its wording.
		one := agent.Verdict{Findings: []agent.Finding{f}}
		findings = append(findings, store.Finding{
			Severity: string(f.Severity),
			File:     f.FileOrEmpty(),
			Line:     f.LineOrZero(),
			Summary:  f.Summary,
			Detail:   f.Detail,
			Blocking: len(one.Blocking(task.BlockingSeverity)) == 1,
		})
	}
	if err := e.Store.FinishStep(ctx, step, findings); err != nil {
		return err
	}
	// Findings the loop deliberately did not act on go on the repository's
	// backlog rather than being displayed once and thrown away. Deliberately
	// dropped on error: a task's outcome does not depend on its backlog, and
	// failing a turn that otherwise succeeded over a mislaid nit would be the
	// worse trade by a wide margin.
	_ = e.recordBacklogFindings(ctx, task, findings)
	e.notify(task.ID)
	return nil
}

// finish commits anything outstanding, pushes, and opens a draft PR.
//
// finish must be idempotent. The task's state only becomes "done" on the
// next Next call, so a daemon that exits between finish returning and that
// save leaves the task in "finishing"; recovery then re-dispatches ActFinish
// against a task whose PR already exists and whose worktree may already be
// gone. Two things make the repeat safe: this early return, and GhOpener
// returning the existing PR instead of failing to create a duplicate.
func (e *Engine) finish(ctx context.Context, task *store.Task) (*loop.Outcome, error) {
	wt := e.worktreeOf(*task)

	if task.PRURL != "" {
		// The PR was already opened on a previous attempt. All that may be
		// outstanding is worktree cleanup.
		if err := e.WT.Remove(ctx, wt); err != nil {
			fmt.Fprintf(os.Stderr, "overseer: task %d: remove worktree: %v\n", task.ID, err)
		}
		return &loop.Outcome{}, nil
	}

	if _, err := e.WT.Commit(ctx, wt, "overseer: final state"); err != nil {
		return &loop.Outcome{Failed: true, ErrMsg: "commit: " + err.Error()}, nil
	}
	has, err := e.WT.HasCommits(ctx, wt)
	if err != nil {
		return &loop.Outcome{Failed: true, ErrMsg: err.Error()}, nil
	}
	if !has {
		return &loop.Outcome{Failed: true,
			ErrMsg: "nothing was committed; refusing to open an empty pull request"}, nil
	}
	pushed, err := e.WT.Push(ctx, wt)
	if err != nil {
		// The worktree is deliberately left in place so the work is not lost.
		return &loop.Outcome{Failed: true, ErrMsg: "push: " + err.Error()}, nil
	}
	if !pushed {
		// No remote to push to, which is a repository overseer can perfectly
		// well work on — one it created itself, most obviously. The work is
		// committed on the branch, which is the durable record either way, so
		// this is done rather than failed. Add a remote later and the pull
		// request flow resumes with no other change.
		//
		// Nothing is written to say so. A done task with no pull request URL
		// is already exactly that statement, and the board reads it — putting
		// it in ErrMsg instead would render a successful task in the styling
		// reserved for ones that broke.
		//
		// The worktree is kept, unlike the pull-request path: there is no pull
		// request to read the change in, so the checkout is the only place it
		// can be looked at.
		e.notify(task.ID)
		return &loop.Outcome{}, nil
	}

	plan, _ := os.ReadFile(filepath.Join(wt.Dir, "PLAN.md"))
	base := task.BaseRef
	if len(base) > 7 && base[:7] == "origin/" {
		base = base[7:]
	}
	url, err := e.PR.Open(ctx, worktree.PRRequest{
		Worktree:   wt,
		Title:      worktree.PRTitle(task.Headline()),
		Body:       worktree.PRBody(task.Goal, string(plan), "No blocking findings remained."),
		BaseBranch: base,
	})
	if err != nil {
		return &loop.Outcome{Failed: true, ErrMsg: "open pr: " + err.Error()}, nil
	}
	task.PRURL = url
	if err := e.Store.SaveTask(ctx, *task); err != nil {
		return nil, err
	}

	// Only now is it safe to remove the worktree; the branch survives.
	if err := e.WT.Remove(ctx, wt); err != nil {
		fmt.Fprintf(os.Stderr, "overseer: task %d: remove worktree: %v\n", task.ID, err)
	}
	e.notify(task.ID)
	return &loop.Outcome{}, nil
}

func (e *Engine) runDir(task store.Task) string {
	return filepath.Join(e.Cfg.RunsDir(), task.RunSlug())
}

func (e *Engine) worktreeOf(task store.Task) worktree.Worktree {
	return worktree.Worktree{
		RepoPath: task.RepoPath,
		Dir:      task.WorktreeDir,
		Branch:   task.Branch,
		BaseRef:  task.BaseRef,
	}
}

func (e *Engine) notify(taskID int64) {
	if e.OnChange != nil {
		e.OnChange(taskID)
	}
}

// sandboxSpec builds the mounts one agent needs.
//
// The rule is: an empty tmpfs over $HOME, then re-expose exactly what the
// agent must have. Anything not listed here — other repositories, dotfiles,
// SSH keys, the overseer database — is simply absent.
// sandboxSpec confines one agent run. writable says whether the worktree is
// exposed read-write, and it comes from the ROLE rather than being inferred
// from the agent's name: with roles free to pick either CLI, "claude means the
// coder" stopped being true, and a reviewer that could write would be able to
// edit the very diff it was asked to judge.
func (e *Engine) sandboxSpec(task store.Task, agentName string, writable bool) sandbox.Spec {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	spec := sandbox.Spec{
		HomeDir: home,
		WorkDir: task.WorktreeDir,
		PathEnv: os.Getenv("PATH"),
		// See F2: the daemon's own environment must not reach the agent
		// unattended. AllowedEnv keeps a fixed allowlist plus whatever the
		// operator opted into via sandbox_env_passthrough; everything else —
		// GITHUB_TOKEN, AWS_*, and the rest — is left behind by bwrap's
		// --clearenv.
		Env: sandbox.AllowedEnv(os.Environ(), e.Cfg.SandboxEnvPassthrough),
	}

	// The agent binary. Installed as a symlink into a versioned directory
	// under $HOME, so both directories are needed.
	binary := e.Cfg.ClaudeBin
	if agentName == "codex" {
		binary = e.Cfg.CodexBin
	}
	for _, m := range sandbox.BinMounts(binary) {
		spec = spec.AddAt(m.Src, m.Dest, m.Write)
	}

	spec = agentHomeMounts(spec, home, agentName, e.runDir(task))

	// The work itself. The worktree's .git is a file pointing into the
	// repository's shared git directory, so that directory must be readable
	// and this worktree's own administrative directory writable, or even
	// `git status` fails. Both were resolved with rev-parse when the worktree
	// was created; they are never derived from RepoPath, because a submitted
	// path may itself be a linked worktree whose .git is a file.
	spec = spec.
		Add(task.GitCommonDir, false).
		Add(task.GitAdminDir, writable).
		Add(task.WorktreeDir, writable)

	// Codex reads the schema and writes its last-message file itself, so
	// both need mounts. Only this task's run directory is exposed — never
	// the whole data directory, which holds the database.
	spec = spec.
		Add(e.SchemaPath, false).
		Add(e.runDir(task), true)

	return e.toolchainMounts(spec)
}

// agentHomeMounts installs the agent's state directory and layers its real
// configuration back read-only.
//
// This is the subtle part of the sandbox, so it lives in one place and every
// caller goes through it. The writable parent is a directory of overseer's
// under runDir, NOT the real state directory, with the real configuration
// mounted read-only on top. Mounting the real directory writable and pinning
// config files read-only individually does not work: a config file that does
// not exist yet is skipped by the optional mount, leaving the writable real
// parent exposed, and the agent can simply create it. It then runs on the next
// unsandboxed invocation. `~/.claude/settings.local.json` is absent on a
// typical install, so that hole is the common case rather than an edge case.
//
// Inverting it removes the class of bug: nothing under the real state
// directory is writable, so nothing absent can be planted, and whatever the
// agent does write dies with the run. Sessions still persist for that run's
// own lifetime, which is all `--resume` needs.
func agentHomeMounts(spec sandbox.Spec, home, agentName, runDir string) sandbox.Spec {
	if home == "" {
		return spec
	}
	stateDir := agentStateDirIn(runDir, agentName)
	real := filepath.Join(home, "."+agentName)

	switch agentName {
	case "claude":
		return spec.
			// The per-run directory becomes ~/.claude inside.
			AddAt(stateDir, real, true).
			// Then the real config is layered back, read-only. These are
			// optional because a not-yet-used install lacks them — and being
			// absent is now harmless, since the parent is not the real
			// directory.
			AddOptional(filepath.Join(real, ".credentials.json"), false).
			AddOptional(filepath.Join(real, "settings.json"), false).
			AddOptional(filepath.Join(real, "plugins"), false).
			AddOptional(filepath.Join(real, "skills"), false).
			// ~/.claude.json carries top-level mcpServers — executable
			// configuration — so the agent gets a per-run copy, never the
			// real file.
			AddAt(agentStateFileIn(runDir, "claude.json"),
				filepath.Join(home, ".claude.json"), true)
	case "codex":
		return spec.
			AddAt(stateDir, real, true).
			AddOptional(filepath.Join(real, "auth.json"), false).
			AddOptional(filepath.Join(real, "config.toml"), false).
			AddOptional(filepath.Join(real, "packages"), false)
	}
	return spec
}

// toolchainMounts exposes the build caches an agent needs to be usably fast,
// plus whatever the operator opted into.
func (e *Engine) toolchainMounts(spec sandbox.Spec) sandbox.Spec {
	// The agent's own Go build and module cache — never the operator's real
	// ~/.cache/go-build and ~/go/pkg/mod (see F8). Go's build cache holds
	// trusted, reused-verbatim output blobs, so a write smuggled through the
	// real one would be a persistence channel out of the sandbox: it could
	// get linked into a later *unsandboxed* build on the operator's machine
	// without ever being rebuilt from the source that would have shown the
	// tampering. This directory is overseer's own, under the data directory,
	// so nothing an agent writes here touches anything of the operator's; it
	// is shared across the whole run (not per-task) to keep the speed benefit
	// across iterations, and mounted optional/writable like the other caches
	// below, since New already called sandbox.EnsureDirs on it once at
	// startup.
	goBuild, goMod := e.goCacheDirs()
	spec = spec.AddOptional(goBuild, true).AddOptional(goMod, true)
	spec.Env["GOCACHE"] = goBuild
	spec.Env["GOMODCACHE"] = goMod

	// Other toolchain caches. Without these, a $HOME tmpfs hides them, so the
	// agent's own test runs start from a cold cache and re-download every
	// dependency on every iteration — minutes per turn on a real repository,
	// and an outright failure for a private module that needs credentials
	// this sandbox deliberately withholds. They are derived data, so exposing
	// them writable costs nothing — and unlike Go's build cache, they are
	// integrity-checked on read, which is what makes mounting the operator's
	// real ones an acceptable risk that mounting ~/.cache/go-build was not.
	for _, p := range e.Cfg.SandboxCachePaths {
		spec = spec.AddOptional(os.ExpandEnv(p), true)
	}
	// Operator-supplied extras, for cases the defaults cannot cover — a
	// private module proxy needing ~/.netrc, say. Deliberately not default:
	// mounting credentials hands them to the agent.
	for _, p := range e.Cfg.SandboxExtraReadOnly {
		spec = spec.AddOptional(os.ExpandEnv(p), false)
	}
	for _, p := range e.Cfg.SandboxExtraReadWrite {
		spec = spec.AddOptional(os.ExpandEnv(p), true)
	}
	return spec
}

// analysisSandboxSpec confines a repository analysis.
//
// It differs from a task's sandbox in exactly two ways, both of them
// tightening. The repository is mounted READ-ONLY — including its .git, so
// `git log` and `git ls-files` work and nothing can be written — and the only
// writable path is the proposal's own run directory, for the transcript. An
// analysis that could write would be able to leave a branch, a stash or an
// edit behind in a repository the operator only asked it to read.
func (e *Engine) analysisSandboxSpec(repoPath, runDir, agentName string) sandbox.Spec {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	spec := sandbox.Spec{
		HomeDir: home,
		WorkDir: repoPath,
		PathEnv: os.Getenv("PATH"),
		Env:     sandbox.AllowedEnv(os.Environ(), e.Cfg.SandboxEnvPassthrough),
	}
	for _, m := range sandbox.BinMounts(e.Cfg.Bin(agentName)) {
		spec = spec.AddAt(m.Src, m.Dest, m.Write)
	}
	spec = agentHomeMounts(spec, home, agentName, runDir)
	spec = spec.
		Add(repoPath, false).
		Add(runDir, true)
	return e.toolchainMounts(spec)
}

// scaffoldSandboxSpec confines the one agent turn that writes a new project.
//
// The analysis sandbox with the repository WRITABLE, and that inversion is the
// whole difference. It is safe here for a reason that does not generalise: the
// repository is one overseer created a moment ago and it contains a single
// empty commit, so there is nothing in it to damage and nothing of the
// operator's to leave a mess in. Every other non-task turn keeps the read-only
// mount, and a task's writable tree is a worktree rather than the repository
// itself.
func (e *Engine) scaffoldSandboxSpec(repoPath, runDir, agentName string) sandbox.Spec {
	spec := e.analysisSandboxSpec(repoPath, runDir, agentName)
	// Appended rather than edited in place: mounts apply in order, so a
	// writable mount after the read-only one is what takes effect.
	return spec.Add(repoPath, true)
}

// goCacheDirs are overseer's own Go build and module cache directories, under
// dataDir rather than the operator's real $HOME (see F8). Shared by every
// task and every iteration of a run, not per-task, so the speed benefit a
// warm cache provides survives across the whole run.
func goCacheDirs(dataDir string) (build, mod string) {
	base := filepath.Join(dataDir, "gocache")
	return filepath.Join(base, "go-build"), filepath.Join(base, "pkg", "mod")
}

func (e *Engine) goCacheDirs() (build, mod string) {
	return goCacheDirs(e.Cfg.DataDir)
}

// agentStateDir is the per-task directory that stands in for the agent's real
// state directory inside the sandbox.
func (e *Engine) agentStateDir(task store.Task, agentName string) string {
	return agentStateDirIn(e.runDir(task), agentName)
}

// agentStateFile is a per-task stand-in for a single state file.
func (e *Engine) agentStateFile(task store.Task, name string) string {
	return agentStateFileIn(e.runDir(task), name)
}

// agentStateDirIn and agentStateFileIn are the run-directory-keyed forms.
// A repository analysis has no task, so it addresses its own run directory
// directly; the task forms above are thin wrappers over these so both paths
// lay their state out identically.
func agentStateDirIn(runDir, agentName string) string {
	return filepath.Join(runDir, "state-"+agentName)
}

func agentStateFileIn(runDir, name string) string {
	return filepath.Join(runDir, "state-files", name)
}

// prepareAgentState creates the per-task state directory and seeds the files
// the agent expects to find already populated.
func (e *Engine) prepareAgentState(task store.Task, agentName string) error {
	return prepareAgentStateIn(e.runDir(task), agentName)
}

// prepareAgentStateIn creates the stand-in state directory under runDir and
// seeds the files the agent expects to find already populated.
//
// ~/.claude.json is seeded from the real one because Claude reads project
// state from it, but the copy is never written back: it carries top-level
// mcpServers, which is executable configuration, so changes must not persist.
func prepareAgentStateIn(runDir, agentName string) error {
	if err := sandbox.EnsureDirs(agentStateDirIn(runDir, agentName)); err != nil {
		return err
	}
	if agentName != "claude" {
		return nil
	}

	dest := agentStateFileIn(runDir, "claude.json")
	if err := sandbox.EnsureDirs(filepath.Dir(dest)); err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		return nil // already seeded for this run
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return os.WriteFile(dest, []byte("{}\n"), 0o600)
	}
	src, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		// A fresh install has none; an empty object is a valid starting point.
		return os.WriteFile(dest, []byte("{}\n"), 0o600)
	}
	return os.WriteFile(dest, src, 0o600)
}

// toLoop projects a stored task onto the state machine's view of it.
func toLoop(t store.Task) loop.Task {
	return loop.Task{
		State:            loop.State(t.State),
		Phase:            loop.Phase(t.Phase),
		Iteration:        t.Iteration,
		MaxIterations:    t.MaxIterations,
		BlockingSeverity: t.BlockingSeverity,
		PlanSessionID:    t.PlanSessionID,
		ExecSessionID:    t.ExecSessionID,
		Verify:           t.VerifyCommand != "",
		FindingHashes:    t.FindingHashes,
		ErrMsg:           t.ErrMsg,
	}
}

// applyLoop copies the state machine's output back onto the stored task.
func applyLoop(t *store.Task, lt loop.Task) {
	t.State = string(lt.State)
	t.Phase = string(lt.Phase)
	t.Iteration = lt.Iteration
	t.MaxIterations = lt.MaxIterations
	t.PlanSessionID = lt.PlanSessionID
	t.ExecSessionID = lt.ExecSessionID
	t.FindingHashes = lt.FindingHashes
	if lt.ErrMsg != "" {
		t.ErrMsg = lt.ErrMsg
	}
}

func toAgentFindings(in []store.Finding) []agent.Finding {
	var out []agent.Finding
	for _, f := range in {
		af := agent.Finding{
			Severity: agent.Severity(f.Severity),
			Summary:  f.Summary,
			Detail:   f.Detail,
		}
		if f.File != "" {
			file := f.File
			af.File = &file
		}
		if f.Line != 0 {
			line := f.Line
			af.Line = &line
		}
		out = append(out, af)
	}
	return out
}
