package web

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"overseer/internal/loop"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// Tone is the visual weight a task carries on the board. The Modernist palette
// is one accent on ink, so a task is not given a colour per state — it is
// either quiet, moving, or asking for a human.
const (
	ToneMuted = "muted" // queued, blocked, done: nothing to do here
	ToneLive  = "live"  // a worker is driving it
	ToneAlert = "alert" // escalated or failed: it wants the operator
)

// Tone maps a stored state to its board weight.
func Tone(state string) string {
	switch state {
	case "escalated", "failed":
		return ToneAlert
	// abandoned is muted, not alert: an operator ending a task is not the
	// board asking for their attention. That is the whole reason it is not
	// "failed".
	case "queued", "done", "abandoned":
		return ToneMuted
	default:
		return ToneLive
	}
}

// TaskTone is Tone with the stop taken into account. A stopped task is resting,
// however mid-flight the state it is resting in.
func TaskTone(t store.Task) string {
	if t.Stopped() {
		return ToneMuted
	}
	return Tone(t.State)
}

// Progress renders the phase and iteration counter, which is the fastest
// signal on the board for whether a task is converging or ping-ponging.
func Progress(t store.Task) string {
	if t.Phase == "" {
		return t.State
	}
	return fmt.Sprintf("%s %d/%d", t.Phase, t.Iteration, t.MaxIterations)
}

// stateLabel collapses the engine's states into the words the board uses. The
// loop distinguishes plan_review from code_review because it dispatches
// differently; an operator reading the board only needs to know a review is
// happening.
func stateLabel(t store.Task, blocked bool) string {
	// Stopped wins over whatever the task was doing. The state column keeps
	// naming the action in flight — that is what starting it again
	// re-dispatches — but to an operator the task is stopped, not planning.
	if t.Stopped() {
		return "stopped"
	}
	switch t.State {
	case "queued":
		if blocked {
			return "blocked"
		}
		return "queued"
	case "worktree":
		return "starting"
	case "plan_review", "code_review":
		return "reviewing"
	default:
		return t.State
	}
}

// Chip is a filter or tab button.
type Chip struct {
	Label string
	Count int
	On    bool
	URL   string
}

// Tag is a small label in the detail header.
type Tag struct {
	Label string
	Kind  string // neutral | outline | accent
}

// Field is one hidden input on an action form.
type Field struct{ Name, Value string }

// Action is a button in the detail header or a banner. Href renders an anchor;
// Post renders a form, which is what every state change goes through so the
// same-origin check applies.
type Action struct {
	Label  string
	Kind   string // primary | secondary
	Href   string
	Post   string
	Fields []Field
}

// Row is one line of the task list: either a repo group header or a task.
type Row struct {
	Header bool
	Label  string
	Sub    string

	ID       int64
	Selected bool
	State    string
	Tone     string
	Progress string
	Goal     string
	Repo     string
	Meta     string
	Bars     []Bar
	Note     string
	// PR is the pull request URL as text, not a link: the whole row is one
	// anchor, and an anchor inside an anchor is not a thing. The clickable
	// version is the primary action in the detail header.
	PR       string
	URL      string
	Bulkable bool
	Checked  bool
	BulkURL  string
}

// BudgetAlert is the advisory over-cap banner.
type BudgetAlert struct {
	TaskID  int64
	Message string
	NewCap  string
	URL     string
}

// Toast nudges the operator towards a task that has parked while they were
// looking elsewhere.
type Toast struct {
	Message  string
	OpenURL  string
	CloseURL string
}

// Banner is the full-width explanation above a parked or failed task.
type Banner struct {
	Title    string
	Body     string
	Actions  []Action
	TakeOver string
}

// FindingRow is one finding inside a timeline step.
type FindingRow struct {
	Severity string
	Summary  string
	Loc      string
	Detail   string
	Open     bool
	Blocking bool
}

// StepCard is one agent turn in the timeline. Col is 1 for Claude and 2 for
// the reviewers, which is what makes the two lanes read as a conversation.
type StepCard struct {
	Col           int
	Agent         string
	Phase         string
	Duration      string
	Cost          string
	Title         string
	Verdict       string
	VerdictOK     bool
	Live          bool
	Open          bool
	ToggleURL     string
	Findings      []FindingRow
	TranscriptURL string
	Err           string
}

// DiffTab is one file button above the diff.
type DiffTab struct {
	Name string
	Stat string
	On   bool
	URL  string
}

// Anchor is a finding pinned to the diff line it was raised against.
type Anchor struct {
	Severity string
	Round    string
	Text     string
}

// DiffRow is one line of the rendered diff.
type DiffRow struct {
	Kind   string
	A      string
	B      string
	Text   string
	Anchor *Anchor
}

// DiffPane is the diff tab.
type DiffPane struct {
	Files     []DiffTab
	Lines     []DiffRow
	Err       string
	Empty     string
	Truncated bool
}

// LivePane is the live-output tab.
type LivePane struct {
	Head  string
	Meta  string
	Lines []string
	Live  bool
	Empty string
}

// Right is the right-hand pane in all three of its modes.
type Right struct {
	Tab    string
	Title  string
	Sub    string
	Tabs   []Chip
	Diff   *DiffPane
	Ledger []LedgerRow
	Live   *LivePane
	Plan   *PlanPane
	Intro  string
}

// PlanPane is the plan tab.
type PlanPane struct {
	Body string
	// Editable is true only while the task is stopped. A write landing mid-turn
	// races the agent editing the same worktree, so the form is not offered at
	// all rather than offered and refused.
	Editable bool
	// Why explains what the operator can do from here, and why not more.
	Why   string
	Empty string
	// Path is where the file lives, for an operator who would rather use their
	// own editor.
	Path string
}

// Detail is the selected task.
type Detail struct {
	ID           int64
	Slug         string
	State        string
	Tone         string
	Progress     string
	Branch       string
	Spend        string
	Goal         string
	Chips        []Tag
	Actions      []Action
	Banner       *Banner
	StepCount    string
	Converge     []Bar
	ConvergeNote string
	FP           *Matrix
	Steps        []StepCard
	Right        Right
}

// DepChoice is one existing task offered as a dependency in the add dialog.
type DepChoice struct {
	ID    int64
	Slug  string
	State string
}

// Template is a starting point for a new task's goal.
type Template struct {
	Label string
	Goal  string
}

// AddForm is the queue-a-task dialog.
type AddForm struct {
	// Repos are the registered repositories, offered as a dropdown. A path not
	// listed yet is still accepted — registration is a side effect of use, not
	// a gate in front of it.
	Repos      []RepoChoice
	Deps       []DepChoice
	Severities []string
	Severity   string
	Verify     string
	Cap        string
	Templates  []Template
}

// Wizard step numbers, which are also the order they happen in.
const (
	StepSource = 1
	// StepDesign is the architect conversation. It shares a number with Focus
	// because they are the same moment in the two flows: the step where the
	// operator says what they want before anything is paid for.
	StepDesign = 2
	StepFocus  = 2
	StepRun    = 3
	StepReview = 4
)

// WizardDesign is the wizard id for "opened to design something, but nothing
// created yet". Like WizardNew, a negative id that cannot collide with a real
// one — the first screen creates no row, because an operator who changes their
// mind should not leave a proposal behind.
const WizardDesign = -2

// WizardNew is the wizard id for "opened, but nothing created yet".
//
// The first screen only asks which repository to look at, and creating a
// proposal row before the operator has answered would litter the database with
// abandoned drafts every time somebody opened the wizard and changed their
// mind. A negative id can never collide with a real one.
const WizardNew = -1

// FocusChoice is one of the areas an analysis can be pointed at.
type FocusChoice struct {
	Label string
	Hint  string
	On    bool
}

// ProposedRow is one proposed task on the review step.
type ProposedRow struct {
	ID   int64
	Goal string
	// Meta is the verify command, threshold and cap as one line, because on a
	// list of twelve they are scanned rather than read.
	Meta      string
	Rationale string
	Evidence  []string
	DependsOn []string
	Severity  string
	Verify    string
	Cap       string
	Selected  bool
	// Queued marks a row that already produced a task on an earlier pass.
	// It stays on the list rather than disappearing, so reopening an analysis
	// shows what was acted on as well as what is left.
	Queued bool
	// TaskURL points at the task this row became.
	TaskURL string
}

// WizardWaiting explains a step that is working rather than asking.
type WizardWaiting struct {
	Title string
	Body  string
}

// WizardView is the repository analysis wizard.
type WizardView struct {
	ID    int64
	Step  int
	State string

	RepoPath  string
	SourceURL string
	Detected  string
	Model     string
	Spend     string
	Err       string

	Focus    []FocusChoice
	Notes    string
	MaxTasks int
	// Models are the (provider, model) pairs the analyse role may be pointed
	// at for this one analysis, grouped by provider.
	Models []ModelChoice

	Waiting *WizardWaiting
	Live    *LivePane

	Rows     []ProposedRow
	Selected int
	// AlreadyQueued is how many of these rows became tasks on an earlier
	// pass, which is what makes a reopened analysis legible.
	AlreadyQueued int

	// Kind is analyse or create, as stored on the proposal.
	Kind string
	// Designing is the architect flow rather than the analysis one. Separate
	// from Kind because it is true before a proposal exists — the first screen
	// creates no row — and because a redesign of an existing repository is an
	// analyse-kind proposal that still went through the conversation.
	Designing bool
	// Design is the conversation surface, on the steps where there is one.
	Design *DesignPane
	// Repos are the registered repositories offered on the first step. A
	// dropdown rather than a typed path: analysing the same repository twice is
	// the normal case, and the second time should not need the path again.
	Repos []RepoChoice
	// ReposURL opens the Repos overlay, for a repository not on the list yet.
	ReposURL string

	CloseURL string
}

// DesignPane is the architect conversation.
type DesignPane struct {
	Turns []DesignTurn
	// Busy is true while a reply is in flight — the operator's turn was the
	// last thing said. The reply box stays open anyway, because thinking of
	// the next thing while it answers is the normal way to use this.
	Busy bool
	// Spend is what the conversation has cost so far. Said plainly, because a
	// design conversation is the one surface where an operator can spend a
	// lot without noticing they are doing it.
	Spend string
	// Target names what is being designed: a repository, or nothing yet.
	Target string
	// Accepted is true once the conversation is over.
	Accepted bool
}

// DesignTurn is one thing said.
type DesignTurn struct {
	Speaker string
	Body    string
	When    string
	// Mine is true for the operator's turns, which the template sides
	// differently.
	Mine bool
	Err  bool
}

// FocusAreas are the choices offered on the wizard's second step.
//
// They exist because "find me some work" produces a scattering of unrelated
// suggestions, whereas a stated focus produces a queue an operator can act on
// in one sitting. The free-text notes field is where anything not on this list
// goes.
var FocusAreas = []FocusChoice{
	{Label: "test coverage", Hint: "behaviour that nothing currently proves"},
	{Label: "correctness", Hint: "unhandled errors and broken edge cases"},
	{Label: "DRY / KISS", Hint: "duplication, needless indirection, code that could be smaller"},
	{Label: "tech debt", Hint: "dead code, tangled boundaries, abandoned migrations"},
	{Label: "security", Hint: "unvalidated input, secret handling, permissions"},
	{Label: "performance", Hint: "work repeated per request or per row"},
	{Label: "documentation", Hint: "READMEs and comments the code has outgrown"},
	{Label: "general suggestions", Hint: "anything worth doing that the other areas miss"},
}

// Dashboard is the whole page. There is only one page: the board and the task
// detail are two panes of the same view, because choosing a task and reading
// it are the same motion.
type Dashboard struct {
	Q Query

	RunSummary  string
	SandboxNote string
	// Spend is subscription-covered CLI usage — a usage signal, not a bill.
	// Metered is usage against an endpoint the operator configured, which is
	// real money. They are shown separately and never added.
	Spend      string
	Metered    string
	HasMetered bool
	RunCap     string
	OverRunCap bool

	PauseReason string
	// StoppedAll distinguishes the operator's own global stop from the
	// authentication pause. They share the dispatch gate but not the banner:
	// one is a decision to undo, the other a condition to retry.
	StoppedAll bool

	Budget *BudgetAlert
	Toast  *Toast

	Filters   []Chip
	Rows      []Row
	ListCount string
	Empty     bool
	BulkCount int
	GroupURL  string

	Sel *Detail

	CLIText  string
	Add      AddForm
	Wizard   *WizardView
	Settings *SettingsView
	// Running is the nav chip for an analysis in progress or waiting to be
	// reviewed. Without it, closing the wizard's tab loses the only link to a
	// run that is still going.
	Running    *RunningAnalysis
	Analyses   []AnalysisRow
	ShowingAll bool

	// Repos and Backlog are the two repository surfaces. RepoChip shows the
	// active filter, and OpenBacklog is how many items are waiting across
	// every repository.
	Repos       *ReposView
	Backlog     *BacklogView
	RepoChip    *RepoChip
	OpenBacklog int
}

// RunningAnalysis is the nav chip pointing at an in-flight or unreviewed
// analysis.
type RunningAnalysis struct {
	Label string
	URL   string
	Live  bool
}

// AnalysisRow is one past analysis in the history list.
type AnalysisRow struct {
	ID    int64
	State string
	Tone  string
	Repo  string
	When  string
	Spend string
	Focus string
	// Progress reads "3 of 12 queued", which is the number that decides
	// whether there is anything left to act on.
	Progress string
	// Remaining is how many proposed tasks have not become real ones.
	Remaining int
	// OpenURL reopens the analysis; empty when there is nothing to reopen.
	OpenURL string
	Err     string
}

// buildWizard assembles the analysis wizard for the proposal in the URL.
func buildWizard(p store.Proposal, rows []store.ProposalTask, live *LivePane, q Query) *WizardView {
	w := &WizardView{
		ID:        p.ID,
		State:     p.State,
		RepoPath:  p.RepoPath,
		SourceURL: p.SourceURL,
		Detected:  p.Detected,
		Model:     p.Model,
		Spend:     money(p.CostUSD),
		Err:       p.ErrMsg,
		Notes:     p.Notes,
		MaxTasks:  p.MaxTasks,
		Live:      live,
		CloseURL:  q.URL("wizard", 0),
	}

	chosen := map[string]bool{}
	for _, f := range p.Focus {
		chosen[f] = true
	}
	for _, area := range FocusAreas {
		area.On = chosen[area.Label]
		w.Focus = append(w.Focus, area)
	}

	w.Kind = p.Kind
	switch p.State {
	case store.ProposalDesigning:
		w.Step = StepDesign
	case store.ProposalScaffolding:
		w.Step = StepRun
		w.Waiting = &WizardWaiting{
			Title: "Building the scaffold",
			Body: "One turn, writing the skeleton every later task assumes: the layout, " +
				"the manifest, something that builds, and a test command that works. " +
				"It is committed straight to the default branch — the features come " +
				"afterwards, as tasks, each planned and reviewed on its own branch.",
		}
	case store.ProposalCloning:
		w.Step = StepSource
		w.Waiting = &WizardWaiting{
			Title: "Cloning",
			Body:  "Fetching " + p.SourceURL + ". This runs in the background — the page will move on by itself.",
		}
	case store.ProposalDraft:
		w.Step = StepFocus
	case store.ProposalAnalysing:
		w.Step = StepRun
		w.Waiting = &WizardWaiting{
			Title: "Reading the repository",
			Body:  "The analysis has the repository mounted read-only. It cannot edit, commit or run anything that writes.",
		}
	case store.ProposalFailed:
		// Failure lands on whichever step could act on it: a clone that never
		// produced a repository has nothing to review.
		w.Step = StepFocus
		if p.RepoPath == "" {
			w.Step = StepSource
		}
	default:
		w.Step = StepReview
	}

	for _, r := range rows {
		meta := []string{"blocking: " + r.Severity}
		if r.Verify != "" {
			meta = append([]string{"verify: " + r.Verify}, meta...)
		} else {
			meta = append([]string{"no verify command"}, meta...)
		}
		if r.CostCap > 0 {
			meta = append(meta, "cap "+money(r.CostCap))
		}
		row := ProposedRow{
			ID:        r.ID,
			Goal:      r.Goal,
			Queued:    r.CreatedTaskID != 0,
			Meta:      strings.Join(meta, " · "),
			Rationale: r.Rationale,
			Evidence:  r.Evidence,
			DependsOn: r.DependsOn,
			Severity:  r.Severity,
			Verify:    r.Verify,
			Cap:       fmt.Sprintf("%.2f", r.CostCap),
			Selected:  r.Selected,
		}
		switch {
		case row.Queued:
			row.TaskURL = q.URL("sel", r.CreatedTaskID, "wizard", 0)
			w.AlreadyQueued++
		case r.Selected:
			// Only rows that have not already become tasks count towards the
			// queue button: pressing it must not promise to re-create work
			// that is already on the board.
			w.Selected++
		}
		w.Rows = append(w.Rows, row)
	}
	return w
}

// taskFacts is everything the board needs about one task, gathered once.
type taskFacts struct {
	Task   store.Task
	Totals store.Totals
	Rounds []round
	// RepoSlug names the registered repository, which is what the board groups
	// by. Two repositories can sit in directories with the same basename, and
	// grouping by that merged them under one heading.
	RepoSlug string
	Blocked  bool
	// UnmetDeps are the dependencies that have not reached done.
	UnmetDeps []store.Task
}

// OverCap reports whether the task has spent past its advisory ceiling.
func (f taskFacts) OverCap() bool {
	return f.Task.CostCapUSD > 0 && f.Totals.CostUSD > f.Task.CostCapUSD
}

// buildRows turns the visible tasks into list rows, grouped by repository when
// the operator asked for it.
func buildRows(facts []taskFacts, q Query) []Row {
	var rows []Row
	emit := func(f taskFacts) {
		t := f.Task
		state := stateLabel(t, f.Blocked)
		r := Row{
			ID:       t.ID,
			Selected: q.Sel == t.ID,
			State:    state,
			Tone:     TaskTone(t),
			Goal:     t.Goal,
			Repo:     repoName(t.RepoPath) + " · " + branchName(t),
			Bars:     bars(f.Rounds, 6, 4, 15),
			Note:     rowNote(f),
			PR:       t.PRURL,
			URL:      q.URL("sel", t.ID, "tab", defaultTab(t)),
			Bulkable: t.State == "escalated" || t.State == "failed",
			Checked:  q.HasBulk(t.ID),
			BulkURL:  q.URL("bulk", t.ID),
		}
		if f.Blocked {
			r.Tone = ToneMuted
		}
		if t.Phase != "" {
			r.Progress = Progress(t)
		}
		r.Meta = strings.TrimSpace(strings.Join(nonEmpty(
			money(f.Totals.CostUSD), elapsed(t)), " · "))
		rows = append(rows, r)
	}

	if !q.Group {
		for _, f := range facts {
			emit(f)
		}
		return rows
	}

	// Group headers carry the repo's share of the spend, which is the number
	// an operator actually wants when deciding what to stop.
	var order []string
	byRepo := map[string][]taskFacts{}
	for _, f := range facts {
		name := f.RepoSlug
		if name == "" {
			name = repoName(f.Task.RepoPath)
		}
		if _, ok := byRepo[name]; !ok {
			order = append(order, name)
		}
		byRepo[name] = append(byRepo[name], f)
	}
	for _, name := range order {
		group := byRepo[name]
		var spend float64
		for _, f := range group {
			spend += f.Totals.CostUSD
		}
		rows = append(rows, Row{
			Header: true,
			Label:  name,
			Sub:    fmt.Sprintf("%s · %s", plural(len(group), "task"), money(spend)),
		})
		for _, f := range group {
			emit(f)
		}
	}
	return rows
}

// rowNote is the one line of explanation a row earns: why it is stuck, why it
// failed, or what it is waiting for.
func rowNote(f taskFacts) string {
	if f.Blocked {
		names := make([]string, 0, len(f.UnmetDeps))
		for _, d := range f.UnmetDeps {
			names = append(names, fmt.Sprintf("%s (%s)", d.Slug, d.State))
		}
		return "Waiting on " + strings.Join(names, ", ") + "."
	}
	if f.OverCap() {
		return fmt.Sprintf("Spent %s against a %s cap.",
			money(f.Totals.CostUSD), money(f.Task.CostCapUSD))
	}
	if note := noRemoteNote(f.Task); note != "" {
		return note
	}
	return f.Task.ErrMsg
}

// noRemoteNote explains a task that converged with nowhere to push.
//
// Derived rather than stored: a done task with no pull request URL is already
// exactly that statement. Writing it into ErrMsg would render a task that
// succeeded in the styling reserved for ones that broke.
func noRemoteNote(t store.Task) string {
	if t.State != "done" || t.PRURL != "" {
		return ""
	}
	return "Done, with no remote to push to — the work is on " + branchName(t) +
		". Add an origin and the next task opens a pull request."
}

// defaultTab picks the right-hand pane that answers the first question a task
// in this state raises: a parked one is about its findings, a running one
// about what the agent is doing right now, anything else about the change.
func defaultTab(t store.Task) string {
	switch t.State {
	case "escalated", "failed":
		return TabFindings
	case "planning", "executing", "verifying", "plan_review", "code_review":
		return TabLive
	default:
		return TabDiff
	}
}

// buildFilters builds the state filter row with its counts. Counts are over
// every task, not the filtered set, so the row does not change under the
// operator as they click through it.
func buildFilters(all []taskFacts, q Query) []Chip {
	counts := map[string]int{FilterAll: len(all)}
	for _, f := range all {
		switch TaskTone(f.Task) {
		case ToneAlert:
			counts[FilterAttention]++
		case ToneLive:
			counts[FilterRunning]++
		}
		if f.Task.State == "done" {
			counts[FilterDone]++
		}
	}
	labels := []struct{ id, label string }{
		{FilterAll, "All"},
		{FilterAttention, "Needs you"},
		{FilterRunning, "Running"},
		{FilterDone, "Done"},
	}
	out := make([]Chip, 0, len(labels))
	for _, l := range labels {
		out = append(out, Chip{
			Label: l.label,
			Count: counts[l.id],
			On:    q.Filter == l.id,
			URL:   q.URL("filter", l.id),
		})
	}
	return out
}

// matches applies the search box to one task.
func matches(t store.Task, search string) bool {
	if search == "" {
		return true
	}
	hay := strings.ToLower(strings.Join([]string{
		t.Goal, t.Slug, t.RepoPath, t.Branch, t.State,
	}, " "))
	return strings.Contains(hay, strings.ToLower(search))
}

// keep applies the state filter to one task.
func keep(t store.Task, filter string) bool {
	switch filter {
	case FilterAttention:
		return Tone(t.State) == ToneAlert
	case FilterRunning:
		return Tone(t.State) == ToneLive
	case FilterDone:
		return t.State == "done"
	default:
		return true
	}
}

// buildDetail assembles the right-hand side for the selected task.
func buildDetail(f taskFacts, steps []store.Step, byStep map[int64][]store.Finding,
	takeOver, sandbox string, q Query) *Detail {

	t := f.Task
	d := &Detail{
		ID:       t.ID,
		Slug:     t.Slug,
		State:    stateLabel(t, f.Blocked),
		Tone:     TaskTone(t),
		Progress: "not started",
		Branch:   branchName(t),
		Goal:     t.Goal,
	}
	if t.Phase != "" {
		d.Progress = Progress(t)
	}
	d.Spend = money(f.Totals.CostUSD)
	if t.CostCapUSD > 0 {
		d.Spend += " of " + money(t.CostCapUSD)
	}

	d.Chips = []Tag{
		{Label: repoName(t.RepoPath), Kind: "neutral"},
		{Label: "blocking: " + t.BlockingSeverity, Kind: severityKind(t.BlockingSeverity)},
	}
	if t.VerifyCommand != "" {
		d.Chips = append(d.Chips, Tag{Label: "verify: " + t.VerifyCommand, Kind: "neutral"})
	} else {
		d.Chips = append(d.Chips, Tag{Label: "no verify command", Kind: "outline"})
	}
	if sandbox != "" {
		d.Chips = append(d.Chips, Tag{Label: sandbox, Kind: "neutral"})
	}

	d.Actions = detailActions(f)
	d.Banner = detailBanner(f, takeOver)
	d.Steps = buildSteps(f, steps, byStep, q)
	d.StepCount = fmt.Sprintf("%s · %s", plural(len(steps), "step"), elapsed(t))
	d.Converge = bars(f.Rounds, 0, 8, 52)
	d.ConvergeNote = convergeNote(f)
	d.FP = fingerprint(f.Rounds)
	return d
}

func severityKind(sev string) string {
	if sev == "any" {
		return "outline"
	}
	return "accent"
}

// detailActions are the state changes available on the selected task.
func detailActions(f taskFacts) []Action {
	t := f.Task
	id := strconv.FormatInt(t.ID, 10)
	var out []Action

	if t.PRURL != "" {
		out = append(out, Action{Label: "Open pull request", Kind: "primary", Href: t.PRURL})
	}
	switch t.State {
	case "escalated":
		out = append(out,
			Action{Label: "Continue ×10", Kind: "primary", Post: "/task/" + id + "/continue"})
		if next := looserSeverity(t.BlockingSeverity); next != "" {
			out = append(out, Action{
				Label:  "Only block on " + next,
				Kind:   "secondary",
				Post:   "/task/" + id + "/severity",
				Fields: []Field{{Name: "blocking_severity", Value: next}},
			})
		}
	case "queued":
		if f.Blocked {
			out = append(out, Action{
				Label: "Release anyway", Kind: "primary",
				Post: "/task/" + id + "/release",
			})
		}
	}
	if f.OverCap() {
		out = append(out, Action{
			Label:  "Raise cap to " + money(nextCap(f.Totals.CostUSD, t.CostCapUSD)),
			Kind:   "secondary",
			Post:   "/task/" + id + "/cap",
			Fields: []Field{{Name: "cost_cap", Value: fmt.Sprintf("%.2f", nextCap(f.Totals.CostUSD, t.CostCapUSD))}},
		})
	}
	// Stop, start and restart, in the order an operator reaches for them.
	switch {
	case t.Stopped():
		out = append(out,
			Action{Label: "Start", Kind: "primary", Post: "/task/" + id + "/start"})
	case !loop.IsTerminal(t.State):
		// Soft first. It costs at most one agent turn and leaves nothing
		// half-written, so it is the one that should be easy to press.
		out = append(out,
			Action{Label: "Stop", Kind: "secondary", Post: "/task/" + id + "/stop"},
			Action{
				Label:  "Stop now",
				Kind:   "ghost",
				Post:   "/task/" + id + "/stop",
				Fields: []Field{{Name: "now", Value: "1"}},
			})
	}
	if t.PRURL == "" {
		out = append(out, Action{Label: "Restart", Kind: "ghost", Post: "/task/" + id + "/restart"})
	}
	if !loop.IsTerminal(t.State) {
		out = append(out, Action{Label: "Abandon", Kind: "ghost", Post: "/task/" + id + "/abandon"})
	}
	return out
}

// looserSeverity is the next threshold up from sev, or "" at the top. It is
// what the dashboard offers a task that is ping-ponging on findings below the
// level anyone cares about.
func looserSeverity(sev string) string {
	order := []string{"any", "minor", "major", "critical"}
	for i, s := range order {
		if s == sev && i+1 < len(order) {
			return order[i+1]
		}
	}
	return ""
}

// nextCap is the round number the dashboard offers as a new ceiling: the next
// multiple of $5 strictly above both the current spend and the current cap.
// An offer that is not above both would be a button that does nothing.
func nextCap(spent, cap float64) float64 {
	const step = 5.0
	base := math.Max(spent, cap)
	next := math.Ceil(base/step) * step
	if next <= base {
		next += step
	}
	return next
}

func detailBanner(f taskFacts, takeOver string) *Banner {
	t := f.Task
	id := strconv.FormatInt(t.ID, 10)

	switch {
	case t.State == "escalated":
		body := t.ErrMsg
		if body == "" {
			body = "The loop reached its iteration budget without converging."
		}
		return &Banner{
			Title: fmt.Sprintf("Parked at iteration %d — %s", t.Iteration, t.Phase),
			Body:  body,
			Actions: []Action{
				{Label: "Continue ×10", Kind: "primary", Post: "/task/" + id + "/continue"},
				{Label: "Abandon", Kind: "secondary", Post: "/task/" + id + "/abandon"},
			},
			TakeOver: takeOver,
		}
	case t.State == "failed":
		body := t.ErrMsg
		if body == "" {
			body = "The task stopped before it finished."
		}
		return &Banner{
			Title:    "Failed — the branch and worktree are kept",
			Body:     body,
			TakeOver: takeOver,
		}
	case f.Blocked:
		names := make([]string, 0, len(f.UnmetDeps))
		for _, d := range f.UnmetDeps {
			names = append(names, fmt.Sprintf("%s (%s)", d.Slug, d.State))
		}
		return &Banner{
			Title: "Blocked by a dependency",
			Body: "No worker will claim this task until " + strings.Join(names, " and ") +
				" reaches done. A dependency that failed will never get there — release the task if it no longer matters.",
			Actions: []Action{
				{Label: "Release anyway", Kind: "primary", Post: "/task/" + id + "/release"},
			},
		}
	}
	return nil
}

func convergeNote(f taskFacts) string {
	if len(f.Rounds) == 0 {
		return "Blocking findings per review round. Nothing has been reviewed yet."
	}
	base := "Blocking findings per review round. A hollow bar is a round that raised nothing."
	last := f.Rounds[len(f.Rounds)-1]
	if last.Blocking == 0 {
		return base + fmt.Sprintf(" The last round (%s) was clean.", last.Label)
	}
	return base + fmt.Sprintf(" The last round (%s) raised %s.",
		last.Label, plural(last.Blocking, "finding"))
}

// defaultStep is the timeline step opened when the operator has not chosen
// one: whatever is running, or failing that the last step that actually said
// something. Opening a task to a wall of collapsed rows hides the findings,
// which are the only reason to open a task.
func defaultStep(steps []store.Step, byStep map[int64][]store.Finding) int {
	open := -1
	for i, s := range steps {
		if s.State == "running" {
			return i
		}
		if len(byStep[s.ID]) > 0 {
			open = i
		}
	}
	if open < 0 && len(steps) > 0 {
		open = len(steps) - 1
	}
	return open
}

// buildSteps lays the timeline out in two lanes.
func buildSteps(f taskFacts, steps []store.Step, byStep map[int64][]store.Finding, q Query) []StepCard {
	openSummaries := map[string]bool{}
	if n := len(f.Rounds); n > 0 {
		for _, s := range f.Rounds[n-1].Summaries {
			openSummaries[s] = true
		}
	}
	openStep := q.Step
	if !q.StepSet {
		openStep = defaultStep(steps, byStep)
	}

	var out []StepCard
	prevBlocking := 0
	for i, s := range steps {
		card := StepCard{
			Col:       1,
			Agent:     s.Agent,
			Phase:     fmt.Sprintf("%s i%d", s.Phase, s.Iteration),
			Duration:  stepDuration(s),
			Cost:      money(s.CostUSD),
			Title:     stepTitle(f.Task, s, prevBlocking),
			Verdict:   s.Verdict,
			VerdictOK: s.Verdict == "approved",
			Live:      s.State == "running",
			Open:      openStep == i,
			ToggleURL: q.URL("step", toggleStep(openStep, i)),
			Err:       s.ErrMsg,
		}
		// The lane is the role's, so a reviewer running through the same CLI
		// as the coder still reads as a reviewer.
		if !isCoder(s.Agent) {
			card.Col = 2
		}
		if s.TranscriptPath != "" {
			card.TranscriptURL = fmt.Sprintf("/task/%d/transcript/%d", f.Task.ID, s.ID)
		}
		blocking := 0
		for _, fi := range byStep[s.ID] {
			if fi.Blocking {
				blocking++
			}
			card.Findings = append(card.Findings, FindingRow{
				Severity: fi.Severity,
				Summary:  fi.Summary,
				Loc:      location(fi),
				Detail:   fi.Detail,
				Blocking: fi.Blocking,
				Open:     openSummaries[fi.Summary],
			})
		}
		if s.Verdict != "" {
			prevBlocking = blocking
		}
		out = append(out, card)
	}
	return out
}

// toggleStep collapses an already-open step rather than reopening it.
func toggleStep(open, i int) int {
	if open == i {
		return -1
	}
	return i
}

// Step agent values. A step records the ROLE it played, not the CLI that ran
// it — with roles free to pick either agent, "claude" no longer means "the
// coder". Databases written by an earlier build hold the CLI name instead, so
// both spellings are recognised.
const (
	stepCoder    = "code"
	stepReviewer = "review"
	stepVerify   = "verify"
)

// isCoder reports whether a step is the one that writes the change.
func isCoder(agent string) bool { return agent == stepCoder || agent == "claude" }

// isReviewer reports whether a step produced a verdict on someone else's work.
func isReviewer(agent string) bool { return agent == stepReviewer || agent == "codex" }

// stepTitle says what a step did, built from what the store actually records.
// There is no title column: inventing one at write time would freeze a
// description the loop later contradicts.
func stepTitle(t store.Task, s store.Step, prevBlocking int) string {
	switch {
	case isCoder(s.Agent):
		switch {
		case s.Phase == "plan" && s.Iteration <= 1:
			return "Wrote PLAN.md from the goal and constraints."
		case s.Phase == "plan":
			return fmt.Sprintf("Revised the plan against %s.", plural(prevBlocking, "finding"))
		case s.Iteration <= 1:
			return "Fresh session seeded with PLAN.md. Implemented the plan."
		default:
			return fmt.Sprintf("Resumed the same session with %s.", plural(prevBlocking, "finding"))
		}
	case s.Agent == stepVerify:
		cmd := t.VerifyCommand
		if cmd == "" {
			cmd = "verify"
		}
		if s.Verdict == "approved" {
			return cmd + " — exit 0"
		}
		return fmt.Sprintf("%s — exit %d", cmd, s.ExitCode)
	case isReviewer(s.Agent):
		if s.Phase == "plan" {
			return "Reviewed PLAN.md against the goal."
		}
		if t.BaseRef != "" {
			return "Reviewed the diff against " + t.BaseRef + "."
		}
		return "Reviewed the accumulated diff."
	}
	return s.Agent + " step"
}

// buildRight assembles the right-hand pane for the current tab.
func buildRight(f taskFacts, steps []store.Step, byStep map[int64][]store.Finding,
	diff []worktree.FileDiff, diffErr error, live *LivePane, plan *PlanPane, q Query) Right {

	rows := ledger(steps, byStep, f.Rounds)
	open := 0
	for _, r := range rows {
		if r.Open {
			open++
		}
	}

	r := Right{
		Tab: q.Tab,
		Tabs: []Chip{
			{Label: "Diff", On: q.Tab == TabDiff, URL: q.URL("tab", TabDiff)},
			{Label: "Findings", On: q.Tab == TabFindings, URL: q.URL("tab", TabFindings)},
			{Label: "Live", On: q.Tab == TabLive, URL: q.URL("tab", TabLive)},
			{Label: "Plan", On: q.Tab == TabPlan, URL: q.URL("tab", TabPlan)},
		},
	}

	switch q.Tab {
	case TabFindings:
		r.Title = "Finding ledger"
		r.Sub = fmt.Sprintf("%d open · %d resolved", open, len(rows)-open)
		r.Ledger = rows
		r.Intro = "Every finding raised on this task, in the order it first appeared. " +
			"A finding stays on the ledger after it is fixed, so a finding that comes " +
			"back reads as a repeat rather than as something new."
	case TabLive:
		r.Title = "Live output"
		r.Live = live
		if live != nil {
			r.Sub = live.Meta
		}
	case TabPlan:
		r.Title = "Plan"
		r.Plan = plan
		if plan != nil && plan.Editable {
			r.Sub = "editable while stopped"
		}
	default:
		r.Title = "Diff"
		r.Diff = buildDiff(f, rows, diff, diffErr, q)
		if r.Diff.Err == "" && r.Diff.Empty == "" {
			added, removed := 0, 0
			for _, fd := range diff {
				added += fd.Added
				removed += fd.Removed
			}
			r.Sub = fmt.Sprintf("%s vs %s · +%d −%d",
				branchName(f.Task), baseName(f.Task), added, removed)
		}
	}
	return r
}

func buildDiff(f taskFacts, rows []LedgerRow, files []worktree.FileDiff, err error, q Query) *DiffPane {
	p := &DiffPane{}
	if err != nil {
		p.Err = err.Error()
		return p
	}
	if f.Task.WorktreeDir == "" || f.Task.BaseRef == "" {
		p.Empty = "This task has no worktree yet, so there is nothing to diff."
		return p
	}
	if len(files) == 0 {
		p.Empty = "No changes against " + baseName(f.Task) + " yet."
		return p
	}

	sel := q.File
	found := false
	for _, fd := range files {
		if fd.Path == sel {
			found = true
		}
	}
	if !found {
		sel = files[0].Path
	}
	for _, fd := range files {
		p.Files = append(p.Files, DiffTab{
			Name: fd.Path,
			Stat: fd.Stat(),
			On:   fd.Path == sel,
			URL:  q.URL("file", fd.Path),
		})
	}

	// Anchor the still-open findings to the lines they were raised against.
	// A finding read next to the code it is about needs no cross-referencing.
	anchors := map[int]*Anchor{}
	for _, r := range rows {
		if !r.Open {
			continue
		}
		file, line := splitLoc(r.Loc)
		if file != sel || line == 0 {
			continue
		}
		anchors[line] = &Anchor{Severity: r.Severity, Round: r.Life, Text: r.Summary}
	}

	for _, fd := range files {
		if fd.Path != sel {
			continue
		}
		p.Truncated = fd.Truncated
		if fd.Binary {
			p.Empty = "Binary file — nothing to show."
			return p
		}
		for _, l := range fd.Lines {
			row := DiffRow{Kind: l.Kind, Text: l.Text}
			if l.A > 0 {
				row.A = strconv.Itoa(l.A)
			}
			if l.B > 0 {
				row.B = strconv.Itoa(l.B)
				row.Anchor = anchors[l.B]
			}
			p.Lines = append(p.Lines, row)
		}
	}
	return p
}

// splitLoc pulls file and line back out of a "file:line" location.
func splitLoc(loc string) (string, int) {
	i := strings.LastIndexByte(loc, ':')
	if i < 0 {
		return loc, 0
	}
	n, err := strconv.Atoi(loc[i+1:])
	if err != nil {
		return loc, 0
	}
	return loc[:i], n
}

// --- small shared helpers ---

func repoName(path string) string {
	if path == "" {
		return "—"
	}
	return filepath.Base(strings.TrimRight(path, "/"))
}

func branchName(t store.Task) string {
	if t.Branch != "" {
		return t.Branch
	}
	return "overseer/" + t.Slug
}

func baseName(t store.Task) string {
	if t.BaseRef != "" {
		return t.BaseRef
	}
	return "the base branch"
}

func money(v float64) string { return fmt.Sprintf("$%.2f", v) }

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// humanDuration renders a duration compactly for the dashboard.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// elapsed reports how long a task has been going, or how long it took.
func elapsed(t store.Task) string {
	end := time.Now()
	if t.State == "done" || t.State == "failed" {
		end = t.UpdatedAt
	}
	if t.CreatedAt.IsZero() {
		return ""
	}
	return humanDuration(end.Sub(t.CreatedAt))
}

// stepDuration reports how long one step took.
func stepDuration(s store.Step) string {
	if s.StartedAt.IsZero() {
		return ""
	}
	end := s.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	return humanDuration(end.Sub(s.StartedAt))
}
