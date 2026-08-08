package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"overseer/internal/store"
)

// planFile is the plan's name in the repository root.
//
// A constant because it is load-bearing in five places — the plan prompt, the
// review prompt, the exec prompt, the pull request body, and this — and because
// the write path below must never take a filename from a request. Accepting one
// would turn an editor into an arbitrary-write primitive into any task's
// worktree.
const planFile = "PLAN.md"

// maxPlanBytes bounds what the dashboard will read or accept. Plans routinely
// run to tens of kilobytes; a megabyte is something else.
const maxPlanBytes = 1 << 20

// ReadPlan returns the plan a task is working from.
//
// It reads the working tree rather than the branch, because that is what the
// agents read: every downstream consumer — the plan review, the exec turn, the
// code review — opens PLAN.md off disk rather than being handed its text. So
// what this shows is exactly what the next turn will act on.
//
// A task with no worktree, or one whose worktree has been removed after its
// pull request opened, has no plan to show. That is not an error.
func (e *Engine) ReadPlan(ctx context.Context, taskID int64) (string, error) {
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if task.WorktreeDir == "" {
		return "", nil
	}
	raw, err := os.ReadFile(filepath.Join(task.WorktreeDir, planFile))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", planFile, err)
	}
	if len(raw) > maxPlanBytes {
		raw = raw[:maxPlanBytes]
	}
	return string(raw), nil
}

// WritePlan replaces the plan a task is working from.
//
// Only while the task is stopped. Not a policy choice: a write landing mid-turn
// races the agent editing the same tree, and the engine's own commit at the end
// of that turn would fold the operator's edit into the agent's, under a message
// claiming the agent did it. Stopped is the one moment the tree is quiescent.
//
// The edit is committed, so it is on the branch, in the diff, and in the pull
// request body — the same visibility every other change to the worktree gets.
//
// It also clears the execution session. A resumed exec turn is prompted with
// the review's findings and never re-reads PLAN.md, running instead on the
// session's memory of it, so without this the edit would be silently ignored by
// exactly the turn it was written for. With the session gone, the next turn is
// fresh and seeded from the file.
func (e *Engine) WritePlan(ctx context.Context, taskID int64, body string) error {
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !task.Stopped() {
		return fmt.Errorf("task %d is not stopped; stop it before editing its plan, "+
			"or the edit races the agent writing the same worktree", taskID)
	}
	if task.WorktreeDir == "" {
		return fmt.Errorf("task %d has no worktree yet, so there is no plan to edit", taskID)
	}
	if len(body) > maxPlanBytes {
		return fmt.Errorf("plan is %d bytes, over the %d-byte limit", len(body), maxPlanBytes)
	}

	// Normalise the line endings a browser textarea produces, so the file does
	// not gain \r\n on every save.
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}

	path := filepath.Join(task.WorktreeDir, planFile)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", planFile, err)
	}

	if _, err := e.WT.Commit(ctx, e.worktreeOf(task), "overseer: plan edited by the operator"); err != nil {
		return fmt.Errorf("commit the edited plan: %w", err)
	}

	// Written narrowly, for the same reason stopped_at is: this runs from an
	// HTTP handler holding a row read before the operator typed anything, and a
	// full-row save would revert whatever else changed meanwhile.
	if err := e.Store.ClearExecSession(ctx, taskID); err != nil {
		return err
	}
	e.notify(taskID)
	return nil
}

// PlanPath is where a task's plan lives, for the CLI to point at.
func PlanPath(task store.Task) string {
	if task.WorktreeDir == "" {
		return ""
	}
	return filepath.Join(task.WorktreeDir, planFile)
}
