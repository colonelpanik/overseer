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

	base := worktree.Slugify(bt.Goal)
	task := store.Task{
		RepoPath:         bt.Repo,
		Goal:             strings.TrimSpace(bt.Goal),
		Constraints:      strings.Join(bt.Constraints, "\n"),
		State:            string(loop.StateQueued),
		MaxIterations:    e.Cfg.MaxIterations,
		BlockingSeverity: severity,
		VerifyCommand:    verify,
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
			e.notify(created.ID)
			return created, nil
		}
		if attempt >= 20 || !strings.Contains(strings.ToLower(err.Error()), "unique") {
			return store.Task{}, err
		}
	}
}

// SubmitBatch queues every task in a batch, returning what it created.
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
