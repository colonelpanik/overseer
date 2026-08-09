package web

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"overseer/internal/agent"
	"overseer/internal/store"
)

// RepoRow is one repository in the Repos overlay.
type RepoRow struct {
	ID     int64
	Slug   string
	Path   string
	Origin string
	Branch string
	// Detected is the toolchain probe's one-line summary.
	Detected string
	// Defaults reads "make check · major · $4 cap", or is empty when the
	// repository inherits everything from the daemon.
	Defaults string
	// The same three settings as raw values, so the inline edit form renders
	// pre-filled. Without these the form would post empty fields and quietly
	// clear settings the operator never touched.
	Verify   string
	Severity string
	Cap      string
	Counts   string
	Backlog  int
	// AgentTime and Turns lead, because unlike the money figures they are
	// always true whatever provider served the work.
	AgentTime string
	Turns     string
	// Reported is subscription-covered CLI usage; Metered is money. They are
	// never added together — see the note the overlay carries.
	Reported string
	Metered  string
	Archived bool

	BoardURL   string
	BacklogURL string
	Analysing  bool
}

// ReposView is the Repos overlay.
type ReposView struct {
	Rows []RepoRow
	// Note explains the two spend figures. It is on the page rather than in a
	// tooltip because presenting subscription-covered usage as money spent
	// would be the dashboard lying, and the correction has to be as visible as
	// the number.
	Note      string
	Reported  string
	Metered   string
	AgentTime string
	CloseURL  string
	Empty     bool
}

// BacklogRow is one item on a repository's todo list.
type BacklogRow struct {
	ID    int64
	Title string
	// Title is the one line the item is listed under and Body is its full text
	// when that is longer — an analysis's proposal is a paragraph, and the
	// list has to be scannable before it is readable.
	Body     string
	Detail   string
	Evidence []string
	Severity string
	Source   string
	State    string
	Tone     string
	// Seen reads "seen 3×" when an item has been raised more than once, which
	// is a far stronger signal than three identical rows would be.
	Seen string
	// Origin says where it came from, in words.
	Origin  string
	When    string
	TaskID  int64
	Actions bool
}

// BacklogView is one repository's durable todo list.
type BacklogView struct {
	RepoID   int64
	RepoSlug string
	RepoPath string
	// Groups are the three sources, in the order they are worth reading:
	// what a review found, what an analysis proposed, what you wrote down.
	Groups   []BacklogGroup
	Open     int
	Total    int
	Repos    []RepoChoice
	CloseURL string
	BoardURL string
	Empty    bool
	// Severities are the FINDING severities an item may carry — not the task
	// thresholds, which include "any" and exclude "nit".
	Severities []string
}

// BacklogGroup is one source's items.
type BacklogGroup struct {
	Source string
	Label  string
	Blurb  string
	Rows   []BacklogRow
}

// RepoChoice is one repository in a picker.
type RepoChoice struct {
	ID    int64
	Slug  string
	Path  string
	On    bool
	URL   string
	Count int
}

// buildRepos assembles the Repos overlay.
func (s *Server) buildRepos(ctx context.Context, q Query) (*ReposView, error) {
	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := s.eng.RepoStats(ctx)
	if err != nil {
		return nil, err
	}
	analysing, err := s.analysingRepos(ctx)
	if err != nil {
		return nil, err
	}

	v := &ReposView{
		CloseURL: q.URL("overlay", ""),
		Note: "Agent time and turns are always true. Reported is what the claude and codex CLIs " +
			"say the usage would cost through the API — those run on your subscription, so it is a " +
			"usage signal, not a bill. Metered is usage against an endpoint you configured yourself, " +
			"which is your own money. The two are never added together.",
	}

	var reported, metered float64
	var agentTime time.Duration
	for _, r := range repos {
		st := stats[r.ID]
		reported += st.Reported
		metered += st.Metered
		agentTime += st.AgentTime

		row := RepoRow{
			ID:         r.ID,
			Slug:       r.Slug,
			Path:       r.Path,
			Origin:     r.OriginURL,
			Branch:     r.DefaultBranch,
			Detected:   r.Detected,
			Defaults:   repoDefaults(r),
			Verify:     r.VerifyCommand,
			Severity:   r.BlockingSeverity,
			Cap:        capValue(r.CostCapUSD),
			Counts:     repoCounts(st),
			Backlog:    st.Backlog,
			AgentTime:  duration(st.AgentTime),
			Turns:      plural(st.Turns, "turn"),
			Reported:   money(st.Reported),
			Metered:    money(st.Metered),
			Archived:   r.Archived(),
			Analysing:  analysing[r.ID],
			BoardURL:   q.URL("repo", r.ID, "overlay", ""),
			BacklogURL: q.URL("repo", r.ID, "overlay", "backlog"),
		}
		v.Rows = append(v.Rows, row)
	}

	v.Empty = len(v.Rows) == 0
	v.Reported = money(reported)
	v.Metered = money(metered)
	v.AgentTime = duration(agentTime)
	return v, nil
}

// repoChoices are the registered repositories a picker may offer, newest last
// and archived ones left out.
//
// A repository the operator has put away should not be in the list of things to
// start new work on — that is what archiving it meant.
func (s *Server) repoChoices(ctx context.Context, q Query) ([]RepoChoice, error) {
	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RepoChoice, 0, len(repos))
	for _, r := range repos {
		if r.Archived() {
			continue
		}
		out = append(out, RepoChoice{
			ID: r.ID, Slug: r.Slug, Path: r.Path, On: r.ID == q.Repo,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// RepoChip is the nav's "showing one repository" indicator, with the way out.
type RepoChip struct {
	Slug     string
	ClearURL string
}

// repoChip renders the active repo filter. A filter the operator cannot see is
// a board that looks empty for no stated reason.
func repoChip(repos map[int64]store.Repo, q Query) *RepoChip {
	if q.Repo == 0 {
		return nil
	}
	r, ok := repos[q.Repo]
	if !ok {
		return nil
	}
	return &RepoChip{Slug: r.Slug, ClearURL: q.URL("repo", 0)}
}

// openBacklogTotal is how many items are waiting across every repository, for
// the nav's count.
func (s *Server) openBacklogTotal(ctx context.Context) (int, error) {
	counts, err := s.store.OpenBacklogCounts(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	return total, nil
}

// analysingRepos is which repositories have an analysis in flight, so the repo
// list can say so rather than looking idle while one is running.
func (s *Server) analysingRepos(ctx context.Context) (map[int64]bool, error) {
	props, err := s.store.ListProposals(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]bool{}
	for _, p := range props {
		if p.RepoID != 0 && (p.State == store.ProposalAnalysing || p.State == store.ProposalCloning) {
			out[p.RepoID] = true
		}
	}
	return out, nil
}

func repoCounts(st store.RepoStats) string {
	if st.Tasks == 0 && st.Analyses == 0 {
		return "nothing run yet"
	}
	parts := []string{plural(st.Tasks, "task")}
	if st.Running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", st.Running))
	}
	if st.Done > 0 {
		parts = append(parts, fmt.Sprintf("%d done", st.Done))
	}
	if st.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", st.Failed))
	}
	if st.Analyses == 1 {
		parts = append(parts, "1 analysis")
	} else if st.Analyses > 1 {
		parts = append(parts, fmt.Sprintf("%d analyses", st.Analyses))
	}
	return strings.Join(parts, " · ")
}

// repoDefaults renders what new tasks on this repository inherit. Empty means
// the repository configures nothing and the daemon's defaults apply, which is
// worth saying differently from "no verify command anywhere".
func repoDefaults(r store.Repo) string {
	var parts []string
	if r.VerifyCommand != "" {
		parts = append(parts, r.VerifyCommand)
	}
	if r.BlockingSeverity != "" {
		parts = append(parts, "blocks on "+r.BlockingSeverity)
	}
	if r.CostCapUSD > 0 {
		parts = append(parts, money(r.CostCapUSD)+" cap")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// capValue renders a cost cap for a form field. Zero renders empty rather than
// "0.00", because zero means "inherit", and a field showing 0.00 would read as
// "no budget at all".
func capValue(v float64) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f", v)
}

// duration renders agent time coarsely. Nobody reads it to the second.
func duration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// backlogSources is the order the panel reads in: what a review actually found
// first, then what an analysis guessed at, then what you wrote down.
var backlogSources = []struct{ source, label, blurb string }{
	{store.BacklogReview, "From reviews",
		"Findings below the task's blocking threshold. The loop deliberately did not act on these — before the backlog they were shown once and thrown away."},
	{store.BacklogAnalysis, "From analyses",
		"Tasks an analysis proposed that nobody queued."},
	{store.BacklogManual, "Written down", "Things you added by hand."},
}

// buildBacklog assembles one repository's todo list.
func (s *Server) buildBacklog(ctx context.Context, q Query) (*BacklogView, error) {
	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return &BacklogView{CloseURL: q.URL("overlay", ""), Empty: true}, nil
	}

	counts, err := s.store.OpenBacklogCounts(ctx)
	if err != nil {
		return nil, err
	}

	// A backlog is always a particular repository's; landing on the panel with
	// no repository chosen shows whichever has the most waiting, which is the
	// one worth looking at.
	repoID := q.Repo
	if repoID == 0 {
		best := 0
		for _, r := range repos {
			if n := counts[r.ID]; n > best || repoID == 0 {
				repoID, best = r.ID, n
			}
		}
	}

	repo, err := s.store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	items, err := s.store.ListBacklog(ctx, repo.ID)
	if err != nil {
		return nil, err
	}

	v := &BacklogView{
		RepoID:     repo.ID,
		RepoSlug:   repo.Slug,
		RepoPath:   repo.Path,
		Total:      len(items),
		Open:       counts[repo.ID],
		CloseURL:   q.URL("overlay", ""),
		BoardURL:   q.URL("repo", repo.ID, "overlay", ""),
		Severities: append([]string{""}, agent.SeverityNames...),
		Empty:      len(items) == 0,
	}
	for _, r := range repos {
		if r.Archived() && r.ID != repo.ID {
			continue
		}
		v.Repos = append(v.Repos, RepoChoice{
			ID: r.ID, Slug: r.Slug, Path: r.Path,
			On:    r.ID == repo.ID,
			Count: counts[r.ID],
			URL:   q.URL("repo", r.ID, "overlay", "backlog"),
		})
	}
	sort.Slice(v.Repos, func(i, j int) bool { return v.Repos[i].Slug < v.Repos[j].Slug })

	bySource := map[string][]BacklogRow{}
	for _, item := range items {
		bySource[item.Source] = append(bySource[item.Source], backlogRow(item))
	}
	for _, g := range backlogSources {
		rows := bySource[g.source]
		if len(rows) == 0 {
			continue
		}
		v.Groups = append(v.Groups, BacklogGroup{
			Source: g.source, Label: g.label, Blurb: g.blurb, Rows: rows,
		})
	}
	return v, nil
}

func backlogRow(item store.BacklogItem) BacklogRow {
	row := BacklogRow{
		ID:       item.ID,
		Title:    item.Headline(),
		Body:     bodyOf(item.Title, item.Headline()),
		Detail:   item.Detail,
		Evidence: item.Evidence,
		Severity: item.Severity,
		Source:   item.Source,
		State:    item.State,
		Tone:     backlogTone(item),
		When:     humanAge(item.UpdatedAt),
		TaskID:   item.CreatedTaskID,
		// A queued item has become a task; there is nothing left to do to it
		// here, and offering the buttons anyway would only produce errors.
		Actions: item.State != store.BacklogQueued,
	}
	if item.Seen > 1 {
		row.Seen = fmt.Sprintf("seen %d×", item.Seen)
	}
	switch item.Source {
	case store.BacklogReview:
		if item.OriginTaskID != 0 {
			row.Origin = fmt.Sprintf("raised reviewing task %d", item.OriginTaskID)
		} else {
			row.Origin = "raised by a review"
		}
	case store.BacklogAnalysis:
		row.Origin = "proposed by an analysis"
	}
	return row
}

func backlogTone(item store.BacklogItem) string {
	switch {
	case item.State == store.BacklogDismissed:
		return ToneMuted
	case item.State == store.BacklogQueued:
		return ToneLive
	case item.Severity == "critical" || item.Severity == "major":
		return ToneAlert
	default:
		return ToneMuted
	}
}
