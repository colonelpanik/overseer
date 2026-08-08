package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"overseer/internal/config"
	"overseer/internal/engine"
)

// These run against the database directly rather than talking to a running
// daemon, exactly as `submit` does. A stop written here is picked up by the
// daemon's next poll — the claim query reads stopped_at — so it works whether
// or not a daemon is running.
//
// The one thing it cannot do from outside the daemon is kill an agent: the
// control that reaches a worker lives in that worker's process. -now against a
// separate daemon parks the task and says so rather than pretending.

func cmdStop(cfg config.Config, idArg string, now bool) error {
	id, err := taskID("stop", idArg)
	if err != nil {
		return err
	}
	st, eng, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	if err := eng.Stop(ctx, id, engine.StopOpts{Now: now, Reason: "stopped from the command line"}); err != nil {
		return err
	}
	task, err := st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("stopped #%d %s (was %s)\n", task.ID, task.Slug, task.State)
	if now {
		fmt.Println("note: an agent running inside a separate daemon keeps going until that daemon " +
			"reaches its next boundary — the kill signal only reaches workers in this process.")
	}
	fmt.Printf("start it again with: overseer start %d\n", task.ID)
	return nil
}

func cmdStart(cfg config.Config, idArg string) error {
	id, err := taskID("start", idArg)
	if err != nil {
		return err
	}
	st, eng, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	if err := eng.Start(ctx, id); err != nil {
		return err
	}
	task, err := st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("started #%d %s; it resumes at %s\n", task.ID, task.Slug, task.State)
	return nil
}

func cmdRestart(cfg config.Config, idArg string, now bool) error {
	id, err := taskID("restart", idArg)
	if err != nil {
		return err
	}
	st, eng, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	if err := eng.Restart(ctx, id, engine.RestartOpts{
		StopOpts: engine.StopOpts{Now: now, Reason: "restarted from the command line"},
	}); err != nil {
		return err
	}
	task, err := st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("restarted #%d %s as attempt %d, on branch overseer/%s\n",
		task.ID, task.Slug, task.RunSeq, task.RunSlug())
	fmt.Println("the previous attempt's branch and worktree are kept.")
	return nil
}

func cmdPlan(cfg config.Config, idArg string) error {
	id, err := taskID("plan", idArg)
	if err != nil {
		return err
	}
	st, eng, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	body, err := eng.ReadPlan(ctx, id)
	if err != nil {
		return err
	}
	task, err := st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		fmt.Println("no plan yet; one is written in the first planning turn")
		return nil
	}
	fmt.Print(body)
	if !strings.HasSuffix(body, "\n") {
		fmt.Println()
	}
	// Pointed at rather than edited here: editing needs the task stopped, and
	// an editor in a subprocess is a worse tool than the one already installed.
	if path := engine.PlanPath(task); path != "" {
		fmt.Fprintf(os.Stderr, "\n%s\nstop the task first to edit it: overseer stop %d\n", path, task.ID)
	}
	return nil
}

func taskID(verb, arg string) (int64, error) {
	if strings.TrimSpace(arg) == "" {
		return 0, fmt.Errorf("%s needs a task id", verb)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a task id", arg)
	}
	return id, nil
}
