package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"overseer/internal/agent"
	"overseer/internal/config"
	"overseer/internal/sandbox"
	"overseer/internal/store"
	"overseer/internal/worktree"
)

// analysisTimeout is how long one analysis may run, from config.
//
// It used to be a hardcoded fifteen minutes, on the reasoning that reading a
// repository and writing a list is not open-ended work. That was wrong in the
// case the wizard is most for: a large repository nobody knows well is both
// the best reason to run an analysis and the slowest thing to read, and the
// timeout landed as a bare "step timeout after 15m0s" with no knob to turn.
func (e *Engine) analysisTimeout() time.Duration {
	if e.Cfg.AnalysisTimeout > 0 {
		return e.Cfg.AnalysisTimeout
	}
	return 30 * time.Minute
}

// StartProposal opens a wizard against a local repository path. The path is
// validated the same way a submitted task's repository is, so the wizard
// cannot get further than the first screen with a directory that is not a git
// repository.
func (e *Engine) StartProposal(ctx context.Context, repoPath string) (store.Proposal, error) {
	// Registering here is what makes an analysis part of a repository's
	// history rather than a loose row: EnsureRepo resolves, validates and
	// probes the path, which is everything the wizard's first screen needs.
	repo, err := e.ResolveRepo(ctx, repoPath)
	if err != nil {
		return store.Proposal{}, err
	}

	p, err := e.Store.CreateProposal(ctx, store.Proposal{
		RepoID:   repo.ID,
		RepoPath: repo.Path,
		State:    store.ProposalDraft,
		Model:    e.Cfg.AnalysisModel,
		MaxTasks: 12,
		Detected: repo.Detected,
	})
	if err != nil {
		return store.Proposal{}, err
	}
	e.notifyProposal()
	return p, nil
}

// ImportProposal clones a repository and opens a wizard against the clone.
//
// The clone happens in the background because it is the one step of the wizard
// that can take minutes, and the dashboard is a page that reloads on every
// state change rather than a request that can block.
func (e *Engine) ImportProposal(ctx context.Context, url string) (store.Proposal, error) {
	url = strings.TrimSpace(url)
	// Validate before creating anything: a proposal recording a URL overseer
	// would never fetch is just a row that can only ever fail.
	if err := worktree.ValidateCloneURL(url); err != nil {
		return store.Proposal{}, err
	}

	p, err := e.Store.CreateProposal(ctx, store.Proposal{
		SourceURL: url,
		State:     store.ProposalCloning,
		Model:     e.Cfg.AnalysisModel,
		MaxTasks:  12,
	})
	if err != nil {
		return store.Proposal{}, err
	}
	e.notifyProposal()

	go e.clone(context.WithoutCancel(ctx), p.ID, url)
	return p, nil
}

func (e *Engine) clone(ctx context.Context, proposalID int64, url string) {
	dest := filepath.Join(e.Cfg.ImportsDir(), worktree.CloneName(url))
	path, err := worktree.Clone(ctx, url, dest)
	if err != nil {
		// A clone spends no agent budget, so there is nothing to charge.
		e.failProposal(ctx, proposalID, err.Error(), agent.Result{})
		return
	}

	p, getErr := e.Store.GetProposal(ctx, proposalID)
	if getErr != nil {
		return
	}
	// The clone is a repository like any other: registering it here is what
	// lets a second analysis of the same import add to the first one's history
	// instead of starting over.
	repo, err := e.EnsureRepo(ctx, path)
	if err != nil {
		e.failProposal(ctx, proposalID, err.Error(), agent.Result{})
		return
	}
	p.RepoID = repo.ID
	p.RepoPath = repo.Path
	p.State = store.ProposalDraft
	p.Detected = repo.Detected
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return
	}
	e.notifyProposal()
}

// ConfigureProposal saves the operator's steering and kicks the analysis off.
func (e *Engine) ConfigureProposal(ctx context.Context, proposalID int64,
	focus []string, notes string, maxTasks int, model string) error {

	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return err
	}
	if p.RepoPath == "" {
		return fmt.Errorf("proposal %d has no repository yet", proposalID)
	}
	if maxTasks < 1 || maxTasks > 40 {
		return fmt.Errorf("max tasks must be between 1 and 40, got %d", maxTasks)
	}

	p.Focus = focus
	p.Notes = notes
	p.MaxTasks = maxTasks
	if model != "" {
		p.Model = model
	}
	p.State = store.ProposalAnalysing
	p.ErrMsg = ""
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return err
	}
	e.notifyProposal()

	go e.analyse(context.WithoutCancel(ctx), p.ID, "")
	return nil
}

// RegenerateProposal re-runs the analysis with the operator's feedback and the
// previous list in hand, replacing the task rows.
func (e *Engine) RegenerateProposal(ctx context.Context, proposalID int64, feedback string) error {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return err
	}
	if p.State != store.ProposalReady && p.State != store.ProposalFailed {
		return fmt.Errorf("proposal %d is %s; it is not waiting to be regenerated", proposalID, p.State)
	}

	p.State = store.ProposalAnalysing
	p.ErrMsg = ""
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return err
	}
	e.notifyProposal()

	go e.analyse(context.WithoutCancel(ctx), p.ID, feedback)
	return nil
}

// analyse runs the agent, parses the response, and stores the proposed tasks.
func (e *Engine) analyse(ctx context.Context, proposalID int64, feedback string) {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}

	prompt := AnalysisPrompt(p.Focus, p.Notes, p.Detected, p.MaxTasks)
	if strings.TrimSpace(feedback) != "" {
		prompt += "\n\nA previous attempt produced a list the operator was not " +
			"happy with. Their feedback:\n\n" + strings.TrimSpace(feedback) +
			"\n\nProduce a fresh list that takes it into account."
	}

	// runAnalysis returns what it spent whether or not it succeeded: a failed
	// analysis really did pay for the attempts that failed, and dropping them
	// would make the dashboard's spend figure an under-report of exactly the
	// runs an operator most wants to know the cost of.
	tasks, res, err := e.runAnalysis(ctx, &p, prompt)
	if err != nil {
		e.failProposal(ctx, proposalID, err.Error(), res)
		return
	}

	// Record the spend before anything that can fail below, for the same
	// reason: money already gone has to show up whatever happens next.
	rows := make([]store.ProposalTask, 0, len(tasks))
	for i, t := range tasks {
		rows = append(rows, store.ProposalTask{
			Ord:         i,
			Key:         strings.TrimSpace(t.KeyOrEmpty()),
			Goal:        strings.TrimSpace(t.GoalOrEmpty()),
			Constraints: t.Constraints,
			Verify:      strings.TrimSpace(t.VerifyOrEmpty()),
			Severity:    t.SeverityOrDefault(),
			CostCap:     t.CostCapOrZero(),
			DependsOn:   t.DependsOn,
			Rationale:   strings.TrimSpace(deref(t.Rationale)),
			Evidence:    t.Evidence,
			// Everything starts selected: the operator deselects what they do
			// not want, which is less work than picking nine of twelve.
			Selected: true,
		})
	}
	if err := e.Store.ReplaceProposalTasks(ctx, proposalID, rows); err != nil {
		e.failProposal(ctx, proposalID, err.Error(), res)
		return
	}

	// Re-read rather than reusing p: a regenerate accumulates spend, and the
	// row may have been written by the run that just finished.
	p, err = e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}
	p.State = store.ProposalReady
	p.ErrMsg = ""
	p.CostUSD += res.CostUSD
	p.InputTokens += res.InputTokens
	p.OutputTokens += res.OutputTokens
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return
	}
	e.notifyProposal()
}

// runAnalysis performs the agent invocation and parses its final message.
//
// A response that will not parse gets exactly one stricter re-ask, the same
// shape runCodex uses for an unparseable verdict, and then fails. There is no
// lenient fallback: a partially understood task list is worse than none,
// because the operator would be reviewing a list that quietly lost items.
func (e *Engine) runAnalysis(ctx context.Context, p *store.Proposal, prompt string) ([]agent.ProposedTask, agent.Result, error) {
	var spent agent.Result

	for attempt := 1; attempt <= 2; attempt++ {
		res, err := e.runAnalysisAgent(ctx, p, prompt, attempt)
		if err != nil {
			return nil, spent, err
		}
		spent.CostUSD += res.CostUSD
		spent.InputTokens += res.InputTokens
		spent.OutputTokens += res.OutputTokens

		if res.ErrMsg != "" {
			if agent.IsAuthFailure(res.ErrMsg) {
				e.Pause(fmt.Sprintf("claude is not authenticated: %s", res.ErrMsg))
			}
			// A timeout is the one failure the operator can act on directly,
			// and "step timeout after 30m0s" on its own does not say there is
			// a setting behind it.
			if strings.Contains(res.ErrMsg, "step timeout") {
				return nil, spent, fmt.Errorf(
					"the analysis ran past its %s limit and was stopped. "+
						"Raise analysis_timeout in the config file, or narrow the "+
						"focus and the task budget so there is less to read: %s",
					e.analysisTimeout(), res.ErrMsg)
			}
			return nil, spent, fmt.Errorf("analysis failed: %s", res.ErrMsg)
		}

		tasks, parseErr := agent.ParseProposal([]byte(res.FinalText), p.MaxTasks)
		if parseErr == nil {
			return tasks, spent, nil
		}
		if attempt == 2 {
			return nil, spent, fmt.Errorf("the analysis returned nothing usable: %w", parseErr)
		}
		prompt += "\n\nYour previous response could not be parsed: " +
			parseErr.Error() + "\nReply with the JSON object the schema " +
			"describes and nothing else."
	}
	return nil, spent, fmt.Errorf("analysis produced no result")
}

func (e *Engine) runAnalysisAgent(ctx context.Context, p *store.Proposal, prompt string, attempt int) (agent.Result, error) {
	role, err := e.resolveRole(config.RoleAnalyse)
	if err != nil {
		return agent.Result{ErrMsg: err.Error()}, nil
	}

	runDir := e.proposalDir(p.ID)
	if err := sandbox.EnsureDirs(runDir); err != nil {
		return agent.Result{}, err
	}
	if err := prepareAgentStateIn(runDir, role.Agent); err != nil {
		return agent.Result{}, err
	}

	transcript := filepath.Join(runDir, "analysis.jsonl")
	if p.TranscriptPath != transcript || p.Provider != role.Provider {
		p.TranscriptPath = transcript
		// Recording which provider served the run is what lets its usage be
		// attributed later as subscription-covered or actually metered.
		p.Provider = role.Provider
		if err := e.Store.SaveProposal(ctx, *p); err != nil {
			return agent.Result{}, err
		}
		e.notifyProposal()
	}

	// The proposal's own model wins: the operator may have picked it in the
	// wizard for this analysis, and the role's model is only the default the
	// wizard offered.
	if p.Model != "" {
		role.Model = p.Model
	}
	return role.Runner.Run(ctx, agent.RunSpec{
		Args:           role.args(prompt, "", "", ""),
		Dir:            p.RepoPath,
		TranscriptPath: transcript,
		Timeout:        e.analysisTimeout(),
		Attempt:        attempt,
		Sandbox:        e.Sandbox,
		SandboxSpec:    e.analysisSandboxSpec(p.RepoPath, runDir, role.Agent),
		Env:            e.agentEnv(role),
		// Without this the wizard's live pane would hold whatever the
		// transcript said when the page loaded, for the whole analysis.
		OnEvent: e.progressNotifier(0),
	})
}

// QueueProposal turns the selected rows into real tasks.
//
// It can be called more than once on the same analysis. Queueing three of
// twelve and coming back next week for four more is the normal way to use a
// long proposal, so a row that already produced a task is skipped rather than
// duplicated, and the proposal only reaches "queued" once nothing is left.
//
// Dependencies are resolved by created task ID, not by slug. Submit suffixes a
// slug on collision, so the slug a proposal predicted is not necessarily the
// slug the task gets, and wiring by name would silently attach a dependency to
// the wrong task or to nothing at all.
func (e *Engine) QueueProposal(ctx context.Context, proposalID int64) ([]store.Task, error) {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return nil, err
	}
	if p.State != store.ProposalReady {
		return nil, fmt.Errorf("proposal %d is %s; only a reviewed proposal can be queued", proposalID, p.State)
	}
	rows, err := e.Store.ProposalTasks(ctx, proposalID)
	if err != nil {
		return nil, err
	}

	// Seed the key map with rows queued on an earlier pass. A task queued
	// today that depends on one queued last week must link to the task that
	// already exists, not silently lose the dependency.
	byKey := map[string]int64{}
	var selected []store.ProposalTask
	remaining := 0
	for _, r := range rows {
		if r.CreatedTaskID != 0 {
			byKey[r.Key] = r.CreatedTaskID
			continue
		}
		remaining++
		if r.Selected {
			selected = append(selected, r)
		}
	}
	if len(selected) == 0 {
		if remaining == 0 {
			return nil, fmt.Errorf("every task from this analysis has already been queued")
		}
		return nil, fmt.Errorf("nothing is selected")
	}

	created := make([]store.Task, 0, len(selected))
	for _, r := range selected {
		task, err := e.Submit(ctx, BatchTask{
			Repo:             p.RepoPath,
			Goal:             r.Goal,
			Constraints:      r.Constraints,
			BlockingSeverity: r.Severity,
			Verify:           r.Verify,
			CostCap:          r.CostCap,
		})
		if err != nil {
			return created, fmt.Errorf("queue %q: %w", r.Goal, err)
		}
		byKey[r.Key] = task.ID
		created = append(created, task)

		r.CreatedTaskID = task.ID
		if err := e.Store.SaveProposalTask(ctx, r); err != nil {
			return created, err
		}
	}

	// Dependencies come second, once every ID exists. A dependency on a row
	// the operator deselected is dropped rather than failing the queue: they
	// said they did not want that task, and refusing the whole batch over it
	// would be the tool arguing with them.
	for _, r := range selected {
		var depIDs []int64
		for _, key := range r.DependsOn {
			if id, ok := byKey[strings.TrimSpace(key)]; ok {
				depIDs = append(depIDs, id)
			}
		}
		if len(depIDs) == 0 {
			continue
		}
		if err := e.Store.SetTaskDeps(ctx, byKey[r.Key], depIDs); err != nil {
			return created, err
		}
	}

	// Whatever was not queued goes on the repository's backlog. Before this it
	// existed only inside the proposal, findable if you remembered which
	// analysis it was; now it is a working list item, deduplicated against
	// everything else already known about the repository. Dropped on error:
	// the tasks were created and reporting a backlog failure as a queue
	// failure would suggest they were not.
	//
	// The rows are re-read because the loop above set CreatedTaskID on the
	// ones it queued, and those must not be filed.
	if fresh, err := e.Store.ProposalTasks(ctx, proposalID); err == nil {
		_ = e.recordBacklogProposals(ctx, p, fresh)
	}

	// The analysis stays reviewable while any of its tasks are still
	// unqueued: that is what makes coming back for the rest possible. Only a
	// proposal with nothing left is finished.
	if remaining-len(created) <= 0 {
		p.State = store.ProposalQueued
	}
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return created, err
	}
	e.notifyProposal()
	return created, nil
}

// DiscardProposal throws a proposal away without queueing anything.
//
// The list still goes on the backlog. "Not now" is the usual reason to discard
// an analysis, and it is a different thing from "none of this was worth doing";
// an operator who means the latter dismisses the items, which is one action on
// a list that at least exists.
func (e *Engine) DiscardProposal(ctx context.Context, proposalID int64) error {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return err
	}
	if rows, err := e.Store.ProposalTasks(ctx, proposalID); err == nil {
		_ = e.recordBacklogProposals(ctx, p, rows)
	}
	p.State = store.ProposalDiscarded
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return err
	}
	e.notifyProposal()
	return nil
}

// failProposal parks a proposal with an explanation, still charging it for
// whatever the failed attempts cost.
func (e *Engine) failProposal(ctx context.Context, proposalID int64, msg string, spent agent.Result) {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}
	p.State = store.ProposalFailed
	p.ErrMsg = msg
	p.CostUSD += spent.CostUSD
	p.InputTokens += spent.InputTokens
	p.OutputTokens += spent.OutputTokens
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return
	}
	e.notifyProposal()
}

// proposalDir is where one analysis writes its transcript and per-run agent
// state. It is the only writable path inside the analysis sandbox.
func (e *Engine) proposalDir(proposalID int64) string {
	return filepath.Join(e.Cfg.ProposalsDir(), strconv.FormatInt(proposalID, 10))
}

// notifyProposal tells the dashboard something about a proposal changed.
// Proposals have no task ID, and the SSE client reloads on any event whatever
// its payload, so zero means "something that is not a task".
func (e *Engine) notifyProposal() { e.notify(0) }

// checkRepo is the same validation Submit applies to a submitted repository.
func checkRepo(ctx context.Context, repoPath string) error {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s is not a git repository: %v: %s",
			repoPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// probeRepo describes a repository in one line, cheaply and without an agent.
//
// This exists so the wizard's first screen can show the operator that it
// understood the repository before they pay for an analysis, and so the
// analysis prompt can start from a fact rather than asking the model to
// rediscover the test command.
func probeRepo(ctx context.Context, repoPath string) string {
	var parts []string

	if lang, verify := detectToolchain(repoPath); lang != "" {
		parts = append(parts, lang)
		if verify != "" {
			parts = append(parts, verify)
		}
	}
	if branch, err := worktree.DefaultBranch(ctx, repoPath); err == nil && branch != "" {
		parts = append(parts, "default branch "+branch)
	}
	if n := countTrackedFiles(ctx, repoPath); n > 0 {
		parts = append(parts, fmt.Sprintf("%d tracked files", n))
	}
	return strings.Join(parts, " · ")
}

// toolchains maps a marker file to the language and the command that most
// often proves a change works in that ecosystem. It is a starting point shown
// to the operator and handed to the analysis, not a decision: the analysis is
// told to use the command it actually finds.
var toolchains = []struct{ marker, lang, verify string }{
	{"go.mod", "Go", "go test ./..."},
	{"Cargo.toml", "Rust", "cargo test"},
	{"pyproject.toml", "Python", "pytest"},
	{"setup.py", "Python", "pytest"},
	{"Gemfile", "Ruby", "bundle exec rspec"},
	{"pom.xml", "Java", "mvn -q test"},
	{"build.gradle", "Java", "gradle test"},
	{"package.json", "JavaScript/TypeScript", "npm test"},
}

func detectToolchain(repoPath string) (lang, verify string) {
	for _, t := range toolchains {
		if _, err := os.Stat(filepath.Join(repoPath, t.marker)); err == nil {
			return t.lang, t.verify
		}
	}
	return "", ""
}

func countTrackedFiles(ctx context.Context, repoPath string) int {
	cmd := exec.CommandContext(ctx, "git", "ls-files")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	return len(strings.Fields(strings.TrimSpace(string(out))))
}

// deref reads an optional string field that has already been validated.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
