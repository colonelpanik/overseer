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

	// running guards against two workers driving the same task.
	mu      sync.Mutex
	running map[int64]bool

	// pauseReason is non-empty when the whole run is halted, which happens
	// on an authentication failure: every task would fail identically.
	pauseReason string
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
		running:      map[int64]bool{},
	}, nil
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
			if !e.claim(t.ID) {
				continue
			}
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				defer e.release(id)
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				if err := e.RunTask(ctx, id); err != nil && ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "overseer: task %d: %v\n", id, err)
				}
			}(t.ID)
		}

		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-ticker.C:
		}
	}
}

func (e *Engine) claim(id int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running[id] {
		return false
	}
	e.running[id] = true
	return true
}

func (e *Engine) release(id int64) {
	e.mu.Lock()
	delete(e.running, id)
	e.mu.Unlock()
}

// isRunning reports whether a worker goroutine currently owns taskID, i.e.
// holds an in-memory copy of it inside RunTask between claim and release.
func (e *Engine) isRunning(id int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id]
}

// RunTask drives one task from its current state to a terminal state.
func (e *Engine) RunTask(ctx context.Context, taskID int64) error {
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
			if len(action.Findings) == 0 {
				// Nothing to feed back: fall through to a fresh turn rather
				// than resuming with an empty review.
				if action.Kind == loop.ActClaudePlanResume {
					action.Kind = loop.ActClaudePlan
				} else {
					action.Kind = loop.ActClaudeExec
				}
				action.ResumeSessionID = ""
			}
		}
		// The same two guards the main loop applies. Recovery is a dispatch
		// like any other: another worker may have paused the run since this
		// task's entry check, and a harness error here must be persisted or
		// the task stays claimable and the scheduler repeats this exact
		// action every poll.
		if reason := e.PauseReason(); reason != "" {
			return nil
		}
		outcome, err := e.dispatch(ctx, &task, action)
		if err != nil {
			return e.failTask(ctx, &task, err)
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

		// Re-check the pause before every dispatch, not only on entry.
		// Another worker may have hit an authentication failure while this
		// task was mid-step, and with MaxParallel above 1 this task would
		// otherwise keep dispatching doomed calls despite the advertised
		// global pause. The task keeps its current state and resumes from
		// its pending action once the operator clears the pause.
		if reason := e.PauseReason(); reason != "" {
			return nil
		}

		outcome, err := e.dispatch(ctx, &task, action)
		if err != nil {
			// A genuine harness failure — an unwritable transcript, a
			// database error — would otherwise leave the task in a
			// non-terminal state that ClaimableTasks keeps returning, and
			// the scheduler would retry it every poll forever.
			return e.failTask(ctx, &task, err)
		}
		last = outcome
	}
}

// failTask records a harness failure and closes any step left running, so a
// task can never be re-claimed and retried indefinitely.
func (e *Engine) failTask(ctx context.Context, task *store.Task, cause error) error {
	msg := cause.Error()
	fmt.Fprintf(os.Stderr, "overseer: task %d failed: %v\n", task.ID, cause)

	if _, err := e.Store.FailRunningSteps(ctx, task.ID, msg); err != nil {
		fmt.Fprintf(os.Stderr, "overseer: task %d: close running steps: %v\n", task.ID, err)
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
	wt, err := e.WT.Create(ctx, task.RepoPath, task.Slug)
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
	args := agent.ClaudeArgs(agent.ClaudeOpts{Prompt: prompt, ResumeSessionID: resume})
	res, _, err := e.runAgent(ctx, task, phase, "claude", e.Claude, args)
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
	lastPath := filepath.Join(e.runDir(*task),
		fmt.Sprintf("%s-%d-verdict.json", phase, task.Iteration))
	args := agent.CodexArgs(agent.CodexOpts{
		Prompt:          prompt,
		SchemaPath:      e.SchemaPath,
		LastMessagePath: lastPath,
	})
	res, step, err := e.runAgent(ctx, task, phase, "codex", e.Codex, args)
	if err != nil {
		return nil, err
	}
	if res.ErrMsg != "" {
		return &loop.Outcome{Failed: true, ErrMsg: res.ErrMsg}, nil
	}

	raw, err := os.ReadFile(lastPath)
	if err != nil {
		// Fall back to the last agent message; either way, no verdict means
		// no approval.
		raw = []byte(res.FinalText)
	}
	verdict, parseErr := agent.ParseVerdict(raw)
	if parseErr != nil {
		// One stricter re-ask before giving up. A second failure fails the
		// task: unparseable output is never approval.
		retryPrompt := prompt + "\n\nYour previous response could not be parsed: " +
			parseErr.Error() + "\nRespond with the JSON object required by the " +
			"output schema and nothing else."
		args = agent.CodexArgs(agent.CodexOpts{
			Prompt: retryPrompt, SchemaPath: e.SchemaPath, LastMessagePath: lastPath,
		})
		res, step, err = e.runAgent(ctx, task, phase, "codex", e.Codex, args)
		if err != nil {
			return nil, err
		}
		raw, readErr := os.ReadFile(lastPath)
		if readErr != nil {
			raw = []byte(res.FinalText)
		}
		verdict, parseErr = agent.ParseVerdict(raw)
		if parseErr != nil {
			return &loop.Outcome{Failed: true,
				ErrMsg: "codex returned no parseable verdict: " + parseErr.Error()}, nil
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
func (e *Engine) runAgent(ctx context.Context, task *store.Task, phase, name string,
	runner *agent.Runner, args []string) (agent.Result, store.Step, error) {

	transcript := filepath.Join(e.runDir(*task),
		fmt.Sprintf("%s-%d-%s.jsonl", phase, task.Iteration, name))

	// Required writable mounts must exist before wrapping: bubblewrap aborts
	// on a missing --bind source. Optional paths use --bind-try instead and
	// are not created here.
	if err := sandbox.EnsureDirs(e.runDir(*task)); err != nil {
		return agent.Result{}, store.Step{}, err
	}
	if err := e.prepareAgentState(*task, name); err != nil {
		return agent.Result{}, store.Step{}, err
	}

	step, err := e.Store.StartStep(ctx, store.Step{
		TaskID: task.ID, Phase: phase, Iteration: task.Iteration,
		Agent: name, TranscriptPath: transcript,
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

	for attempt := 1; ; attempt++ {
		res, err = runner.Run(ctx, agent.RunSpec{
			Args:           args,
			Dir:            task.WorktreeDir,
			TranscriptPath: transcript,
			Timeout:        e.Cfg.StepTimeout,
			Attempt:        attempt,
			Sandbox:        e.Sandbox,
			SandboxSpec:    e.sandboxSpec(*task, name),
		})
		if err != nil {
			return agent.Result{}, store.Step{}, err
		}
		spent.CostUSD += res.CostUSD
		spent.InputTokens += res.InputTokens
		spent.OutputTokens += res.OutputTokens

		if res.ErrMsg == "" || !res.Retryable || attempt >= maxRetries {
			break
		}
		delay := e.RetryBackoff * time.Duration(1<<(attempt-1))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return res, step, ctx.Err()
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
	if err := e.Store.FinishStep(ctx, step, nil); err != nil {
		return agent.Result{}, store.Step{}, err
	}
	e.notify(task.ID)
	return res, step, nil
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
	if err := e.WT.Push(ctx, wt); err != nil {
		// The worktree is deliberately left in place so the work is not lost.
		return &loop.Outcome{Failed: true, ErrMsg: "push: " + err.Error()}, nil
	}

	plan, _ := os.ReadFile(filepath.Join(wt.Dir, "PLAN.md"))
	base := task.BaseRef
	if len(base) > 7 && base[:7] == "origin/" {
		base = base[7:]
	}
	url, err := e.PR.Open(ctx, worktree.PRRequest{
		Worktree:   wt,
		Title:      worktree.PRTitle(task.Goal),
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
	return filepath.Join(e.Cfg.RunsDir(), task.Slug)
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
func (e *Engine) sandboxSpec(task store.Task, agentName string) sandbox.Spec {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	writable := agentName == "claude" // the reviewer never writes

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
func (e *Engine) analysisSandboxSpec(repoPath, runDir string) sandbox.Spec {
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
	for _, m := range sandbox.BinMounts(e.Cfg.ClaudeBin) {
		spec = spec.AddAt(m.Src, m.Dest, m.Write)
	}
	spec = agentHomeMounts(spec, home, "claude", runDir)
	spec = spec.
		Add(repoPath, false).
		Add(runDir, true)
	return e.toolchainMounts(spec)
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
