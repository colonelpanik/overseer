package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"overseer/internal/config"
	"overseer/internal/store"
)

// cmdRepos prints the same table the dashboard's Repos overlay shows.
//
// The two spend columns are deliberately not one. The default claude and codex
// providers run against the operator's subscription through the CLI's own
// login, so what those CLIs report is what the usage would have cost through
// the API — a usage signal, not a bill. Only a provider configured with its own
// endpoint is metered to the operator.
func cmdRepos(cfg config.Config) error {
	st, eng, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	repos, err := st.ListRepos(ctx)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		fmt.Println("no repositories yet — one registers itself the first time you submit or analyse against it")
		return nil
	}
	stats, err := eng.RepoStats(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "SLUG\tTASKS\tBACKLOG\tTIME\tTURNS\tREPORTED\tMETERED\tPATH")
	var reported, metered float64
	var agentTime time.Duration
	for _, r := range repos {
		s := stats[r.ID]
		reported += s.Reported
		metered += s.Metered
		agentTime += s.AgentTime

		slug := r.Slug
		if r.Archived() {
			slug += " (archived)"
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%d\t$%.2f\t$%.2f\t%s\n",
			slug, s.Tasks, s.Backlog, cliDuration(s.AgentTime), s.Turns,
			s.Reported, s.Metered, r.Path)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\n%d repositories · %s agent time\n", len(repos), cliDuration(agentTime))
	fmt.Printf("$%.2f reported (subscription-covered CLI usage, priced as if it had gone through the API)\n", reported)
	fmt.Printf("$%.2f metered (providers you configured with your own endpoint — actual spend)\n", metered)
	return nil
}

// cmdBacklog prints a repository's todo list, or every repository's.
func cmdBacklog(cfg config.Config, ref string) error {
	st, _, err := open(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	repos, err := st.ListRepos(ctx)
	if err != nil {
		return err
	}
	if ref = strings.TrimSpace(ref); ref != "" {
		one, err := st.RepoBySlug(ctx, ref)
		if err != nil {
			// A path is as good a name as a slug, and is what somebody with a
			// task file in front of them has to hand.
			one, err = st.RepoByPath(ctx, ref)
			if err != nil {
				return fmt.Errorf("no repository named %q", ref)
			}
		}
		repos = []store.Repo{one}
	}
	if len(repos) == 0 {
		fmt.Println("no repositories yet")
		return nil
	}

	total := 0
	for _, r := range repos {
		items, err := st.ListBacklog(ctx, r.ID)
		if err != nil {
			return err
		}
		// Not named `open`: that is the package's store/engine constructor.
		openCount := 0
		for _, item := range items {
			if item.State == store.BacklogOpen {
				openCount++
			}
		}
		if openCount == 0 && ref == "" {
			continue
		}
		total += openCount

		fmt.Printf("\n%s — %d open of %d\n", r.Slug, openCount, len(items))
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "  ID\tSEV\tSEEN\tSOURCE\tTITLE\tEVIDENCE")
		for _, item := range items {
			if item.State != store.BacklogOpen && ref == "" {
				continue
			}
			title := item.Title
			if len(title) > 64 {
				title = title[:61] + "..."
			}
			sev := item.Severity
			if sev == "" {
				sev = "—"
			}
			state := ""
			if item.State != store.BacklogOpen {
				state = " [" + item.State + "]"
			}
			fmt.Fprintf(w, "  %d\t%s\t%d\t%s\t%s%s\t%s\n",
				item.ID, sev, item.Seen, item.Source, title, state,
				strings.Join(item.Evidence, ", "))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if total == 0 && ref == "" {
		fmt.Println("nothing on any repository's backlog")
	}
	return nil
}

// cliDuration renders agent time the way the dashboard does.
func cliDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
