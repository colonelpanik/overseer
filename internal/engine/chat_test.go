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
