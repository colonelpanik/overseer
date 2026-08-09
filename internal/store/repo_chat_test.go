package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// chatFixture is a repository with a live chat against it, which every test
// here needs before it can say anything interesting.
func chatFixture(t *testing.T, s *Store) (Repo, Chat) {
	t.Helper()
	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, Repo{Path: "/src/widget"})
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	chat, err := s.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return repo, chat
}

func say(t *testing.T, s *Store, chatID int64, speaker, body string, cost float64) ChatTurn {
	t.Helper()
	turn, err := s.AddChatTurn(context.Background(), ChatTurn{
		ChatID: chatID, Speaker: speaker, Body: body, CostUSD: cost,
	})
	if err != nil {
		t.Fatalf("AddChatTurn: %v", err)
	}
	return turn
}

func TestChatTurnsComeBackInTheOrderTheyWereSaid(t *testing.T) {
	// A conversation read back out of order is a record of something that did
	// not happen, which is the one thing a transcript must never be.
	s := newTestStore(t)
	ctx := context.Background()
	_, chat := chatFixture(t, s)

	say(t, s, chat.ID, SpeakerOperator, "why does this infer busy from turn order?", 0)
	say(t, s, chat.ID, SpeakerAssistant, "because the daemon restarts", 0.25)
	say(t, s, chat.ID, SpeakerOperator, "makes sense", 0)

	turns, err := s.ChatTurns(ctx, chat.ID, 0)
	if err != nil {
		t.Fatalf("ChatTurns: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(turns))
	}
	if turns[0].Body != "why does this infer busy from turn order?" ||
		turns[2].Body != "makes sense" {
		t.Errorf("turns out of order: %q ... %q", turns[0].Body, turns[2].Body)
	}
	if turns[1].Speaker != SpeakerAssistant {
		t.Errorf("speaker = %q, want %q", turns[1].Speaker, SpeakerAssistant)
	}

	spend, err := s.ChatSpend(ctx, chat.ID)
	if err != nil {
		t.Fatalf("ChatSpend: %v", err)
	}
	if spend != 0.25 {
		t.Errorf("spend = %v, want 0.25", spend)
	}
}

func TestChatTurnsReturnsTheNewestWhenItIsBounded(t *testing.T) {
	// The overlay renders on every state event, so it asks for a tail rather
	// than a conversation that has been going for months. The tail must still
	// read forwards, and must be the end of the conversation rather than its
	// beginning.
	s := newTestStore(t)
	_, chat := chatFixture(t, s)
	for _, body := range []string{"one", "two", "three", "four"} {
		say(t, s, chat.ID, SpeakerOperator, body, 0)
	}

	turns, err := s.ChatTurns(context.Background(), chat.ID, 2)
	if err != nil {
		t.Fatalf("ChatTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}
	if turns[0].Body != "three" || turns[1].Body != "four" {
		t.Errorf("tail = %q, %q; want three, four", turns[0].Body, turns[1].Body)
	}

	n, err := s.CountChatTurns(context.Background(), chat.ID)
	if err != nil {
		t.Fatalf("CountChatTurns: %v", err)
	}
	if n != 4 {
		t.Errorf("count = %d, want 4", n)
	}
}

func TestOnlyOneLiveChatPerRepository(t *testing.T) {
	// The overlay can only show one conversation. Two clicks on "chat" would
	// otherwise open a second one against the same checkout and silently hide
	// whichever lost, so the database refuses rather than the handler.
	s := newTestStore(t)
	ctx := context.Background()
	repo, first := chatFixture(t, s)

	if _, err := s.CreateChat(ctx, repo.ID); err == nil {
		t.Fatal("a second live chat should be refused")
	}

	first.ArchivedAt = time.Now().UTC()
	if err := s.SaveChat(ctx, first); err != nil {
		t.Fatalf("SaveChat: %v", err)
	}
	second, err := s.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatalf("a fresh chat should open once the old one is archived: %v", err)
	}

	live, err := s.LiveChat(ctx, repo.ID)
	if err != nil {
		t.Fatalf("LiveChat: %v", err)
	}
	if live.ID != second.ID {
		t.Errorf("live chat = %d, want the fresh one %d", live.ID, second.ID)
	}
}

func TestLiveChatSaysSoWhenARepositoryHasNeverBeenTalkedTo(t *testing.T) {
	// The render path calls this on every dashboard load. It must report the
	// absence rather than create a row, or looking at the overlay would open a
	// conversation nobody asked for.
	s := newTestStore(t)
	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, Repo{Path: "/src/quiet"})
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if _, err := s.LiveChat(ctx, repo.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestChatsAndTheirTurnsGoWithTheRepository(t *testing.T) {
	// A conversation about a repository that is gone cannot be resumed and
	// none of its claims can be checked against anything.
	s := newTestStore(t)
	ctx := context.Background()
	repo, chat := chatFixture(t, s)
	say(t, s, chat.ID, SpeakerOperator, "hello", 0)

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}

	if _, err := s.GetChat(ctx, chat.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("chat err = %v, want ErrNotFound", err)
	}
	turns, err := s.ChatTurns(ctx, chat.ID, 0)
	if err != nil {
		t.Fatalf("ChatTurns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns = %d, want 0", len(turns))
	}
}

func TestFailStrandedChatTurnsOnlyTouchesChatsWaitingOnAReply(t *testing.T) {
	// Busy is derived from "the operator spoke last", so a daemon restart
	// mid-reply leaves the overlay saying "thinking…" for ever. Nothing else
	// should be disturbed to fix that.
	s := newTestStore(t)
	ctx := context.Background()

	waiting, answered, archived := Repo{Path: "/a"}, Repo{Path: "/b"}, Repo{Path: "/c"}
	var chats []Chat
	for _, r := range []Repo{waiting, answered, archived} {
		repo, err := s.UpsertRepo(ctx, r)
		if err != nil {
			t.Fatalf("UpsertRepo: %v", err)
		}
		chat, err := s.CreateChat(ctx, repo.ID)
		if err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
		chats = append(chats, chat)
	}
	say(t, s, chats[0].ID, SpeakerOperator, "still waiting", 0)
	say(t, s, chats[1].ID, SpeakerOperator, "asked", 0)
	say(t, s, chats[1].ID, SpeakerAssistant, "answered", 0)
	say(t, s, chats[2].ID, SpeakerOperator, "waiting but archived", 0)
	chats[2].ArchivedAt = time.Now().UTC()
	if err := s.SaveChat(ctx, chats[2]); err != nil {
		t.Fatalf("SaveChat: %v", err)
	}

	n, err := s.FailStrandedChatTurns(ctx, "the daemon restarted")
	if err != nil {
		t.Fatalf("FailStrandedChatTurns: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared = %d, want 1", n)
	}

	last, err := s.LastChatTurn(ctx, chats[0].ID)
	if err != nil {
		t.Fatalf("LastChatTurn: %v", err)
	}
	if last.Speaker != SpeakerAssistant || last.ErrMsg == "" {
		t.Errorf("last turn = %+v, want an assistant turn carrying the reason", last)
	}
	for _, id := range []int64{chats[1].ID, chats[2].ID} {
		turns, err := s.ChatTurns(ctx, id, 0)
		if err != nil {
			t.Fatalf("ChatTurns: %v", err)
		}
		for _, turn := range turns {
			if turn.ErrMsg != "" {
				t.Errorf("chat %d was disturbed: %+v", id, turn)
			}
		}
	}
}

func TestChatPulledGoalsSpanEveryPullOfThatConversation(t *testing.T) {
	// Every pull makes its own proposal and the chat carries on, so what has
	// already been filed is spread across all of them. Missing one would mean
	// re-proposing work the operator has already queued.
	s := newTestStore(t)
	ctx := context.Background()
	_, chat := chatFixture(t, s)
	_, other := func() (Repo, Chat) {
		repo, err := s.UpsertRepo(ctx, Repo{Path: "/src/other"})
		if err != nil {
			t.Fatalf("UpsertRepo: %v", err)
		}
		c, err := s.CreateChat(ctx, repo.ID)
		if err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
		return repo, c
	}()

	for i, goal := range []string{"add the in-flight column", "fix the reload guard"} {
		p, err := s.CreateProposal(ctx, Proposal{
			Kind: ProposalChat, State: ProposalReady, ChatID: chat.ID,
		})
		if err != nil {
			t.Fatalf("CreateProposal: %v", err)
		}
		if err := s.ReplaceProposalTasks(ctx, p.ID, []ProposalTask{
			{Ord: 0, Key: "k", Goal: goal},
		}); err != nil {
			t.Fatalf("ReplaceProposalTasks %d: %v", i, err)
		}
	}
	elsewhere, err := s.CreateProposal(ctx, Proposal{
		Kind: ProposalChat, State: ProposalReady, ChatID: other.ID,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if err := s.ReplaceProposalTasks(ctx, elsewhere.ID, []ProposalTask{
		{Ord: 0, Key: "k", Goal: "not this one"},
	}); err != nil {
		t.Fatalf("ReplaceProposalTasks: %v", err)
	}

	goals, err := s.ChatPulledGoals(ctx, chat.ID, 50)
	if err != nil {
		t.Fatalf("ChatPulledGoals: %v", err)
	}
	if len(goals) != 2 {
		t.Fatalf("goals = %v, want both pulls", goals)
	}
	for _, g := range goals {
		if g == "not this one" {
			t.Error("another conversation's goals leaked in")
		}
	}
}

func TestChatPullInFlightSeesOnlyARunningPull(t *testing.T) {
	// The second pull is refused on this, so it must not be fooled by a pull
	// that already finished — that would make the button dead for ever.
	s := newTestStore(t)
	ctx := context.Background()
	_, chat := chatFixture(t, s)

	busy, err := s.ChatPullInFlight(ctx, chat.ID)
	if err != nil {
		t.Fatalf("ChatPullInFlight: %v", err)
	}
	if busy {
		t.Fatal("a chat with no pulls is not busy")
	}

	p, err := s.CreateProposal(ctx, Proposal{
		Kind: ProposalChat, State: ProposalAnalysing, ChatID: chat.ID,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if busy, _ = s.ChatPullInFlight(ctx, chat.ID); !busy {
		t.Error("a pull that is running should be in flight")
	}

	p.State = ProposalReady
	if err := s.SaveProposal(ctx, p); err != nil {
		t.Fatalf("SaveProposal: %v", err)
	}
	if busy, _ = s.ChatPullInFlight(ctx, chat.ID); busy {
		t.Error("a pull that finished should not block the next one")
	}
}

func TestAProposalRemembersWhichConversationItCameFrom(t *testing.T) {
	// The link is what finds a chat's earlier pulls, and what the overlay
	// reads its "actions ready" card from.
	s := newTestStore(t)
	ctx := context.Background()
	_, chat := chatFixture(t, s)

	p, err := s.CreateProposal(ctx, Proposal{Kind: ProposalChat, ChatID: chat.ID})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	got, err := s.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if got.ChatID != chat.ID {
		t.Errorf("ChatID = %d, want %d", got.ChatID, chat.ID)
	}

	proposals, err := s.ChatProposals(ctx, chat.ID)
	if err != nil {
		t.Fatalf("ChatProposals: %v", err)
	}
	if len(proposals) != 1 || proposals[0].ID != p.ID {
		t.Errorf("ChatProposals = %+v, want just %d", proposals, p.ID)
	}
}

func TestChatProposalsIncludeTheOnesListProposalsHides(t *testing.T) {
	// A pull that found nothing is discarded, and a pull that was queued is
	// history — but the conversation still has to be able to say what happened
	// to its own last pull.
	s := newTestStore(t)
	ctx := context.Background()
	_, chat := chatFixture(t, s)

	for _, state := range []string{ProposalDiscarded, ProposalQueued} {
		if _, err := s.CreateProposal(ctx, Proposal{
			Kind: ProposalChat, State: state, ChatID: chat.ID,
		}); err != nil {
			t.Fatalf("CreateProposal: %v", err)
		}
	}
	proposals, err := s.ChatProposals(ctx, chat.ID)
	if err != nil {
		t.Fatalf("ChatProposals: %v", err)
	}
	if len(proposals) != 2 {
		t.Fatalf("ChatProposals = %d, want 2", len(proposals))
	}
	// Newest first, so the overlay can take the head and be right.
	if proposals[0].State != ProposalQueued {
		t.Errorf("head state = %q, want the newest (%q)", proposals[0].State, ProposalQueued)
	}
}
