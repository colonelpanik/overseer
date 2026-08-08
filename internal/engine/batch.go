package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
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

// Submit queues one task after checking the repository is usable.
func (e *Engine) Submit(ctx context.Context, bt BatchTask) (store.Task, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = bt.Repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return store.Task{}, fmt.Errorf("%s is not a git repository: %v: %s",
			bt.Repo, err, strings.TrimSpace(string(out)))
	}

	severity := bt.BlockingSeverity
	if severity == "" {
		severity = e.Cfg.BlockingSeverity
	}
	verify := bt.Verify
	if verify == "" {
		verify = e.Cfg.VerifyCommand
	}
	costCap := bt.CostCap
	if costCap == 0 {
		costCap = e.Cfg.TaskCapUSD
	}

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
		RepoPath:         bt.Repo,
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

// Abandon marks a task failed on purpose, keeping its branch and worktree.
//
// It refuses a task a worker goroutine currently owns. Without this guard,
// Abandon would write "failed" here, but the owning worker holds its own
// now-stale in-memory copy of the task from before this call, and its very
// next SaveTask — at the end of whatever action it is mid-dispatch on —
// overwrites this with whatever state its own loop.Next computed, silently
// reverting the abandon a moment later. Failing loudly is strictly better
// than that: the operator sees the abandon did not take effect, rather than
// watching a task that looked stopped quietly resume on its own.
//
// This does not make it possible to stop a task that is actually running:
// the worker keeps going until its current action returns, however long that
// takes. Doing that needs a cooperative cancellation signal the worker checks
// between dispatches, which this guard does not add — it only turns a silent
// failure into an honest one.
func (e *Engine) Abandon(ctx context.Context, taskID int64) error {
	if e.isRunning(taskID) {
		return fmt.Errorf("task %d is currently running; cannot abandon it while a worker is driving it", taskID)
	}

	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	task.State = string(loop.StateFailed)
	if task.ErrMsg == "" {
		task.ErrMsg = "abandoned by the operator"
	}
	if err := e.Store.SaveTask(ctx, task); err != nil {
		return err
	}
	e.notify(taskID)
	return nil
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
