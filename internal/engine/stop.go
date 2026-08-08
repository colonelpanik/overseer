package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"overseer/internal/loop"
	"overseer/internal/store"
)

// StopKind is what an operator asked a running worker to do when it next
// reaches a safe point: a dispatch boundary, or the return of a dispatch whose
// agent was just killed.
type StopKind string

const (
	// StopPark leaves the task exactly as it is, so starting it again
	// re-dispatches the action its state names.
	StopPark StopKind = "park"
	// StopAbandon ends the task on purpose.
	StopAbandon StopKind = "abandon"
	// StopRestart resets the task's run state and queues it as a fresh attempt.
	StopRestart StopKind = "restart"
)

// stopRequest is one lodged operator request.
//
// It is written before the control's stop channel is closed and never mutated
// afterwards, so the close/receive pair publishes it without a lock of its own.
type stopRequest struct {
	Kind StopKind
	// Msg is recorded on the interrupted step, and as the task's error for an
	// abandon.
	Msg string
	// Hard says to cancel the task's context, which kills the agent's process
	// group through the runner's watchdog, rather than waiting for the current
	// step to finish on its own.
	Hard bool
	// Restart carries the amended task for StopRestart: the operator may have
	// changed the goal or the constraints on the way through.
	Restart store.Task
}

// taskControl is how an operator's request reaches the worker driving a task.
//
// Exactly one exists per claimed task, for as long as a worker owns it. It is
// created by claim and torn down by release, so "e.running[id] != nil" and "a
// worker is driving id" are the same statement — which is why there is one map
// rather than a second registry that could only ever disagree with this one.
type taskControl struct {
	// stop is closed exactly once, when a request is lodged. req is written
	// before the close.
	stop chan struct{}
	req  stopRequest
	// cancel tears down the task's context. Cancelling it is what makes a hard
	// stop hard: the context reaches Runner.Run unchanged, which derives its
	// step-timeout context from it and SIGKILLs the whole process group when it
	// fires.
	//
	// The only two things that cancel this context are requestStopLocked, which
	// always closes stop first, and the daemon shutting down. That is what lets
	// the worker tell a deliberate stop from a shutdown.
	cancel context.CancelFunc
	// closed guards stop against a second close. Read and written under
	// Engine.mu only.
	closed bool
}

// errNotRunning means no worker currently owns the task, so a request has to be
// applied directly rather than lodged.
var errNotRunning = errors.New("no worker is driving this task")

// requestStopLocked lodges req with the worker driving id. Engine.mu must be
// held.
func (e *Engine) requestStopLocked(id int64, req stopRequest) error {
	ctrl := e.running[id]
	if ctrl == nil {
		return errNotRunning
	}
	if ctrl.closed {
		// A second request cannot rewrite req without racing the worker's read,
		// so it is refused — except a hard escalation, which touches nothing but
		// the context and is idempotent. That is the one realistic sequence:
		// "stop it" followed by "actually, stop it now".
		if req.Hard {
			ctrl.cancel()
			return nil
		}
		return fmt.Errorf("task %d is already stopping", id)
	}
	ctrl.req = req
	ctrl.closed = true
	close(ctrl.stop)
	if req.Hard {
		// Called under the lock deliberately: it touches only the context tree
		// and never re-enters the engine, and doing it here removes the window
		// in which release could run between the close and the cancel.
		ctrl.cancel()
	}
	return nil
}

// stopRequested reports the operator's pending request, if any.
//
// req was written before ctrl.stop was closed, so the receive publishes it.
func stopRequested(ctrl *taskControl) (stopRequest, bool) {
	if ctrl == nil {
		return stopRequest{}, false
	}
	select {
	case <-ctrl.stop:
		return ctrl.req, true
	default:
		return stopRequest{}, false
	}
}

// applyStop lodges req with the worker driving taskID, or applies its effect
// directly when no worker owns it.
//
// Everything happens under e.mu, including the direct store write. Releasing
// the lock between the ownership test and the write is precisely the window the
// old Abandon guard left open: a scheduler poll can claim the task in it, and
// the worker's first SaveTask then overwrites what was just written with
// whatever its own loop.Next computed — silently reverting the operator a
// moment later.
//
// When a worker does own the task, the worker performs the write, from the copy
// it is actually holding. That is what closes the lost-update race for good:
// there is only ever one writer of a task's state, and it is whoever owns it.
func (e *Engine) applyStop(ctx context.Context, taskID int64, req stopRequest) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	err := e.requestStopLocked(taskID, req)
	if !errors.Is(err, errNotRunning) {
		return err
	}

	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	return e.applyStopDirect(ctx, &task, req)
}

// applyStopDirect performs a request's effect on a task nobody is driving.
func (e *Engine) applyStopDirect(ctx context.Context, task *store.Task, req stopRequest) error {
	switch req.Kind {
	case StopPark:
		// Deliberately no write to the tasks table. The state already names the
		// action that was in flight and loop.Pending re-dispatches it when the
		// task is started again — the same path a restarted daemon takes.
		// stopped_at was written by Stop, in its own narrow statement, and must
		// not be rewritten from this copy.

	case StopAbandon:
		task.State = string(loop.StateAbandoned)
		task.ErrMsg = req.Msg
		if err := e.Store.SaveTaskClearingStop(ctx, *task); err != nil {
			return err
		}

	case StopRestart:
		if err := e.Store.RestartTask(ctx, req.Restart); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown stop kind %q", req.Kind)
	}
	e.notify(task.ID)
	return nil
}

// StopAllRunning stops every task a worker is currently driving, killing their
// agents rather than waiting for each to finish its turn.
//
// The scheduler is stopped separately, by the caller, and that alone is enough
// for tasks not yet claimed. This is for the ones already mid-step, where
// waiting out a step timeout each is the thing being avoided.
func (e *Engine) StopAllRunning(ctx context.Context, reason string) error {
	e.mu.Lock()
	ids := make([]int64, 0, len(e.running))
	for id := range e.running {
		ids = append(ids, id)
	}
	e.mu.Unlock()

	var failed []string
	for _, id := range ids {
		// Persisted first, for the same reason Stop does it: a daemon that dies
		// in the window leaves the task parked rather than running.
		if err := e.Store.StopTask(ctx, id, true); err != nil {
			failed = append(failed, fmt.Sprintf("task %d: %v", id, err))
			continue
		}
		err := e.applyStop(ctx, id, stopRequest{Kind: StopPark, Msg: reason, Hard: true})
		// Already stopping is the expected answer when the operator pressed
		// this twice, and is not worth reporting as a failure.
		if err != nil && !strings.Contains(err.Error(), "already stopping") {
			failed = append(failed, fmt.Sprintf("task %d: %v", id, err))
		}
		e.notify(id)
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// RestoreStopAll re-applies a persisted global stop at startup, so a restart
// does not quietly resume everything the operator stopped.
func (e *Engine) RestoreStopAll(ctx context.Context) error {
	reason, err := e.Store.Setting(ctx, store.SettingStopAll)
	if err != nil {
		return err
	}
	if reason != "" {
		e.Pause(reason)
	}
	return nil
}

// parkStopped applies the operator's request to a task whose worker has just
// come off a dispatch.
//
// Every write here uses a context the stop did not cancel. The whole point of
// the request is that it must be recorded, and a hard stop cancels the very
// context the worker was running under — the same reason a background analysis
// detaches from its request context.
func (e *Engine) parkStopped(ctx context.Context, task *store.Task, req stopRequest) error {
	ctx = context.WithoutCancel(ctx)

	// A hard stop SIGKILLed the agent mid-step, so its row is still "running".
	// Marked interrupted rather than failed: the step did not fail, it was
	// taken away, and unlike a crash there is no restart coming to sweep it.
	if _, err := e.Store.InterruptTaskSteps(ctx, task.ID, req.Msg); err != nil {
		e.logf("task %d: close stopped step: %v", task.ID, err)
	}
	e.commitInterrupted(ctx, task)
	return e.applyStopDirect(ctx, task, req)
}

// commitInterrupted commits whatever a killed agent had written.
//
// The engine commits after each *completed* turn, so a turn killed partway
// through is the only thing that ever leaves the worktree dirty. Left alone,
// those edits sit uncommitted: invisible in the Diff tab, which shows committed
// work only, and waiting to be swept into the next turn's commit along with
// whatever the operator did in between — including an edit to PLAN.md, which is
// the main reason to stop a task in the first place.
//
// So it gets its own commit, under its own message. Deliberately not the normal
// "overseer: <phase> iteration N", which would claim an iteration completed
// when it did not.
//
// Best effort. A task that cannot be committed is still stopped; reporting a
// commit failure as a failure to stop would leave the operator unable to stop
// it at all.
func (e *Engine) commitInterrupted(ctx context.Context, task *store.Task) {
	if task.WorktreeDir == "" {
		return
	}
	msg := fmt.Sprintf("overseer: interrupted during %s iteration %d",
		orDash(task.Phase), task.Iteration)
	committed, err := e.WT.Commit(ctx, e.worktreeOf(*task), msg)
	if err != nil {
		e.logf("task %d: commit interrupted work: %v", task.ID, err)
		return
	}
	if committed {
		e.notify(task.ID)
	}
}

func orDash(s string) string {
	if s == "" {
		return "setup"
	}
	return s
}
