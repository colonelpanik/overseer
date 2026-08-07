// Command overseer runs the Claude-plan/Codex-review loop for a list of
// tasks and serves a dashboard for watching them.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"

	"overseer/internal/config"
	"overseer/internal/engine"
	"overseer/internal/store"
	"overseer/internal/web"
	"overseer/internal/worktree"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "overseer: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `overseer — autonomous plan/review loops

Usage:
  overseer serve                 run the daemon and dashboard
  overseer submit <tasks.yaml>   queue a batch of tasks
  overseer status                list tasks and their progress
  overseer logs <task-id>        print a task's transcripts

Flags:
  -config <path>   config file (default ~/.overseer/config.yaml)
`)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("no subcommand")
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	configPath := fs.String("config", filepath.Join(home, ".overseer", "config.yaml"), "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	switch args[0] {
	case "serve":
		return cmdServe(cfg)
	case "submit":
		return cmdSubmit(cfg, fs.Arg(0))
	case "status":
		return cmdStatus(cfg)
	case "logs":
		return cmdLogs(cfg, fs.Arg(0))
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// open wires up the store and engine shared by every subcommand.
func open(cfg config.Config) (*store.Store, *engine.Engine, error) {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, nil, err
	}
	eng, err := engine.New(cfg, st,
		worktree.NewManager(cfg.WorktreesDir()),
		worktree.NewGhOpener(cfg.GhBin))
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return st, eng, nil
}

func cmdServe(cfg config.Config) error {
	st, eng, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := eng.Recover(ctx); err != nil {
		return err
	}

	srv := web.New(cfg, st, eng)
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe(ctx) }()
	go func() { errc <- eng.Run(ctx) }()

	fmt.Printf("overseer listening on http://%s\n", cfg.ListenAddr)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func cmdSubmit(cfg config.Config, path string) error {
	if path == "" {
		return fmt.Errorf("submit needs a task file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	batch, err := engine.ParseBatch(raw)
	if err != nil {
		return err
	}

	st, eng, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	created, err := eng.SubmitBatch(context.Background(), batch)
	for _, t := range created {
		fmt.Printf("queued #%d %s (%s)\n", t.ID, t.Slug, t.RepoPath)
	}
	if err != nil {
		return err
	}
	fmt.Printf("\n%d task(s) queued. Run `overseer serve` if the daemon is not already running.\n", len(created))
	return nil
}

func cmdStatus(cfg config.Config) error {
	st, _, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no tasks")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tPHASE\tITER\tCOST\tSLUG\tNOTE")
	for _, t := range tasks {
		tot, err := st.TaskTotals(ctx, t.ID)
		if err != nil {
			return err
		}
		note := t.PRURL
		if note == "" {
			note = t.ErrMsg
		}
		if len(note) > 60 {
			note = note[:57] + "..."
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%d/%d\t$%.2f\t%s\t%s\n",
			t.ID, t.State, t.Phase, t.Iteration, t.MaxIterations,
			tot.CostUSD, t.Slug, note)
	}
	return w.Flush()
}

func cmdLogs(cfg config.Config, idArg string) error {
	if idArg == "" {
		return fmt.Errorf("logs needs a task id")
	}
	var id int64
	if _, err := fmt.Sscanf(idArg, "%d", &id); err != nil {
		return fmt.Errorf("invalid task id %q", idArg)
	}

	st, _, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	steps, err := st.ListSteps(ctx, id)
	if err != nil {
		return err
	}
	for _, s := range steps {
		fmt.Printf("\n=== %s %s iteration %d (%s) ===\n", s.Agent, s.Phase, s.Iteration, s.State)
		if s.Verdict != "" {
			fmt.Printf("verdict: %s\n", s.Verdict)
		}
		findings, err := st.ListFindings(ctx, s.ID)
		if err != nil {
			return err
		}
		for _, f := range findings {
			loc := f.File
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Printf("  [%s] %s %s\n", f.Severity, loc, f.Summary)
		}
		if s.TranscriptPath != "" {
			fmt.Printf("transcript: %s\n", s.TranscriptPath)
		}
		if s.ErrMsg != "" {
			fmt.Printf("error: %s\n", s.ErrMsg)
		}
	}
	return nil
}
