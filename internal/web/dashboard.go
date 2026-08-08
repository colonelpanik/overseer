package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"overseer/internal/agent"
	"overseer/internal/engine"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// liveTailBytes bounds how much of a transcript the live pane reads. A step
// that prints continuously must not be able to make the dashboard the thing
// that runs the daemon out of memory.
const liveTailBytes = 96 * 1024

// liveTailLines is how many summarised events the live pane shows.
const liveTailLines = 60

// build assembles the whole dashboard for one request.
func (s *Server) build(ctx context.Context, q Query) (*Dashboard, error) {
	tasks, err := s.store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	totals, err := s.store.AllTotals(ctx)
	if err != nil {
		return nil, err
	}
	deps, err := s.store.AllTaskDeps(ctx)
	if err != nil {
		return nil, err
	}
	reviews, err := s.store.AllReviewRounds(ctx)
	if err != nil {
		return nil, err
	}

	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	repoByID := make(map[int64]store.Repo, len(repos))
	for _, r := range repos {
		repoByID[r.ID] = r
	}

	byID := make(map[int64]store.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}

	// ListTasks is newest first, which is the right order for a flat list but
	// the wrong one for group headers, where a repo's tasks should read in the
	// order they were queued.
	all := make([]taskFacts, 0, len(tasks))
	for _, t := range tasks {
		f := taskFacts{Task: t, Totals: totals[t.ID], Rounds: roundsOf(reviews[t.ID])}
		// The group header names the registered repository, not the path's
		// basename. Grouping by basename silently merged /a/widget with
		// /b/vendor/widget into one heading whose totals belonged to neither.
		if r, ok := repoByID[t.RepoID]; ok {
			f.RepoSlug = r.Slug
		} else {
			f.RepoSlug = repoName(t.RepoPath)
		}
		for _, id := range deps[t.ID] {
			dep, ok := byID[id]
			if !ok || dep.State == "done" {
				continue
			}
			f.UnmetDeps = append(f.UnmetDeps, dep)
		}
		// The gate only applies while queued; past that the task is in flight
		// and its dependencies no longer hold it up. This mirrors the SQL in
		// ClaimableTasks, and the two must agree or the board will label a
		// task blocked while a worker is driving it.
		f.Blocked = t.State == "queued" && len(f.UnmetDeps) > 0
		all = append(all, f)
	}

	d := &Dashboard{
		Q:           q,
		SandboxNote: s.eng.SandboxNote,
		PauseReason: s.eng.PauseReason(),
		Filters:     buildFilters(all, q),
		GroupURL:    q.URL("group", !q.Group),
		CLIText:     cliText(all, s.cfg.MaxParallel, s.eng.SandboxNote, s.cfg.RunCapUSD),
	}

	running := 0
	for _, f := range all {
		if TaskTone(f.Task) == ToneLive {
			running++
		}
	}
	// The two figures are kept apart, here and everywhere else. The default
	// claude and codex providers run on the operator's subscription, so what
	// those CLIs report is what the usage would have cost through the API — a
	// usage signal, not a bill. Only a provider configured with its own
	// endpoint is money. Presenting one total would be the dashboard lying,
	// which is the one thing the rest of this project is careful not to do.
	reported, metered, err := s.eng.RepoSpend(ctx)
	if err != nil {
		return nil, err
	}
	d.RunSummary = fmt.Sprintf("%s · %d running · max_parallel %d",
		plural(len(all), "task"), running, s.cfg.MaxParallel)
	d.Spend = money(reported)
	d.Metered = money(metered)
	d.HasMetered = metered > 0
	// The cap is stated against reported usage, since that is what the
	// subscription-driven default actually accumulates.
	if s.cfg.RunCapUSD > 0 {
		d.RunCap = "/ " + money(s.cfg.RunCapUSD) + " run cap"
		d.OverRunCap = reported+metered > s.cfg.RunCapUSD
	}
	stoppedAll, err := s.store.Setting(ctx, store.SettingStopAll)
	if err != nil {
		return nil, err
	}
	d.StoppedAll = stoppedAll != ""

	d.Budget = budgetAlert(all, q)
	d.Toast = parkedToast(all, q)
	d.Add = s.addForm(all, repos, q)

	var visible []taskFacts
	for _, f := range all {
		if q.Repo != 0 && f.Task.RepoID != q.Repo {
			continue
		}
		if keep(f.Task, q.Filter) && matches(f.Task, q.Search) {
			visible = append(visible, f)
		}
	}
	d.Rows = buildRows(visible, q)
	d.Empty = len(visible) == 0
	d.ListCount = fmt.Sprintf("%d shown", len(visible))
	d.BulkCount = len(q.Bulk)

	if q.Sel != 0 {
		for _, f := range all {
			if f.Task.ID != q.Sel {
				continue
			}
			sel, err := s.buildSelection(ctx, f, q)
			if err != nil {
				return nil, err
			}
			d.Sel = sel
			break
		}
	}

	chip, analyses, err := s.buildAnalyses(ctx, q)
	if err != nil {
		return nil, err
	}
	d.Running = chip
	if q.Overlay == "analyses" {
		d.Analyses = analyses
		d.ShowingAll = true
	}

	if q.Overlay == "settings" {
		settings := s.buildSettings(q)
		settings.Saved = q.Saved
		d.Settings = &settings
	}

	if q.Overlay == "repos" {
		if d.Repos, err = s.buildRepos(ctx, q); err != nil {
			return nil, err
		}
	}
	if q.Overlay == "backlog" {
		if d.Backlog, err = s.buildBacklog(ctx, q); err != nil {
			return nil, err
		}
	}
	// The chip is on the nav whatever overlay is open: a repository filter you
	// cannot see is a board that looks empty for no stated reason.
	d.RepoChip = repoChip(repoByID, q)
	d.OpenBacklog, err = s.openBacklogTotal(ctx)
	if err != nil {
		return nil, err
	}

	if q.Wizard != 0 {
		wiz, err := s.buildWizard(ctx, q)
		if err != nil {
			return nil, err
		}
		d.Wizard = wiz
	}
	return d, nil
}

// buildWizard loads the open proposal. A wizard id that no longer resolves —
// a proposal queued in another tab, or a stale bookmark — closes the overlay
// rather than failing the whole page.
func (s *Server) buildWizard(ctx context.Context, q Query) (*WizardView, error) {
	if q.Wizard < 0 {
		repos, err := s.repoChoices(ctx, q)
		if err != nil {
			return nil, err
		}
		// Two doors into the same wizard: design something, or analyse a
		// repository. Both start on the source screen; the kind decides which
		// form that screen is.
		return &WizardView{
			ID:        WizardNew,
			Designing: q.Wizard == WizardDesign,
			Step:      StepSource,
			Model:     s.analyseModel(),
			MaxTasks:  12,
			Focus:     append([]FocusChoice(nil), FocusAreas...),
			Models:    s.analyseModels(""),
			Repos:     repos,
			ReposURL:  q.URL("wizard", 0, "overlay", "repos"),
			CloseURL:  q.URL("wizard", 0),
		}, nil
	}

	p, err := s.store.GetProposal(ctx, q.Wizard)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.State == store.ProposalQueued || p.State == store.ProposalDiscarded {
		return nil, nil
	}

	rows, err := s.store.ProposalTasks(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	var live *LivePane
	if p.TranscriptPath != "" &&
		(p.State == store.ProposalAnalysing || p.State == store.ProposalScaffolding) {
		live = transcriptPane(p.TranscriptPath, "agent", true)
	}
	w := buildWizard(p, rows, live, q)
	w.Models = s.analyseModels(p.Model)
	if p.State == store.ProposalDesigning || p.Design != "" {
		w.Designing = true
		if w.Design, err = s.buildDesign(ctx, p); err != nil {
			return nil, err
		}
		// The header's figure has to include the conversation, or it reads
		// $0.00 next to a footer saying the conversation has cost real money.
		spend, err := s.store.ArchitectSpend(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		w.Spend = money(p.CostUSD + spend)
	}
	return w, nil
}

// transcriptPane renders a running transcript for the live view. Both the
// task timeline and the wizard show one, and they must read identically.
func transcriptPane(path, head string, running bool) *LivePane {
	raw, err := tailFile(path, liveTailBytes)
	if err != nil {
		return &LivePane{Empty: "Nothing has been written yet."}
	}
	lines := agent.SummariseTranscript(raw, liveTailLines)
	p := &LivePane{
		Head:  head,
		Live:  running,
		Meta:  plural(len(lines), "event"),
		Lines: lines,
	}
	if len(lines) == 0 {
		p.Empty = "Nothing has been written yet."
	}
	return p
}

// buildSelection loads the selected task's steps and whichever right-hand
// pane is showing. The diff and the live tail are read only when their tab is
// open: both touch the filesystem, and paying for them on every reload of a
// page showing neither would be a shell-out per state event.
func (s *Server) buildSelection(ctx context.Context, f taskFacts, q Query) (*Detail, error) {
	steps, err := s.store.ListSteps(ctx, f.Task.ID)
	if err != nil {
		return nil, err
	}
	byStep, err := s.store.AllFindings(ctx, f.Task.ID)
	if err != nil {
		return nil, err
	}

	d := buildDetail(f, steps, byStep, s.eng.TakeOverHint(f.Task), s.eng.SandboxNote, q)

	var files []worktree.FileDiff
	var diffErr error
	var live *LivePane
	var plan *PlanPane
	switch q.Tab {
	case TabDiff:
		files, diffErr = s.diff(ctx, f.Task)
	case TabLive:
		live = livePane(f.Task, steps)
	case TabPlan:
		plan = s.planPane(ctx, f.Task)
	}
	d.Right = buildRight(f, steps, byStep, files, diffErr, live, plan, q)
	return d, nil
}

// planPane reads the plan the next turn will act on.
//
// Read from the worktree rather than the branch, because that is where every
// agent reads it from — so this shows what will actually happen, not what was
// last committed.
func (s *Server) planPane(ctx context.Context, task store.Task) *PlanPane {
	p := &PlanPane{
		Editable: task.Stopped(),
		Path:     engine.PlanPath(task),
	}
	switch {
	case task.WorktreeDir == "":
		p.Empty = "No plan yet. One is written in the first planning turn, once this task has a worktree."
		return p
	case task.PRURL != "":
		p.Empty = "This task's worktree was removed when its pull request opened. The plan is in the branch and in the pull request body."
		return p
	}

	body, err := s.eng.ReadPlan(ctx, task.ID)
	if err != nil {
		p.Empty = err.Error()
		return p
	}
	p.Body = body
	if body == "" {
		p.Empty = "The planning turn has not written PLAN.md yet."
	}

	if p.Editable {
		p.Why = "Edits are committed to the branch, and the next turn starts fresh from this file " +
			"rather than resuming the session that wrote it — so what you write here is what gets built."
	} else {
		p.Why = "Stop the task to edit this. A write while an agent is working the same tree would " +
			"race it, and would be swept into that turn's commit as if the agent had made it."
	}
	return p
}

// diff reads the task's accumulated change against its base ref.
func (s *Server) diff(ctx context.Context, t store.Task) ([]worktree.FileDiff, error) {
	if t.WorktreeDir == "" {
		return nil, nil
	}
	wt := worktree.Worktree{
		RepoPath:  t.RepoPath,
		Dir:       t.WorktreeDir,
		Branch:    t.Branch,
		BaseRef:   t.BaseRef,
		CommonDir: t.GitCommonDir,
		AdminDir:  t.GitAdminDir,
	}
	if wt.BaseRef == "" {
		return nil, nil
	}
	return s.eng.WT.ParsedDiff(ctx, wt)
}

// livePane summarises the transcript of whatever is running now, falling back
// to the most recent step that left one behind.
func livePane(t store.Task, steps []store.Step) *LivePane {
	var chosen *store.Step
	for i := range steps {
		s := &steps[i]
		if s.TranscriptPath == "" {
			continue
		}
		if s.State == "running" {
			chosen = s
			break
		}
		chosen = s
	}
	if chosen == nil {
		return &LivePane{Empty: "No agent has written a transcript for this task yet."}
	}

	raw, err := tailFile(chosen.TranscriptPath, liveTailBytes)
	if err != nil {
		return &LivePane{Empty: "The transcript for this step is no longer on disk."}
	}
	lines := agent.SummariseTranscript(raw, liveTailLines)
	p := &LivePane{
		Head: fmt.Sprintf("%s · %s i%d", chosen.Agent, chosen.Phase, chosen.Iteration),
		Live: chosen.State == "running",
		Meta: fmt.Sprintf("%s · %d tokens · %s",
			plural(len(lines), "event"),
			chosen.InputTokens+chosen.OutputTokens, money(chosen.CostUSD)),
		Lines: lines,
	}
	if !p.Live {
		p.Head = "last step · " + p.Head
	}
	if len(lines) == 0 {
		p.Empty = "The step has not emitted anything yet."
	}
	return p
}

// tailFile reads at most the last n bytes of a file, dropping the first
// partial line so the summariser never sees half an event.
func tailFile(path string, n int64) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	info, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= n {
		return io.ReadAll(fh)
	}
	if _, err := fh.Seek(info.Size()-n, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(fh)
	if err != nil {
		return nil, err
	}
	if i := strings.IndexByte(string(raw), '\n'); i >= 0 {
		raw = raw[i+1:]
	}
	return raw, nil
}

// budgetAlert finds the first task past its advisory cap.
func budgetAlert(all []taskFacts, q Query) *BudgetAlert {
	for _, f := range all {
		if !f.OverCap() {
			continue
		}
		next := nextCap(f.Totals.CostUSD, f.Task.CostCapUSD)
		return &BudgetAlert{
			TaskID: f.Task.ID,
			Message: fmt.Sprintf("%s has spent %s against its %s cap. It keeps running: overseer will not kill an agent mid-edit.",
				f.Task.Slug, money(f.Totals.CostUSD), money(f.Task.CostCapUSD)),
			NewCap: fmt.Sprintf("%.2f", next),
			URL:    q.URL("sel", f.Task.ID, "tab", TabFindings),
		}
	}
	return nil
}

// parkedToast points at a task that needs the operator while they are looking
// at something else. It is deliberately not time-based: a nudge that expires
// is a nudge that gets missed on a run left alone for an hour.
func parkedToast(all []taskFacts, q Query) *Toast {
	if q.NoToast || q.Filter == FilterAttention {
		return nil
	}
	var pick *taskFacts
	for i := range all {
		f := &all[i]
		if f.Task.State != "escalated" || f.Task.ID == q.Sel {
			continue
		}
		if pick == nil || f.Task.UpdatedAt.After(pick.Task.UpdatedAt) {
			pick = f
		}
	}
	if pick == nil {
		return nil
	}
	body := pick.Task.ErrMsg
	if body == "" {
		body = fmt.Sprintf("parked at %s without converging", Progress(pick.Task))
	}
	return &Toast{
		Message:  pick.Task.Slug + " — " + body,
		OpenURL:  q.URL("sel", pick.Task.ID, "tab", TabFindings),
		CloseURL: q.URL("toast", false),
	}
}

// addForm offers the registered repositories and the dependencies already in
// play, so queueing a second task against the same repo is a click rather than
// a path typed out again.
func (s *Server) addForm(all []taskFacts, repos []store.Repo, q Query) AddForm {
	choices := make([]RepoChoice, 0, len(repos))
	for _, r := range repos {
		if r.Archived() {
			continue
		}
		choices = append(choices, RepoChoice{
			ID: r.ID, Slug: r.Slug, Path: r.Path, On: r.ID == q.Repo,
		})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Slug < choices[j].Slug })

	var deps []DepChoice
	for _, f := range all {
		if f.Task.State != "done" && f.Task.State != "failed" {
			deps = append(deps, DepChoice{
				ID: f.Task.ID, Slug: f.Task.Slug, State: f.Task.State,
			})
		}
	}
	if len(deps) > 12 {
		deps = deps[:12]
	}
	return AddForm{
		Repos:      choices,
		Deps:       deps,
		Severities: []string{"any", "minor", "major", "critical"},
		Severity:   s.cfg.BlockingSeverity,
		Verify:     s.cfg.VerifyCommand,
		Cap:        fmt.Sprintf("%.2f", s.cfg.TaskCapUSD),
		Templates: []Template{
			{Label: "blank", Goal: ""},
			{Label: "dependency bump", Goal: "Update the project's dependencies to their current releases and fix whatever the upgrade breaks."},
			{Label: "add a test", Goal: "Add a regression test that fails without the fix and passes with it."},
			{Label: "a11y sweep", Goal: "Fix keyboard traps and colour-contrast failures across the templates."},
		},
	}
}

// cliText renders the `overseer status` table the CLI overlay shows. It is
// built from the same facts as the board so the two cannot disagree.
func cliText(all []taskFacts, maxParallel int, sandbox string, runCap float64) string {
	var sb strings.Builder
	sb.WriteString("$ overseer status\n\n")
	if len(all) == 0 {
		sb.WriteString("no tasks\n")
		return sb.String()
	}

	w := tabwriter.NewWriter(&sb, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSLUG\tREPO\tSTATE\tPHASE\tITER\tCOST\tNOTE")
	var spend float64
	counts := map[string]int{}
	for _, f := range all {
		t := f.Task
		spend += f.Totals.CostUSD
		counts[Tone(t.State)]++
		counts[t.State]++
		note := t.PRURL
		if note == "" {
			note = t.ErrMsg
		}
		if len(note) > 48 {
			note = note[:45] + "..."
		}
		phase := t.Phase
		if phase == "" {
			phase = "—"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\n",
			t.ID, t.Slug, repoName(t.RepoPath), stateLabel(t, f.Blocked), phase,
			t.Iteration, t.MaxIterations, money(f.Totals.CostUSD), note)
	}
	w.Flush()

	fmt.Fprintf(&sb, "\n%s · %d running · %d escalated · %d failed · %d done\n",
		plural(len(all), "task"), counts[ToneLive],
		counts["escalated"], counts["failed"], counts["done"])
	fmt.Fprintf(&sb, "spend %s", money(spend))
	if runCap > 0 {
		fmt.Fprintf(&sb, " of %s run cap", money(runCap))
	}
	fmt.Fprintf(&sb, " · max_parallel %d · %s\n", maxParallel, sandbox)
	return sb.String()
}

// writeError renders a store or engine failure without leaking a stack.
func writeError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
