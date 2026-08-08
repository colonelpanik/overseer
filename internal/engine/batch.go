package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"overseer/internal/config"
	"overseer/internal/loop"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// BatchTask is one entry submitted to the engine, whether it came from a
// parsed task file or was built directly (as the engine's own tests do). The
// yaml tags let ParseBatch decode a task file straight into this shape.
type BatchTask struct {
	Repo             string   `yaml:"repo"`
	Goal             string   `yaml:"goal"`
	Constraints      []string `yaml:"constraints"`
	BlockingSeverity string   `yaml:"blocking_severity"`
	// Verify overrides the daemon's verify_command for this task. Empty
	// falls back to the daemon default.
	Verify string `yaml:"verify"`
	// CostCap is this task's advisory spend ceiling in USD. Zero falls back to
	// the daemon's task_cap_usd.
	CostCap float64 `yaml:"cost_cap"`
	// DependsOn names tasks — by slug — that must reach done before this one
	// is claimed. A slug may name a task earlier in the same batch or one
	// already in the database.
	DependsOn []string `yaml:"depends_on"`
}

// Batch is a submitted task file. It carries tasks only: daemon settings
// live in the config file so a second submit cannot change a live run.
type Batch struct {
	Tasks []BatchTask `yaml:"tasks"`
}

// ParseBatch decodes and validates a task file.
func ParseBatch(raw []byte) (Batch, error) {
	var b Batch
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	// KnownFields rejects daemon settings such as max_parallel, which belong
	// in the config file rather than a batch.
	dec.KnownFields(true)
	// An empty document decodes to io.EOF rather than a nil error with a
	// zero-value Batch; the len(b.Tasks) == 0 check below reports that case
	// as "no tasks" instead of treating it as a hard parse error.
	if err := dec.Decode(&b); err != nil && !errors.Is(err, io.EOF) {
		return Batch{}, fmt.Errorf("parse batch: %w", err)
	}
	if len(b.Tasks) == 0 {
		return Batch{}, errors.New("parse batch: no tasks")
	}
	for i, t := range b.Tasks {
		if strings.TrimSpace(t.Repo) == "" {
			return Batch{}, fmt.Errorf("parse batch: task %d has no repo", i+1)
		}
		if strings.TrimSpace(t.Goal) == "" {
			return Batch{}, fmt.Errorf("parse batch: task %d has no goal", i+1)
		}
		if t.CostCap < 0 {
			return Batch{}, fmt.Errorf("parse batch: task %d has a negative cost_cap", i+1)
		}
		if t.BlockingSeverity == "" {
			continue
		}
		valid := false
		for _, s := range config.ValidSeverities {
			if t.BlockingSeverity == s {
				valid = true
				break
			}
		}
		if !valid {
			return Batch{}, fmt.Errorf("parse batch: task %d has blocking_severity %q, want one of %v",
				i+1, t.BlockingSeverity, config.ValidSeverities)
		}
	}
	return b, nil
}

// Submit queues one task against a registered repository.
//
// The repository is registered as a side effect, so a task file that names a
// path — as every existing one does — keeps working unchanged while the repo
// list fills itself in. `repo:` may equally name a registered repository by its
// slug, which is the point of registering one: the path is typed once.
//
// Settings resolve task > repo > daemon default. An empty value at any level
// means "fall through", never "off": a task that wants no verify gate is one
// whose repository and daemon also configure none.
func (e *Engine) Submit(ctx context.Context, bt BatchTask) (store.Task, error) {
	repo, err := e.ResolveRepo(ctx, bt.Repo)
	if err != nil {
		return store.Task{}, err
	}

	severity := firstNonEmpty(bt.BlockingSeverity, repo.BlockingSeverity, e.Cfg.BlockingSeverity)
	verify := firstNonEmpty(bt.Verify, repo.VerifyCommand, e.Cfg.VerifyCommand)
	costCap := firstNonZero(bt.CostCap, repo.CostCapUSD, e.Cfg.TaskCapUSD)

	// Resolve dependency slugs before creating anything: a submit that names a
	// task nobody has heard of should fail outright rather than queue a task
	// whose stated precondition silently does not exist.
	depIDs := make([]int64, 0, len(bt.DependsOn))
	for _, slug := range bt.DependsOn {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}
		dep, err := e.Store.GetTaskBySlug(ctx, slug)
		if err != nil {
			return store.Task{}, fmt.Errorf("depends_on %q: %w", slug, err)
		}
		depIDs = append(depIDs, dep.ID)
	}

	base := worktree.Slugify(bt.Goal)
	task := store.Task{
		RepoID:           repo.ID,
		RepoPath:         repo.Path,
		Goal:             strings.TrimSpace(bt.Goal),
		Constraints:      strings.Join(bt.Constraints, "\n"),
		State:            string(loop.StateQueued),
		MaxIterations:    e.Cfg.MaxIterations,
		BlockingSeverity: severity,
		VerifyCommand:    verify,
		CostCapUSD:       costCap,
	}

	// Slugs are unique because they name a branch and a directory. Suffix on
	// collision rather than failing the submit.
	for attempt := 0; ; attempt++ {
		task.Slug = base
		if attempt > 0 {
			task.Slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		created, err := e.Store.CreateTask(ctx, task)
		if err == nil {
			if len(depIDs) > 0 {
				if err := e.Store.SetTaskDeps(ctx, created.ID, depIDs); err != nil {
					return store.Task{}, err
				}
			}
			e.notify(created.ID)
			return created, nil
		}
		if attempt >= 20 || !strings.Contains(strings.ToLower(err.Error()), "unique") {
			return store.Task{}, err
		}
	}
}

// firstNonEmpty implements the task > repo > daemon fallback for a string
// setting, where empty at each level means "fall through".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstNonZero is the same fallback for a numeric setting. Zero means "no cap
// configured here", which is why it falls through rather than winning.
func firstNonZero(values ...float64) float64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// SubmitBatch queues every task in a batch, returning what it created.
//
// Tasks are submitted in file order, so a depends_on may name a task earlier
// in the same batch by the slug it will get — which, for a colliding goal, is
// the suffixed one. Naming a task later in the batch fails: nothing has
// created it yet, and guessing its slug ahead of the collision suffix would
// only be right some of the time.
func (e *Engine) SubmitBatch(ctx context.Context, b Batch) ([]store.Task, error) {
	var out []store.Task
	for i, bt := range b.Tasks {
		task, err := e.Submit(ctx, bt)
		if err != nil {
			return out, fmt.Errorf("task %d: %w", i+1, err)
		}
		out = append(out, task)
	}
	return out, nil
}

// ContinueEscalated grants a parked task another budget of iterations and
// returns it to its phase's working state.
func (e *Engine) ContinueEscalated(ctx context.Context, taskID int64, extra int) error {
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State != string(loop.StateEscalated) {
		return fmt.Errorf("task %d is %s, not escalated", taskID, task.State)
	}
	applyLoop(&task, loop.GrantMoreIterations(toLoop(task), extra))
	task.ErrMsg = ""
	if err := e.Store.SaveTask(ctx, task); err != nil {
		return err
	}
	e.notify(taskID)
	return nil
}

// Abandon ends a task on purpose, keeping its branch and worktree.
//
// It reaches a running task now: the request is lodged with the worker driving
// it, which applies it from the copy it is actually holding. This used to
// refuse outright, because a write from here would be overwritten by that
// worker's next SaveTask — an honest failure, but still a task the operator
// could not stop.
//
// The task lands in "abandoned", not "failed". Failed means the machinery or
// the agent failed; an operator's decision reading the same way makes the
// board's one urgent signal mean two different things.
func (e *Engine) Abandon(ctx context.Context, taskID int64, opts StopOpts) error {
	return e.applyStop(ctx, taskID, stopRequest{
		Kind: StopAbandon,
		Msg:  reasonOr(opts.Reason, "abandoned by the operator"),
		Hard: opts.Now,
	})
}

// StopOpts is how far a stop goes.
type StopOpts struct {
	// Now kills the agent mid-turn instead of letting the current step finish.
	//
	// The default is to wait, because a turn seconds from finishing is worth
	// far more than the wait: stopping at the boundary costs nothing and leaves
	// nothing half-written. Now is for a step that is wedged, where waiting out
	// the step timeout is the thing being avoided.
	Now bool
	// Reason is recorded on the interrupted step, and on the task for an
	// abandon. Empty takes a sensible default.
	Reason string
}

// Stop parks a task where it is. Starting it again re-dispatches the action its
// state names, so nothing is lost and nothing is repeated from the beginning.
func (e *Engine) Stop(ctx context.Context, taskID int64, opts StopOpts) error {
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if loop.IsTerminal(task.State) {
		return fmt.Errorf("task %d is %s; there is nothing to stop", taskID, task.State)
	}
	if task.Stopped() {
		// Not an error: pressing Stop on a stopped task is how an operator
		// escalates to Stop now, and refusing would make the escalation
		// impossible.
		if !opts.Now {
			return nil
		}
	}

	// Persisted first, so a daemon that dies in the window leaves the task
	// parked rather than running. The request that follows lives only in
	// memory, deliberately: a "please stop" flag nobody can see, applied by a
	// restarted daemon minutes later out of nowhere, is worse than a request
	// that evaporates with the process that was going to honour it.
	if err := e.Store.StopTask(ctx, taskID, true); err != nil {
		return err
	}
	e.notify(taskID)

	// applyStop lodges this with the worker driving the task, or — when nobody
	// is — does nothing beyond the notify, which is right: the row is already
	// written and the scheduler will not pick the task up again.
	return e.applyStop(ctx, taskID, stopRequest{
		Kind: StopPark,
		Msg:  reasonOr(opts.Reason, "stopped by the operator"),
		Hard: opts.Now,
	})
}

// Start clears a stop. The next poll claims the task and loop.Pending
// re-dispatches the action its state names — the same path a restarted daemon
// takes, which is why stopping is a column rather than a state.
func (e *Engine) Start(ctx context.Context, taskID int64) error {
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !task.Stopped() {
		return nil
	}
	if loop.IsTerminal(task.State) {
		return fmt.Errorf("task %d is %s; restart it rather than starting it", taskID, task.State)
	}
	if err := e.Store.StopTask(ctx, taskID, false); err != nil {
		return err
	}
	e.notify(taskID)
	return nil
}

// RestartOpts amends a task on its way into a fresh attempt.
type RestartOpts struct {
	StopOpts
	// Goal and Constraints replace the task's own when non-empty. "Restart it,
	// but this time do not touch the schema" is the usual reason to restart at
	// all, and making the operator edit the task separately loses that.
	Goal        string
	Constraints []string
}

// Restart runs a task again from the top, on its own branch.
//
// The previous attempt is kept: its branch and worktree stay exactly as they
// are, and the new attempt works on <slug>-r<n>. That costs disk, and buys the
// ability to compare against the attempt you are restarting — which is usually
// why you are restarting it.
func (e *Engine) Restart(ctx context.Context, taskID int64, opts RestartOpts) error {
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	// A task with a pull request cannot be restarted safely in either
	// direction. Keeping the URL makes finish short-circuit, so the new attempt
	// would skip to worktree removal and be marked done having done nothing;
	// clearing it silently force-pushes over an open pull request's branch,
	// because the opener returns the existing one rather than creating a
	// duplicate.
	if task.PRURL != "" {
		return fmt.Errorf("task %d already opened %s; restarting it would rewrite that pull request's branch",
			taskID, task.PRURL)
	}

	next := task
	next.State = string(loop.StateQueued)
	next.Phase = ""
	next.Iteration = 0
	// Back to the configured budget, not whatever Continue accumulated: a task
	// restarted after three Continues should not begin with a budget of forty.
	next.MaxIterations = e.Cfg.MaxIterations
	if strings.TrimSpace(opts.Goal) != "" {
		next.Goal = strings.TrimSpace(opts.Goal)
	}
	if len(opts.Constraints) > 0 {
		next.Constraints = strings.Join(opts.Constraints, "\n")
	}

	return e.applyStop(ctx, taskID, stopRequest{
		Kind:    StopRestart,
		Msg:     reasonOr(opts.Reason, "restarted by the operator"),
		Hard:    opts.Now,
		Restart: next,
	})
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return strings.TrimSpace(reason)
}

// RaiseCap sets a task's advisory spend ceiling. The dashboard offers this
// beside the budget banner, so an operator who has decided the task is worth
// finishing can clear the warning without editing the task file.
func (e *Engine) RaiseCap(ctx context.Context, taskID int64, cap float64) error {
	if cap < 0 {
		return fmt.Errorf("cost cap %.2f must not be negative", cap)
	}
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	task.CostCapUSD = cap
	if err := e.Store.SaveTask(ctx, task); err != nil {
		return err
	}
	e.notify(taskID)
	return nil
}

// SetBlockingSeverity raises or lowers a task's blocking threshold.
//
// This is the answer to a task ping-ponging on nits: the dashboard shows the
// recurring fingerprint and offers the threshold change that would let the
// task converge, instead of leaving the operator to abandon it or watch it
// spend another ten iterations.
func (e *Engine) SetBlockingSeverity(ctx context.Context, taskID int64, severity string) error {
	valid := false
	for _, s := range config.ValidSeverities {
		if severity == s {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("blocking_severity %q must be one of %v", severity, config.ValidSeverities)
	}
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	task.BlockingSeverity = severity
	if err := e.Store.SaveTask(ctx, task); err != nil {
		return err
	}
	e.notify(taskID)
	return nil
}

// ReleaseDeps clears a task's dependencies so it can be claimed immediately.
// A dependency that failed would otherwise hold its dependents forever, and
// the operator is the one who decides whether that still matters.
func (e *Engine) ReleaseDeps(ctx context.Context, taskID int64) error {
	if _, err := e.Store.GetTask(ctx, taskID); err != nil {
		return err
	}
	if err := e.Store.SetTaskDeps(ctx, taskID, nil); err != nil {
		return err
	}
	e.notify(taskID)
	return nil
}

// TakeOverHint returns the commands for driving a parked task by hand.
func (e *Engine) TakeOverHint(task store.Task) string {
	session := task.PlanSessionID
	if task.Phase == string(loop.PhaseExec) && task.ExecSessionID != "" {
		session = task.ExecSessionID
	}
	if session == "" {
		return fmt.Sprintf("cd %s", task.WorktreeDir)
	}
	return fmt.Sprintf("cd %s && claude --resume %s", task.WorktreeDir, session)
}
