package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"overseer/internal/agent"
	"overseer/internal/config"
	"overseer/internal/sandbox"
	"overseer/internal/store"
)

// StartDesign opens a design conversation.
//
// The repository is optional. With one, the architect reads it and its
// questions are grounded in what is actually there; without one, the
// conversation decides what the project is.
//
// newProject says the repository is one just created for this design, so what
// follows is scaffolding rather than a redesign. It cannot be inferred from the
// path being empty: the wizard creates the project first, precisely so the
// architect has somewhere real to work.
func (e *Engine) StartDesign(ctx context.Context, repoRef, brief string, newProject bool) (store.Proposal, error) {
	if strings.TrimSpace(brief) == "" {
		return store.Proposal{}, fmt.Errorf("say what you want built, or changed")
	}

	p := store.Proposal{
		Kind:     store.ProposalCreate,
		State:    store.ProposalDesigning,
		Notes:    strings.TrimSpace(brief),
		MaxTasks: 12,
		Model:    e.Cfg.AnalysisModel,
	}
	if strings.TrimSpace(repoRef) != "" {
		repo, err := e.ResolveRepo(ctx, repoRef)
		if err != nil {
			return store.Proposal{}, err
		}
		if !newProject {
			// A repository that was already there: this is a redesign, and
			// there is nothing to scaffold.
			p.Kind = store.ProposalAnalyse
		}
		p.RepoID = repo.ID
		p.RepoPath = repo.Path
		p.Detected = repo.Detected
	}

	created, err := e.Store.CreateProposal(ctx, p)
	if err != nil {
		return store.Proposal{}, err
	}
	e.notifyProposal()

	// The brief is the operator's first turn, so the conversation reads as one
	// from the top rather than starting with a reply to something invisible.
	if _, err := e.Store.AddArchitectTurn(ctx, store.ArchitectTurn{
		ProposalID: created.ID, Speaker: store.SpeakerOperator, Body: created.Notes,
	}); err != nil {
		return store.Proposal{}, err
	}
	go e.architectTurn(context.WithoutCancel(ctx), created.ID, "")
	return created, nil
}

// Say adds the operator's turn and asks the architect for its reply.
//
// The turn runs in the background for the same reason an analysis does: it
// takes as long as it takes, and the dashboard is a page that reloads on every
// event rather than a request that can block.
func (e *Engine) Say(ctx context.Context, proposalID int64, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return fmt.Errorf("nothing to say")
	}
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return err
	}
	if p.State != store.ProposalDesigning {
		return fmt.Errorf("proposal %d is %s; the design conversation is over", proposalID, p.State)
	}
	// Refuse while a reply is in flight rather than interleaving two turns into
	// one session, which would make the transcript a record of something that
	// did not happen in that order.
	if e.architectBusy(ctx, proposalID) {
		return fmt.Errorf("the architect is still replying")
	}

	if _, err := e.Store.AddArchitectTurn(ctx, store.ArchitectTurn{
		ProposalID: proposalID, Speaker: store.SpeakerOperator, Body: message,
	}); err != nil {
		return err
	}
	e.notifyProposal()

	go e.architectTurn(context.WithoutCancel(ctx), proposalID, message)
	return nil
}

// architectBusy reports whether the last thing said was the operator's, which
// means a reply is still coming.
func (e *Engine) architectBusy(ctx context.Context, proposalID int64) bool {
	turns, err := e.Store.ArchitectTurns(ctx, proposalID)
	if err != nil || len(turns) == 0 {
		return false
	}
	last := turns[len(turns)-1]
	return last.Speaker == store.SpeakerOperator
}

// architectTurn runs one reply and records it.
//
// A failure is recorded as a turn rather than failing the proposal: the
// conversation is still there, and the operator can say something else or try
// again. Losing an hour of design to one timed-out reply would be the worst
// possible failure mode for this surface.
func (e *Engine) architectTurn(ctx context.Context, proposalID int64, message string) {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}

	res, err := e.runArchitect(ctx, &p, e.architectPrompt(ctx, p, message, false), "architect")
	// A resume that fails is recoverable, and has to be. Without this a session
	// lost at turn twenty made every turn after it fail identically, for ever,
	// and an hour of design became unusable with no way back. The turns are in
	// the database precisely so the conversation can be started again.
	if p.ArchitectSession != "" && (err != nil || res.ErrMsg != "") {
		p.ArchitectSession = ""
		if saveErr := e.Store.SaveProposal(ctx, p); saveErr == nil {
			res, err = e.runArchitect(ctx, &p, e.architectPrompt(ctx, p, message, true), "architect")
		}
	}
	if err != nil {
		e.recordArchitectFailure(ctx, proposalID, err.Error())
		return
	}
	if res.ErrMsg != "" {
		if agent.IsAuthFailure(res.ErrMsg) {
			e.Pause(fmt.Sprintf("the architect is not authenticated: %s", res.ErrMsg))
		}
		e.recordArchitectFailure(ctx, proposalID, res.ErrMsg)
		return
	}

	body := strings.TrimSpace(res.FinalText)
	if body == "" {
		e.recordArchitectFailure(ctx, proposalID, "the architect replied with nothing")
		return
	}
	if _, err := e.Store.AddArchitectTurn(ctx, store.ArchitectTurn{
		ProposalID: proposalID, Speaker: store.SpeakerArchitect, Body: body,
		CostUSD: res.CostUSD, InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
	}); err != nil {
		return
	}
	e.notifyProposal()
}

// architectPrompt builds one turn's prompt.
//
// A resumed session gets the bare message: everything else is already in the
// agent's own context. The opening turn, or one re-seeding a session that could
// not be resumed, gets the framing — and in the re-seed case the transcript, so
// the conversation carries on rather than starting over.
func (e *Engine) architectPrompt(ctx context.Context, p store.Proposal, message string, reseed bool) string {
	if p.ArchitectSession != "" && message != "" && !reseed {
		return message
	}
	var conversation string
	if reseed {
		turns, err := e.Store.ArchitectTurns(ctx, p.ID)
		if err == nil {
			conversation = renderConversation(architectSpoken(turns))
		}
	}
	prompt := ArchitectPrompt(p.Notes, p.RepoPath != "", conversation)
	if message != "" {
		prompt += "\n\nTHEY SAID:\n" + message
	}
	return prompt
}

func (e *Engine) recordArchitectFailure(ctx context.Context, proposalID int64, msg string) {
	if _, err := e.Store.AddArchitectTurn(ctx, store.ArchitectTurn{
		ProposalID: proposalID, Speaker: store.SpeakerArchitect,
		Body:   "I could not reply: " + msg,
		ErrMsg: msg,
	}); err != nil {
		return
	}
	e.notifyProposal()
}

// Accept ends the conversation: the architect writes the design down and
// breaks it into tasks.
func (e *Engine) Accept(ctx context.Context, proposalID int64) error {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return err
	}
	if p.State != store.ProposalDesigning {
		return fmt.Errorf("proposal %d is %s, not being designed", proposalID, p.State)
	}
	if e.architectBusy(ctx, proposalID) {
		return fmt.Errorf("the architect is still replying")
	}

	p.State = store.ProposalAnalysing
	p.ErrMsg = ""
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return err
	}
	e.notifyProposal()

	go e.accept(context.WithoutCancel(ctx), proposalID)
	return nil
}

func (e *Engine) accept(ctx context.Context, proposalID int64) {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}

	prompt := ArchitectAcceptPrompt(p.RepoPath != "", p.MaxTasks)
	design, tasks, spent, err := e.acceptOnce(ctx, &p, prompt)
	if err != nil {
		e.failProposal(ctx, proposalID, err.Error(), spent)
		return
	}

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
			Selected:    true,
		})
	}
	if err := e.Store.ReplaceProposalTasks(ctx, proposalID, rows); err != nil {
		e.failProposal(ctx, proposalID, err.Error(), spent)
		return
	}

	p, err = e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}
	p.Design = design
	p.CostUSD += spent.CostUSD
	p.InputTokens += spent.InputTokens
	p.OutputTokens += spent.OutputTokens
	p.ErrMsg = ""
	// A new project is not reviewable until it exists: scaffolding comes next,
	// and only then is there something to queue tasks against. A redesign of an
	// existing repository has everything it needs already.
	if p.Kind == store.ProposalCreate {
		p.State = store.ProposalScaffolding
	} else {
		p.State = store.ProposalReady
	}
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return
	}
	e.notifyProposal()

	if p.Kind == store.ProposalCreate {
		go e.scaffold(context.WithoutCancel(ctx), p.ID)
	}
}

// acceptOnce asks for the design and the task list, with one stricter re-ask.
//
// The same shape runAnalysis uses, and for the same reason: a partially
// understood answer is worse than none, because the operator would be reviewing
// a list that quietly lost items.
func (e *Engine) acceptOnce(ctx context.Context, p *store.Proposal, prompt string) (string, []agent.ProposedTask, agent.Result, error) {
	var spent agent.Result

	for attempt := 1; attempt <= 2; attempt++ {
		res, err := e.runArchitect(ctx, p, prompt, "accept")
		if err != nil {
			return "", nil, spent, err
		}
		spent.CostUSD += res.CostUSD
		spent.InputTokens += res.InputTokens
		spent.OutputTokens += res.OutputTokens

		if res.ErrMsg != "" {
			if agent.IsAuthFailure(res.ErrMsg) {
				e.Pause(fmt.Sprintf("the architect is not authenticated: %s", res.ErrMsg))
			}
			return "", nil, spent, fmt.Errorf("the architect could not finish: %s", res.ErrMsg)
		}

		design, tasks, parseErr := agent.ParseDesign([]byte(res.FinalText), p.MaxTasks)
		if parseErr == nil {
			return design, tasks, spent, nil
		}
		if attempt == 2 {
			return "", nil, spent, fmt.Errorf("the architect returned nothing usable: %w", parseErr)
		}
		prompt += "\n\nYour previous response could not be parsed: " + parseErr.Error() +
			"\nReply with the JSON object described above and nothing else."
	}
	return "", nil, spent, fmt.Errorf("the architect produced no result")
}

// runArchitect performs one architect invocation, resuming the conversation.
//
// The sandbox is the analysis one: the repository read-only, the proposal's own
// run directory the only writable path. A design conversation about an existing
// repository must not be able to leave a branch, a stash or an edit behind in a
// tree the operator only asked it to think about.
func (e *Engine) runArchitect(ctx context.Context, p *store.Proposal, prompt, label string) (agent.Result, error) {
	role, err := e.resolveRole(config.RoleArchitect)
	if err != nil {
		return agent.Result{ErrMsg: err.Error()}, nil
	}
	if p.Model != "" && label == "accept" {
		// The wizard's model choice applies to the final structured turn the
		// same way it applies to an analysis.
		role.Model = p.Model
	}

	runDir := e.proposalDir(p.ID)
	if err := sandbox.EnsureDirs(runDir); err != nil {
		return agent.Result{}, err
	}
	if err := prepareAgentStateIn(runDir, role.Agent); err != nil {
		return agent.Result{}, err
	}

	// One transcript for the whole conversation. It is opened append-only, so
	// every turn stacks in the order it happened — which is what makes it
	// readable as a record rather than a series of unrelated runs.
	transcript := filepath.Join(runDir, "architect.jsonl")

	// Without a repository there is nowhere to run: an empty per-proposal
	// directory stands in, so the agent has a working directory that exists
	// and contains nothing to mislead it.
	dir := p.RepoPath
	spec := e.analysisSandboxSpec(p.RepoPath, runDir, role.Agent)
	if dir == "" {
		dir = filepath.Join(runDir, "empty")
		if err := sandbox.EnsureDirs(dir); err != nil {
			return agent.Result{}, err
		}
		spec = e.analysisSandboxSpec(dir, runDir, role.Agent)
	}

	res, err := role.Runner.Run(ctx, agent.RunSpec{
		Args:           role.args(prompt, p.ArchitectSession, "", ""),
		Dir:            dir,
		TranscriptPath: transcript,
		Timeout:        e.analysisTimeout(),
		Attempt:        1,
		Sandbox:        e.Sandbox,
		SandboxSpec:    spec,
		Env:            e.agentEnv(role),
		// Without this the conversation would sit still until the whole reply
		// landed, which for a design turn is a long time to look at nothing.
		OnEvent: e.progressNotifier(0),
	})
	if err != nil {
		return res, err
	}

	// Remember the session so the next turn continues this one rather than
	// starting over. Recorded even on a failed turn: the session exists either
	// way, and losing it would silently restart the conversation.
	if res.SessionID != "" && res.SessionID != p.ArchitectSession {
		p.ArchitectSession = res.SessionID
		p.TranscriptPath = transcript
		if err := e.Store.SaveProposal(ctx, *p); err != nil {
			return res, err
		}
	}
	return res, nil
}
