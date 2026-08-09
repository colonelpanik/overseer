package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"overseer/internal/store"
)

// fakeChat replies with the given text. Same shape as fakeArchitect, with its
// own session id so a test can tell the two apart.
func fakeChat(t *testing.T, reply string) string {
	t.Helper()
	// Backslashes first, then quotes: the reply may itself be JSON, so its own
	// escapes have to survive being embedded in the transcript's JSON.
	escaped := strings.ReplaceAll(reply, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	// printf, not echo: /bin/sh is dash on most Linux, whose echo expands
	// backslash escapes, so a JSON string containing \n would arrive with a
	// real newline in it and fail to parse.
	return writeScript(t, "claude", `
printf '%s\n' '{"type":"system","subtype":"init","session_id":"chat-sess"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"`+escaped+`"}]},"session_id":"chat-sess"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"chat-sess","total_cost_usd":0.20,"usage":{"input_tokens":90,"output_tokens":40}}'
`)
}

func waitForChatTurns(t *testing.T, h *harness, chatID int64, n int) []store.ChatTurn {
	t.Helper()
	var turns []store.ChatTurn
	waitFor(t, "the chat to reach the next turn", func() bool {
		var err error
		turns, err = h.st.ChatTurns(context.Background(), chatID, 0)
		return err == nil && len(turns) >= n
	})
	return turns
}

// openChat registers the harness repository and opens a conversation about it.
func openChat(t *testing.T, h *harness) store.Chat {
	t.Helper()
	chat, err := h.eng.OpenChat(context.Background(), h.repo)
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	return chat
}

func TestAskRecordsBothTurnsAndRemembersTheSession(t *testing.T) {
	// A chat that did not resume would answer every question as if it were the
	// first, which is the difference between a conversation and a search box.
	h := newHarness(t, fakeChat(t, "Because the daemon restarts."), "true")
	ctx := context.Background()
	chat := openChat(t, h)

	if err := h.eng.Ask(ctx, chat.ID, "why does busy come from turn order?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	turns := waitForChatTurns(t, h, chat.ID, 2)

	if turns[0].Speaker != store.SpeakerOperator || !strings.Contains(turns[0].Body, "turn order") {
		t.Errorf("first turn = %s: %q", turns[0].Speaker, turns[0].Body)
	}
	if turns[1].Speaker != store.SpeakerAssistant || !strings.Contains(turns[1].Body, "daemon restarts") {
		t.Errorf("second turn = %s: %q", turns[1].Speaker, turns[1].Body)
	}
	// A chat is used casually and often, so what it costs has to be recorded.
	if turns[1].CostUSD == 0 {
		t.Error("the assistant's turn recorded no usage")
	}

	got, err := h.st.GetChat(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session == "" {
		t.Error("no session recorded; the next question would start over")
	}
}

func TestOpenChatReturnsTheSameConversationEveryTime(t *testing.T) {
	// The overlay posts to one route whether it is the first message or the
	// thousandth, so this has to be idempotent rather than opening a thread
	// per message.
	h := newHarness(t, fakeChat(t, "hello"), "true")
	first := openChat(t, h)
	second := openChat(t, h)
	if first.ID != second.ID {
		t.Errorf("OpenChat gave %d then %d, want the same conversation", first.ID, second.ID)
	}
}

func TestAskRefusesWhileAReplyIsInFlight(t *testing.T) {
	// Two turns interleaved into one session would make the transcript a
	// record of something that did not happen in that order.
	h := newHarness(t, blockingClaude(t), "true")
	ctx := context.Background()
	chat := openChat(t, h)

	if err := h.eng.Ask(ctx, chat.ID, "first"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	waitForChatTurns(t, h, chat.ID, 1)
	if err := h.eng.Ask(ctx, chat.ID, "second"); err == nil {
		t.Error("a second question should be refused while the first is unanswered")
	}
}

func TestAFailedChatTurnKeepsTheConversation(t *testing.T) {
	// Losing a long conversation to one timed-out reply would be the worst
	// failure this surface has. The failure is a turn, not the end of it.
	h := newHarness(t, writeScript(t, "claude", "exit 1"), "true")
	ctx := context.Background()
	chat := openChat(t, h)

	if err := h.eng.Ask(ctx, chat.ID, "why?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	turns := waitForChatTurns(t, h, chat.ID, 2)
	if turns[1].ErrMsg == "" {
		t.Errorf("the failed turn carries no error: %+v", turns[1])
	}
	got, err := h.st.GetChat(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Archived() {
		t.Error("a failed turn must not end the conversation")
	}
	// And the operator can simply ask again: the assistant spoke last, so
	// nothing is in flight.
	if err := h.eng.Ask(ctx, chat.ID, "try again"); err != nil {
		t.Errorf("asking again after a failure: %v", err)
	}
}

func TestALostSessionIsReseededFromTheStoredTurns(t *testing.T) {
	// The architect has this defect: it only tolerates a missing session on the
	// opening turn, so a resume that fails at turn twenty fails identically for
	// ever after. The database holds every turn verbatim precisely so this can
	// recover.
	h := newHarness(t, writeScript(t, "claude", `
for arg in "$@"; do
  if [ "$arg" = "--resume" ]; then
    printf '%s\n' 'session not found' >&2
    exit 1
  fi
done
printf '%s\n' '{"type":"system","subtype":"init","session_id":"fresh-sess"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"carrying on"}]},"session_id":"fresh-sess"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"fresh-sess","total_cost_usd":0.1,"usage":{"input_tokens":10,"output_tokens":5}}'
`), "true")
	ctx := context.Background()
	chat := openChat(t, h)

	// The opening turn passes no --resume, so it succeeds and records a
	// session. The second turn would resume it, and cannot.
	if err := h.eng.Ask(ctx, chat.ID, "first"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	waitForChatTurns(t, h, chat.ID, 2)
	if err := h.eng.Ask(ctx, chat.ID, "second"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	turns := waitForChatTurns(t, h, chat.ID, 4)

	if turns[3].ErrMsg != "" {
		t.Errorf("a lost session should have been re-seeded, not recorded as a failure: %q", turns[3].ErrMsg)
	}
	if !strings.Contains(turns[3].Body, "carrying on") {
		t.Errorf("fourth turn = %q", turns[3].Body)
	}
}

func TestAskRefusesOnAnArchivedRepository(t *testing.T) {
	// Archiving a repository is the operator saying they are done with it.
	// A conversation that kept answering would be spending money on it.
	h := newHarness(t, fakeChat(t, "hello"), "true")
	ctx := context.Background()
	chat := openChat(t, h)

	repo, err := h.st.GetRepo(ctx, chat.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	repo.ArchivedAt = time.Now().UTC()
	if err := h.st.SaveRepo(ctx, repo); err != nil {
		t.Fatalf("SaveRepo: %v", err)
	}
	if err := h.eng.Ask(ctx, chat.ID, "still there?"); err == nil {
		t.Error("a chat about an archived repository should refuse")
	}
}

func TestNewChatArchivesTheOldOneAndStartsFresh(t *testing.T) {
	// The affordance for a thread that has wandered. The old one stays
	// readable: it is a record of decisions, not scratch.
	h := newHarness(t, fakeChat(t, "hello"), "true")
	ctx := context.Background()
	old := openChat(t, h)
	if err := h.eng.Ask(ctx, old.ID, "something"); err != nil {
		t.Fatal(err)
	}
	waitForChatTurns(t, h, old.ID, 2)

	fresh, err := h.eng.NewChat(ctx, old.RepoID)
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if fresh.ID == old.ID {
		t.Fatal("NewChat returned the same conversation")
	}
	if fresh.Session != "" {
		t.Error("a fresh chat must not inherit the old session")
	}

	wasOld, err := h.st.GetChat(ctx, old.ID)
	if err != nil {
		t.Fatalf("the archived conversation should still be readable: %v", err)
	}
	if !wasOld.Archived() {
		t.Error("the old conversation was not archived")
	}
	turns, err := h.st.ChatTurns(ctx, old.ID, 0)
	if err != nil || len(turns) != 2 {
		t.Errorf("archived turns = %d (err %v), want the conversation kept", len(turns), err)
	}
	if err := h.eng.Ask(ctx, old.ID, "hello?"); err == nil {
		t.Error("an archived conversation should refuse new turns")
	}
}

func TestRecoverAnswersAChatLeftWaitingByARestart(t *testing.T) {
	// Busy is derived from the operator having spoken last, so nothing else
	// would ever clear it and the overlay would say "thinking…" for ever.
	h := newHarness(t, fakeChat(t, "hello"), "true")
	ctx := context.Background()
	chat := openChat(t, h)

	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerOperator, Body: "asked just before the restart",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	last, err := h.st.LastChatTurn(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last.Speaker != store.SpeakerAssistant || last.ErrMsg == "" {
		t.Errorf("last turn = %+v, want an error turn clearing the wait", last)
	}
}

func TestTheChatCannotWriteToTheRepository(t *testing.T) {
	// The whole premise is that this reads a tree somebody only asked it to
	// think about. A conversation that could leave a branch, a stash or an edit
	// behind would be a different and much worse feature.
	h := newHarness(t, fakeChat(t, "hello"), "true")
	chat := openChat(t, h)

	spec := h.eng.analysisSandboxSpec(h.repo, h.eng.chatDir(chat.ID), "claude")
	for _, m := range spec.Mounts {
		if m.Write && strings.HasPrefix(h.repo, m.Dest) {
			t.Errorf("the chat has write access to %s", m.Dest)
		}
	}
}

// fakePull answers with a task list. It fails if it is ever given --resume:
// the pull must run in its own session, or its "reply with JSON and nothing
// else" instruction would sit in the conversation's context for ever.
func fakePull(t *testing.T, tasksJSON string) string {
	t.Helper()
	escaped := strings.ReplaceAll(tasksJSON, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return writeScript(t, "claude", `
for arg in "$@"; do
  if [ "$arg" = "--resume" ]; then
    printf '%s\n' 'the pull must not resume the conversation' >&2
    exit 1
  fi
done
printf '%s\n' '{"type":"system","subtype":"init","session_id":"pull-sess"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"`+escaped+`"}]},"session_id":"pull-sess"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"pull-sess","total_cost_usd":0.15,"usage":{"input_tokens":80,"output_tokens":30}}'
`)
}

func oneActionJSON(key, goal string) string {
	return `{"tasks":[{"key":"` + key + `","goal":"` + goal + `","constraints":[],` +
		`"verify":"go test ./...","blocking_severity":"any","cost_cap":null,` +
		`"depends_on":[],"rationale":"agreed in the conversation","evidence":["chat.go:1"]}]}`
}

func waitForProposalState(t *testing.T, h *harness, id int64, want string) store.Proposal {
	t.Helper()
	var p store.Proposal
	waitFor(t, "the pull to reach "+want, func() bool {
		var err error
		p, err = h.st.GetProposal(context.Background(), id)
		return err == nil && p.State == want
	})
	return p
}

// The headline behaviour: a pull produces a reviewable list and the
// conversation carries on.
func TestPullCreatesAReviewableProposalAndLeavesTheChatAlive(t *testing.T) {
	h := newHarness(t, fakePull(t, oneActionJSON("in-flight", "Add an explicit in-flight column")), "true")
	ctx := context.Background()
	chat := openChat(t, h)
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerOperator, Body: "let us add an in-flight column",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "agreed",
	}); err != nil {
		t.Fatal(err)
	}

	p, err := h.eng.PullActions(ctx, chat.ID)
	if err != nil {
		t.Fatalf("PullActions: %v", err)
	}
	if p.Kind != store.ProposalChat {
		t.Errorf("Kind = %q, want %q", p.Kind, store.ProposalChat)
	}
	if p.ChatID != chat.ID {
		t.Errorf("ChatID = %d, want %d", p.ChatID, chat.ID)
	}
	// Created up front and in analysing, so the existing stranded-proposal
	// sweep covers a restart mid-pull with no new code.
	if p.State != store.ProposalAnalysing {
		t.Errorf("State = %q, want analysing at the moment of the request", p.State)
	}

	waitForProposalState(t, h, p.ID, store.ProposalReady)
	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Goal, "in-flight column") {
		t.Fatalf("rows = %+v, want the agreed action", rows)
	}

	// And the conversation is untouched and still usable.
	got, err := h.st.GetChat(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Archived() {
		t.Error("pulling must not end the conversation")
	}
	turns, err := h.st.ChatTurns(ctx, chat.ID, 0)
	if err != nil || len(turns) != 2 {
		t.Errorf("turns = %d (err %v), want the conversation unchanged", len(turns), err)
	}
}

func TestAPullThatFindsNothingLeavesNoEmptyReviewList(t *testing.T) {
	// A conversation that has not decided anything yet has nothing to pull,
	// and that is a normal early state rather than a failure. An empty review
	// list sitting in the wizard would be noise the operator has to dismiss.
	h := newHarness(t, fakePull(t, `{"tasks":[]}`), "true")
	ctx := context.Background()
	chat := openChat(t, h)
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerOperator, Body: "just wondering how this works",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "understood",
	}); err != nil {
		t.Fatal(err)
	}

	p, err := h.eng.PullActions(ctx, chat.ID)
	if err != nil {
		t.Fatalf("PullActions: %v", err)
	}
	done := waitForProposalState(t, h, p.ID, store.ProposalDiscarded)
	if done.ErrMsg == "" {
		t.Error("a pull that found nothing should say so")
	}
	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want none", len(rows))
	}
}

func TestPullingTwiceDoesNotReproposeWhatWasAlreadyFiled(t *testing.T) {
	// The same conversation pulled again will describe the same decisions.
	// Without a deterministic drop the operator reviews a list of work they
	// already queued, which is the fastest way to make them stop using this.
	h := newHarness(t, fakePull(t, oneActionJSON("in-flight", "Add an explicit in-flight column")), "true")
	ctx := context.Background()
	chat := openChat(t, h)
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerOperator, Body: "add an in-flight column",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "understood",
	}); err != nil {
		t.Fatal(err)
	}

	first, err := h.eng.PullActions(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitForProposalState(t, h, first.ID, store.ProposalReady)

	second, err := h.eng.PullActions(ctx, chat.ID)
	if err != nil {
		t.Fatalf("a second pull should be allowed: %v", err)
	}
	done := waitForProposalState(t, h, second.ID, store.ProposalDiscarded)
	if done.ErrMsg == "" {
		t.Error("the second pull should say it found nothing new")
	}
}

func TestPullRefusesWhileAnotherPullIsRunning(t *testing.T) {
	// Two pulls of one conversation would both spend money producing the same
	// list, and the operator would review it twice.
	h := newHarness(t, blockingClaude(t), "true")
	ctx := context.Background()
	chat := openChat(t, h)
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "agreed",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.eng.PullActions(ctx, chat.ID); err != nil {
		t.Fatalf("PullActions: %v", err)
	}
	if _, err := h.eng.PullActions(ctx, chat.ID); err == nil {
		t.Error("a second pull should be refused while the first is running")
	}
}

func TestPullRefusesWhileAReplyIsStillComing(t *testing.T) {
	// Extracting from a half-finished exchange records decisions from a
	// conversation that has not happened yet.
	h := newHarness(t, blockingClaude(t), "true")
	ctx := context.Background()
	chat := openChat(t, h)
	if err := h.eng.Ask(ctx, chat.ID, "what about X?"); err != nil {
		t.Fatal(err)
	}
	waitForChatTurns(t, h, chat.ID, 1)
	if _, err := h.eng.PullActions(ctx, chat.ID); err == nil {
		t.Error("a pull should be refused while a reply is in flight")
	}
}

// The whole premise, end to end: talk, pull, queue real tasks, keep talking,
// pull again without being shown the same work twice.
func TestQueueingAPullMakesRealTasksAndTheChatCarriesOn(t *testing.T) {
	h := newHarness(t, fakePull(t, oneActionJSON("in-flight", "Add an explicit in-flight column")), "true")
	ctx := context.Background()
	chat := openChat(t, h)
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerOperator, Body: "add an in-flight column",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "understood",
	}); err != nil {
		t.Fatal(err)
	}

	p, err := h.eng.PullActions(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitForProposalState(t, h, p.ID, store.ProposalReady)

	tasks, err := h.eng.QueueProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("QueueProposal: %v", err)
	}
	if len(tasks) != 1 || !strings.Contains(tasks[0].Goal, "in-flight column") {
		t.Fatalf("tasks = %+v, want one real task", tasks)
	}

	// The conversation is still there, still live, and still accepts a turn.
	if _, err := h.st.LiveChat(ctx, chat.RepoID); err != nil {
		t.Errorf("the conversation should still be live after queueing: %v", err)
	}
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerOperator, Body: "what about the reload guard?",
	}); err != nil {
		t.Errorf("the conversation should still accept turns: %v", err)
	}
}

func TestAFailedChatPullCannotBeRegeneratedAsAnAnalysis(t *testing.T) {
	// RegenerateProposal accepts ready or failed and runs the analysis prompt.
	// Offered on a chat pull it would quietly start a full repository analysis
	// — different work, different cost, and not what the button says.
	h := newHarness(t, fakeChat(t, "hello"), "true")
	ctx := context.Background()
	chat := openChat(t, h)

	p, err := h.st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalChat, State: store.ProposalFailed,
		ChatID: chat.ID, RepoID: chat.RepoID, RepoPath: h.repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RegenerateProposal(ctx, p.ID, ""); err == nil {
		t.Error("regenerating a chat pull should be refused")
	}
}

func TestAPulledActionCarriesASubjectLikeEveryOtherProposedTask(t *testing.T) {
	// The board lists a task by its subject and falls back to deriving one from
	// the goal. A pull that left the column empty would make the one path that
	// produces tasks from a conversation the only one whose rows never carry
	// the model's own one-line summary.
	tasks := `{"tasks":[{"key":"in-flight","subject":"Add an in_flight column",` +
		`"goal":"Add an explicit in_flight boolean column to architect_turns and read it in architectBusy.",` +
		`"constraints":[],"verify":"go test ./...","blocking_severity":"any","cost_cap":null,` +
		`"depends_on":[],"rationale":"agreed","evidence":["architect.go:106"]}]}`
	h := newHarness(t, fakePull(t, tasks), "true")
	ctx := context.Background()
	chat := openChat(t, h)
	if _, err := h.st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "agreed",
	}); err != nil {
		t.Fatal(err)
	}

	p, err := h.eng.PullActions(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitForProposalState(t, h, p.ID, store.ProposalReady)
	rows, err := h.st.ProposalTasks(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Subject != "Add an in_flight column" {
		t.Errorf("Subject = %q, want the one the model wrote", rows[0].Subject)
	}
}
