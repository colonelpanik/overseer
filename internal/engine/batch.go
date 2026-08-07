package engine

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"overseer/internal/loop"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// BatchTask is one entry submitted to the engine. Task 11 (the CLI) parses a
// task file into these; the shape is fixed here so that work does not need
// to change it.
type BatchTask struct {
	Repo             string
	Goal             string
	Constraints      []string
	BlockingSeverity string
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

	base := worktree.Slugify(bt.Goal)
	task := store.Task{
		RepoPath:         bt.Repo,
		Goal:             strings.TrimSpace(bt.Goal),
		Constraints:      strings.Join(bt.Constraints, "\n"),
		State:            string(loop.StateQueued),
		MaxIterations:    e.Cfg.MaxIterations,
		BlockingSeverity: severity,
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
