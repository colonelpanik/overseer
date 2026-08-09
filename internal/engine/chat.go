package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"overseer/internal/agent"
	"overseer/internal/config"
	"overseer/internal/sandbox"
	"overseer/internal/store"
)

// chatTail bounds how much of a conversation is replayed into a prompt.
//
// Long enough that no real conversation loses its thread, short enough that a
// chat alive for months cannot grow one prompt without limit. It applies to
// both places a transcript is rendered: re-seeding a lost session, and pulling
// the actions out.
const chatTail = 200

// OpenChat returns the repository's live conversation, opening one if there is
// none.
//
// Idempotent on purpose: the overlay posts to one route whether this is the
// first message or the thousandth, and there is no chat row until somebody
// actually says something.
func (e *Engine) OpenChat(ctx context.Context, repoRef string) (store.Chat, error) {
	repo, err := e.ResolveRepo(ctx, repoRef)
	if err != nil {
		return store.Chat{}, err
	}
	if repo.Archived() {
		return store.Chat{}, fmt.Errorf("repository %s is archived", repo.Slug)
	}

	chat, err := e.Store.LiveChat(ctx, repo.ID)
	if err == nil {
		return chat, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Chat{}, err
	}
	return e.Store.CreateChat(ctx, repo.ID)
}

// Ask adds the operator's turn and asks for a reply.
//
// The turn runs in the background for the same reason a design turn does: it
// takes as long as it takes, and the dashboard is a page that reloads on every
// event rather than a request that can block.
func (e *Engine) Ask(ctx context.Context, chatID int64, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return errors.New("nothing to ask")
	}
	chat, err := e.Store.GetChat(ctx, chatID)
	if err != nil {
		return err
	}
	if chat.Archived() {
		return fmt.Errorf("chat %d is archived; start a new one", chatID)
	}
	repo, err := e.Store.GetRepo(ctx, chat.RepoID)
	if err != nil {
		return err
	}
	// Archiving a repository is the operator saying they are done with it.
	// A conversation that kept answering would be spending money on it.
	if repo.Archived() {
		return fmt.Errorf("repository %s is archived", repo.Slug)
	}
	// Refuse while a reply is in flight rather than interleaving two turns into
	// one session, which would make the transcript a record of something that
	// did not happen in that order.
	if e.chatBusy(ctx, chatID) {
		return errors.New("still answering the last question")
	}

	if _, err := e.Store.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chatID, Speaker: store.SpeakerOperator, Body: message,
	}); err != nil {
		return err
	}
	e.notifyChat()

	go e.chatTurn(context.WithoutCancel(ctx), chatID, message)
	return nil
}

// NewChat retires a repository's conversation and opens a fresh one.
//
// The affordance for a thread that has wandered somewhere it will not come
// back from. The old conversation stays readable: it is a record of what was
// decided, not scratch.
func (e *Engine) NewChat(ctx context.Context, repoID int64) (store.Chat, error) {
	repo, err := e.Store.GetRepo(ctx, repoID)
	if err != nil {
		return store.Chat{}, err
	}
	if repo.Archived() {
		return store.Chat{}, fmt.Errorf("repository %s is archived", repo.Slug)
	}

	switch chat, err := e.Store.LiveChat(ctx, repoID); {
	case err == nil:
		if err := e.ArchiveChat(ctx, chat.ID); err != nil {
			return store.Chat{}, err
		}
	case !errors.Is(err, store.ErrNotFound):
		return store.Chat{}, err
	}

	fresh, err := e.Store.CreateChat(ctx, repoID)
	if err != nil {
		return store.Chat{}, err
	}
	e.notifyChat()
	return fresh, nil
}

// ArchiveChat retires one conversation.
func (e *Engine) ArchiveChat(ctx context.Context, chatID int64) error {
	chat, err := e.Store.GetChat(ctx, chatID)
	if err != nil {
		return err
	}
	if chat.Archived() {
		return nil
	}
	chat.ArchivedAt = time.Now().UTC()
	if err := e.Store.SaveChat(ctx, chat); err != nil {
		return err
	}
	e.notifyChat()
	return nil
}

// chatBusy reports whether the last thing said was the operator's, which means
// a reply is still coming.
//
// Derived rather than held in memory, for the reason every other busy check in
// this daemon is: a flag in a map dies with the process, and the conversation
// does not.
func (e *Engine) chatBusy(ctx context.Context, chatID int64) bool {
	last, err := e.Store.LastChatTurn(ctx, chatID)
	if err != nil {
		return false
	}
	return last.Speaker == store.SpeakerOperator
}

// chatTurn runs one reply and records it.
//
// A failure is recorded as a turn rather than ending the conversation. Losing
// months of context to one timed-out reply would be the worst failure this
// surface has.
func (e *Engine) chatTurn(ctx context.Context, chatID int64, message string) {
	chat, err := e.Store.GetChat(ctx, chatID)
	if err != nil {
		return
	}
	repo, err := e.Store.GetRepo(ctx, chat.RepoID)
	if err != nil {
		e.recordChatFailure(ctx, chatID, err.Error())
		return
	}

	res, err := e.runChat(ctx, &chat, repo, e.chatPrompt(ctx, chat, repo, message, false))
	// A resume that fails is recoverable, and must be: the architect only
	// tolerates a missing session on its opening turn, so a session lost at
	// turn twenty makes every turn after it fail identically for ever. The
	// database holds every turn verbatim precisely so this can start again.
	if chat.Session != "" && (err != nil || res.ErrMsg != "") {
		chat.Session = ""
		if saveErr := e.Store.SaveChat(ctx, chat); saveErr == nil {
			res, err = e.runChat(ctx, &chat, repo,
				e.chatPrompt(ctx, chat, repo, message, true))
		}
	}

	if err != nil {
		e.recordChatFailure(ctx, chatID, err.Error())
		return
	}
	if res.ErrMsg != "" {
		if agent.IsAuthFailure(res.ErrMsg) {
			e.Pause(fmt.Sprintf("the chat is not authenticated: %s", res.ErrMsg))
		}
		e.recordChatFailure(ctx, chatID, res.ErrMsg)
		return
	}

	body := strings.TrimSpace(res.FinalText)
	if body == "" {
		e.recordChatFailure(ctx, chatID, "the chat replied with nothing")
		return
	}
	if _, err := e.Store.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chatID, Speaker: store.SpeakerAssistant, Body: body,
		CostUSD: res.CostUSD, InputTokens: res.InputTokens,
		OutputTokens: res.OutputTokens, Provider: e.chatProvider(),
	}); err != nil {
		return
	}
	e.notifyChat()
}

// chatPrompt builds one turn's prompt.
//
// A resumed session gets the bare message: everything else is already in the
// agent's own context. An opening turn, or one re-seeding a session that could
// not be resumed, gets the framing and the transcript.
func (e *Engine) chatPrompt(ctx context.Context, chat store.Chat, repo store.Repo, message string, reseed bool) string {
	if chat.Session != "" && !reseed {
		return message
	}
	var conversation string
	if reseed {
		turns, err := e.Store.ChatTurns(ctx, chat.ID, chatTail)
		if err == nil {
			conversation = renderConversation(turns)
		}
	}
	prompt := ChatPrompt(repo.Detected, conversation)
	if message != "" {
		prompt += "\n\nTHEY ASKED:\n" + message
	}
	return prompt
}

func (e *Engine) recordChatFailure(ctx context.Context, chatID int64, msg string) {
	if _, err := e.Store.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chatID, Speaker: store.SpeakerAssistant,
		Body:   "I could not reply: " + msg,
		ErrMsg: msg,
	}); err != nil {
		return
	}
	e.notifyChat()
}

// runChat performs one chat invocation, resuming the conversation.
//
// The sandbox is the analysis one: the repository read-only, the chat's own
// run directory the only writable path. A conversation about a repository must
// not be able to leave a branch, a stash or an edit behind in a tree the
// operator only asked it to think about.
func (e *Engine) runChat(ctx context.Context, chat *store.Chat, repo store.Repo, prompt string) (agent.Result, error) {
	role, err := e.resolveRole(config.RoleChat)
	if err != nil {
		return agent.Result{ErrMsg: err.Error()}, nil
	}
	// bubblewrap aborts on a missing --bind source, and "bwrap: Can't find
	// source path" reads to an operator like a sandbox bug rather than a
	// repository that has been moved or deleted underneath them.
	if err := dirExists(repo.Path); err != nil {
		return agent.Result{ErrMsg: err.Error()}, nil
	}

	runDir := e.chatDir(chat.ID)
	if err := sandbox.EnsureDirs(runDir); err != nil {
		return agent.Result{}, err
	}
	if err := prepareAgentStateIn(runDir, role.Agent); err != nil {
		return agent.Result{}, err
	}

	// One transcript for the whole conversation, opened append-only, so every
	// turn stacks in the order it happened.
	transcript := filepath.Join(runDir, "chat.jsonl")

	res, err := role.Runner.Run(ctx, agent.RunSpec{
		Args:           role.args(prompt, chat.Session, "", ""),
		Dir:            repo.Path,
		TranscriptPath: transcript,
		Timeout:        e.analysisTimeout(),
		Attempt:        1,
		Sandbox:        e.Sandbox,
		SandboxSpec:    e.analysisSandboxSpec(repo.Path, runDir, role.Agent),
		Env:            e.agentEnv(role),
		// Without this the overlay would sit still until the whole reply
		// landed, which for a question about a large repository is a long time
		// to look at nothing.
		OnEvent: e.progressNotifier(0),
	})
	if err != nil {
		return res, err
	}

	// Remember the session so the next question continues this conversation
	// rather than starting over. Recorded even on a failed turn: the session
	// exists either way, and losing it would silently restart the chat.
	if res.SessionID != "" && res.SessionID != chat.Session {
		chat.Session = res.SessionID
		if err := e.Store.SaveChat(ctx, *chat); err != nil {
			return res, err
		}
	}
	return res, nil
}

// chatProvider names the provider serving chat turns, for the accounting split.
// An unresolvable role has already failed the turn, so the empty string here
// only ever attaches to a turn that recorded no cost either.
func (e *Engine) chatProvider() string {
	role, err := e.resolveRole(config.RoleChat)
	if err != nil {
		return ""
	}
	return role.Provider
}

// renderConversation writes turns out as a transcript a model can read.
//
// Deliberately plain: two labels and the text. Anything more decorative would
// be indistinguishable, to the model, from something the operator wrote.
func renderConversation(turns []store.ChatTurn) string {
	var b strings.Builder
	for _, t := range turns {
		who := "them"
		if t.Speaker == store.SpeakerOperator {
			who = "developer"
		}
		// A turn that failed records what went wrong, which is a fact about the
		// daemon rather than anything either of them said.
		if t.ErrMsg != "" {
			continue
		}
		b.WriteString(who)
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(t.Body))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// chatDir is where one conversation writes its transcript and per-run agent
// state. It is the only writable path inside the chat's sandbox.
func (e *Engine) chatDir(chatID int64) string {
	return filepath.Join(e.Cfg.ChatsDir(), strconv.FormatInt(chatID, 10))
}

// ChatTranscriptPath is where a conversation's raw agent events are, for the
// live pane. Derived rather than stored: it is a pure function of the id.
func (e *Engine) ChatTranscriptPath(chatID int64) string {
	return filepath.Join(e.chatDir(chatID), "chat.jsonl")
}

// dirExists reports a missing or unusable repository path as a plain error.
func dirExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("repository path %s is not readable: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repository path %s is not a directory", path)
	}
	return nil
}

// notifyChat tells the dashboard something about a conversation changed. Chats
// have no task ID, and the SSE client reloads on any event whatever its
// payload, so zero means "something that is not a task".
func (e *Engine) notifyChat() { e.notify(0) }

// PullActions turns what the conversation decided into a reviewable task list.
//
// The proposal is created up front, in analysing, rather than after the agent
// answers. That buys three things with no new code: the existing stranded
// sweep parks it if the daemon restarts mid-pull, failProposal records what a
// failed attempt cost, and "is a pull already running" becomes a fact in the
// database rather than a lock that dies with the process.
//
// The conversation is untouched. Pulling is not the end of it.
func (e *Engine) PullActions(ctx context.Context, chatID int64) (store.Proposal, error) {
	chat, err := e.Store.GetChat(ctx, chatID)
	if err != nil {
		return store.Proposal{}, err
	}
	repo, err := e.Store.GetRepo(ctx, chat.RepoID)
	if err != nil {
		return store.Proposal{}, err
	}
	if repo.Archived() {
		return store.Proposal{}, fmt.Errorf("repository %s is archived", repo.Slug)
	}
	// Extracting from a half-finished exchange would record decisions from a
	// conversation that has not happened yet.
	if e.chatBusy(ctx, chatID) {
		return store.Proposal{}, errors.New("still answering the last question")
	}
	busy, err := e.Store.ChatPullInFlight(ctx, chatID)
	if err != nil {
		return store.Proposal{}, err
	}
	if busy {
		return store.Proposal{}, errors.New("already pulling the actions out of this conversation")
	}

	p, err := e.Store.CreateProposal(ctx, store.Proposal{
		Kind:     store.ProposalChat,
		State:    store.ProposalAnalysing,
		ChatID:   chatID,
		RepoID:   repo.ID,
		RepoPath: repo.Path,
		Detected: repo.Detected,
		MaxTasks: 12,
		Model:    e.Cfg.AnalysisModel,
		Notes:    "pulled from the chat about " + repo.Slug,
	})
	if err != nil {
		return store.Proposal{}, err
	}
	e.notifyProposal()

	go e.pullActions(context.WithoutCancel(ctx), chatID, p.ID)
	return p, nil
}

func (e *Engine) pullActions(ctx context.Context, chatID, proposalID int64) {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}
	turns, err := e.Store.ChatTurns(ctx, chatID, chatTail)
	if err != nil {
		e.failProposal(ctx, proposalID, err.Error(), agent.Result{})
		return
	}
	filed, err := e.Store.ChatPulledGoals(ctx, chatID, 50)
	if err != nil {
		e.failProposal(ctx, proposalID, err.Error(), agent.Result{})
		return
	}

	prompt := PullActionsPrompt(renderConversation(turns), p.Detected, filed, p.MaxTasks)
	tasks, spent, err := e.runPull(ctx, &p, prompt)
	if err != nil {
		e.failProposal(ctx, proposalID, err.Error(), spent)
		return
	}

	// Validation has already run over the model's own list, so depends_on is
	// known to point backwards only. Dropping happens after that, so the graph
	// it checked is the one the model actually proposed.
	tasks = dropAlreadyFiled(tasks, filed)

	if len(tasks) == 0 {
		// Not a failure. Either nothing has been decided yet, or everything
		// that has is already filed — and both are ordinary states for a
		// conversation that is still going. Discarded rather than a state of
		// its own: every switch in the dashboard would otherwise have to learn
		// one, and this way it stays in the history carrying what it cost.
		e.finishEmptyPull(ctx, proposalID, spent)
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
	p.State = store.ProposalReady
	p.ErrMsg = ""
	p.CostUSD += spent.CostUSD
	p.InputTokens += spent.InputTokens
	p.OutputTokens += spent.OutputTokens
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return
	}
	e.notifyProposal()
}

// finishEmptyPull records a pull that correctly found nothing to do.
func (e *Engine) finishEmptyPull(ctx context.Context, proposalID int64, spent agent.Result) {
	p, err := e.Store.GetProposal(ctx, proposalID)
	if err != nil {
		return
	}
	p.State = store.ProposalDiscarded
	p.ErrMsg = "nothing new has been decided in this conversation yet"
	p.CostUSD += spent.CostUSD
	p.InputTokens += spent.InputTokens
	p.OutputTokens += spent.OutputTokens
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return
	}
	e.notifyProposal()
}

// runPull asks for the task list, with one stricter re-ask.
//
// It runs in a FRESH session — never resuming the chat — for three reasons.
// Resuming would append "reply with JSON and nothing else" to the
// conversation's own context permanently, and every later reply would be
// answering with that still in it. Two claude processes would be writing one
// session file if the operator asked something while a pull ran. And rendering
// the turns from the database instead means a pull works on a conversation
// whose session is long gone, and is reproducible.
//
// The same shape runAnalysis uses, and for the same reason: a partially
// understood answer is worse than none, because the operator would be
// reviewing a list that quietly lost items.
func (e *Engine) runPull(ctx context.Context, p *store.Proposal, prompt string) ([]agent.ProposedTask, agent.Result, error) {
	var spent agent.Result

	role, err := e.resolveRole(config.RoleChat)
	if err != nil {
		return nil, spent, err
	}
	if p.Model != "" {
		role.Model = p.Model
	}
	if err := dirExists(p.RepoPath); err != nil {
		return nil, spent, err
	}

	// The pull's own run directory, not the chat's, so it cannot disturb the
	// conversation's agent state while a question is in flight.
	runDir := e.proposalDir(p.ID)
	if err := sandbox.EnsureDirs(runDir); err != nil {
		return nil, spent, err
	}
	if err := prepareAgentStateIn(runDir, role.Agent); err != nil {
		return nil, spent, err
	}
	transcript := filepath.Join(runDir, "pull.jsonl")

	for attempt := 1; attempt <= 2; attempt++ {
		res, err := role.Runner.Run(ctx, agent.RunSpec{
			Args:           role.args(prompt, "", "", ""),
			Dir:            p.RepoPath,
			TranscriptPath: transcript,
			Timeout:        e.analysisTimeout(),
			Attempt:        attempt,
			Sandbox:        e.Sandbox,
			SandboxSpec:    e.analysisSandboxSpec(p.RepoPath, runDir, role.Agent),
			Env:            e.agentEnv(role),
			OnEvent:        e.progressNotifier(0),
		})
		if err != nil {
			return nil, spent, err
		}
		spent.CostUSD += res.CostUSD
		spent.InputTokens += res.InputTokens
		spent.OutputTokens += res.OutputTokens

		if p.Provider != role.Provider {
			p.Provider = role.Provider
			p.TranscriptPath = transcript
			if err := e.Store.SaveProposal(ctx, *p); err != nil {
				return nil, spent, err
			}
		}

		if res.ErrMsg != "" {
			if agent.IsAuthFailure(res.ErrMsg) {
				e.Pause(fmt.Sprintf("the chat is not authenticated: %s", res.ErrMsg))
			}
			return nil, spent, fmt.Errorf("the pull could not finish: %s", res.ErrMsg)
		}

		tasks, parseErr := agent.ParseActions([]byte(res.FinalText), p.MaxTasks)
		if parseErr == nil {
			return tasks, spent, nil
		}
		if attempt == 2 {
			return nil, spent, fmt.Errorf("the pull returned nothing usable: %w", parseErr)
		}
		prompt += "\n\nYour previous response could not be parsed: " + parseErr.Error() +
			"\nReply with the JSON object described above and nothing else."
	}
	return nil, spent, errors.New("the pull produced no result")
}

// dropAlreadyFiled removes work this conversation has produced before.
//
// Fingerprinted the same way the backlog collapses repeats, so "already filed"
// means the same thing on both surfaces. Survivors then lose any depends_on
// naming something that was dropped: QueueProposal would discard those
// silently, and a review list that shows a dependency it will not honour is
// worse than one that never claimed it.
func dropAlreadyFiled(tasks []agent.ProposedTask, filed []string) []agent.ProposedTask {
	if len(filed) == 0 {
		return tasks
	}
	seen := make(map[string]bool, len(filed))
	for _, goal := range filed {
		seen[store.Fingerprint(goal)] = true
	}

	kept := make([]agent.ProposedTask, 0, len(tasks))
	keys := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		if seen[store.Fingerprint(t.GoalOrEmpty())] {
			continue
		}
		keys[strings.TrimSpace(t.KeyOrEmpty())] = true
		kept = append(kept, t)
	}
	for i := range kept {
		deps := kept[i].DependsOn[:0:0]
		for _, dep := range kept[i].DependsOn {
			if keys[strings.TrimSpace(dep)] {
				deps = append(deps, dep)
			}
		}
		kept[i].DependsOn = deps
	}
	return kept
}
