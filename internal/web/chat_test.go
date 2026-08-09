package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"overseer/internal/store"
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// chatRepo registers a repository and returns it, since every chat needs one.
func chatRepo(t *testing.T, st *store.Store) store.Repo {
	t.Helper()
	repo, err := st.UpsertRepo(context.Background(), store.Repo{
		Path: "/src/widget", Detected: "Go · go test ./...",
	})
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	return repo
}

func TestTheChatOverlayIsInTheAllowList(t *testing.T) {
	// ParseQuery silently drops an overlay it does not know, which would make
	// the Chat button render the plain board and appear to do nothing at all.
	s, st := newTestServer(t)
	repo := chatRepo(t, st)

	body := get(t, s, "/?overlay=chat&repo="+itoa(repo.ID)).Body.String()
	if !strings.Contains(body, `action="/chat"`) {
		t.Error("the chat overlay did not render")
	}
}

func TestChatOverlayRendersForARepositoryWithNothingSaidYet(t *testing.T) {
	s, st := newTestServer(t)
	repo := chatRepo(t, st)

	body := get(t, s, "/?overlay=chat&repo="+itoa(repo.ID)).Body.String()
	if !strings.Contains(body, "widget") {
		t.Error("the overlay does not name the repository")
	}
	if !strings.Contains(body, `name="message"`) {
		t.Error("there is nowhere to ask a question")
	}
}

func TestRenderingTheChatDoesNotOpenOne(t *testing.T) {
	// The dashboard reloads on every state event. A render path that created a
	// row would open a conversation against every repository the operator
	// happened to look at, each one an empty thread with a cost of its own.
	s, st := newTestServer(t)
	repo := chatRepo(t, st)

	get(t, s, "/?overlay=chat&repo="+itoa(repo.ID))

	if _, err := st.LiveChat(context.Background(), repo.ID); err == nil {
		t.Error("looking at the overlay opened a conversation")
	}
}

func TestChatRendersTheConversationSidedAndMarksAFailedTurn(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := chatRepo(t, st)
	chat, err := st.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range []store.ChatTurn{
		{ChatID: chat.ID, Speaker: store.SpeakerOperator, Body: "why does busy come from turn order?"},
		{ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "because the daemon restarts"},
		{ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "I could not reply: timed out", ErrMsg: "timed out"},
	} {
		if _, err := st.AddChatTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
	}

	body := get(t, s, "/?overlay=chat&repo="+itoa(repo.ID)).Body.String()
	for _, want := range []string{
		"why does busy come from turn order?",
		"because the daemon restarts",
		`turn mine`,
		`turn theirs`,
		"bad",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chat pane missing %q", want)
		}
	}
}

func TestChatSaysItIsThinkingWhileAReplyIsOwed(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := chatRepo(t, st)
	chat, err := st.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerOperator, Body: "why?",
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?overlay=chat&repo="+itoa(repo.ID)).Body.String()
	if !strings.Contains(body, "thinking") {
		t.Error("the overlay should say a reply is coming")
	}
}

func TestPostChatRecordsTheQuestionAndComesBackToTheOverlay(t *testing.T) {
	s, st := newTestServer(t)
	// OpenChat resolves and validates the repository, so this one has to exist.
	path := initRepo(t)
	repo, err := st.UpsertRepo(context.Background(), store.Repo{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/chat", url.Values{
		"repo_id": {itoa(repo.ID)},
		"message": {"why does busy come from turn order?"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "overlay=chat") || !strings.Contains(loc, "repo=") {
		t.Errorf("Location = %q, want the chat overlay", loc)
	}

	ctx := context.Background()
	chat, err := st.LiveChat(ctx, repo.ID)
	if err != nil {
		t.Fatalf("asking should have opened a conversation: %v", err)
	}
	turns, err := st.ChatTurns(ctx, chat.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Speaker != store.SpeakerOperator {
		t.Fatalf("turns = %+v, want the operator's question", turns)
	}
}

func TestPullingDoesNotCloseTheConversation(t *testing.T) {
	// The requirement, expressed as a redirect. Sending the operator to the
	// wizard would close the overlay, and closing the wizard would then strand
	// them on the bare board with no way back to what they were saying.
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := chatRepo(t, st)
	chat, err := st.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddChatTurn(ctx, store.ChatTurn{
		ChatID: chat.ID, Speaker: store.SpeakerAssistant, Body: "agreed",
	}); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/chat/"+itoa(chat.ID)+"/pull", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "wizard=") {
		t.Errorf("Location = %q, want to stay in the conversation", loc)
	}
	if !strings.Contains(loc, "overlay=chat") {
		t.Errorf("Location = %q, want the chat overlay", loc)
	}
}

func TestChatOffersTheReviewLinkOncePulledActionsAreReady(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := chatRepo(t, st)
	chat, err := st.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalChat, State: store.ProposalReady,
		ChatID: chat.ID, RepoID: repo.ID, RepoPath: repo.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceProposalTasks(ctx, p.ID, []store.ProposalTask{
		{Ord: 0, Key: "a", Goal: "Add the in-flight column", Selected: true},
		{Ord: 1, Key: "b", Goal: "Fix the reload guard", Selected: true},
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?overlay=chat&repo="+itoa(repo.ID)).Body.String()
	if !strings.Contains(body, "2 actions") {
		t.Error("the overlay does not say how many actions are waiting")
	}
	if !strings.Contains(body, "wizard="+itoa(p.ID)) {
		t.Error("there is no link to review them")
	}
}

func TestChatSaysWhenAPullIsRunningOrFoundNothing(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := chatRepo(t, st)
	chat, err := st.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	running, err := st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalChat, State: store.ProposalAnalysing,
		ChatID: chat.ID, RepoID: repo.ID, RepoPath: repo.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := get(t, s, "/?overlay=chat&repo="+itoa(repo.ID)).Body.String(); !strings.Contains(body, "Pulling") {
		t.Error("a running pull should say so")
	}

	running.State = store.ProposalDiscarded
	running.ErrMsg = "nothing new has been decided in this conversation yet"
	if err := st.SaveProposal(ctx, running); err != nil {
		t.Fatal(err)
	}
	// ListProposals hides a discarded row, so this is also the test that the
	// pane reads its pull state through the chat link rather than that list.
	if body := get(t, s, "/?overlay=chat&repo="+itoa(repo.ID)).Body.String(); !strings.Contains(body, "nothing new has been decided") {
		t.Error("a pull that found nothing should say so in the conversation")
	}
}

func TestAFailedChatPullDoesNotOfferToRunAnAnalysis(t *testing.T) {
	// A failed proposal lands on the focus step, whose form posts to
	// /analyse/{id}/focus. On a chat pull that button would start a full
	// repository analysis — different work, different cost.
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := chatRepo(t, st)
	chat, err := st.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalChat, State: store.ProposalFailed,
		ChatID: chat.ID, RepoID: repo.ID, RepoPath: repo.Path,
		ErrMsg: "the pull returned nothing usable",
	})
	if err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?wizard="+itoa(p.ID)).Body.String()
	if strings.Contains(body, `action="/analyse/`+itoa(p.ID)+`/focus"`) {
		t.Error("a failed chat pull offers to run a repository analysis")
	}
	if !strings.Contains(body, "the pull returned nothing usable") {
		t.Error("the failure is not shown")
	}
}

func TestTheWizardOpensAChatPullOnTheReviewStep(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := chatRepo(t, st)
	chat, err := st.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProposal(ctx, store.Proposal{
		Kind: store.ProposalChat, State: store.ProposalReady,
		ChatID: chat.ID, RepoID: repo.ID, RepoPath: repo.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceProposalTasks(ctx, p.ID, []store.ProposalTask{
		{Ord: 0, Key: "a", Goal: "Add the in-flight column", Selected: true},
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/?wizard="+itoa(p.ID)).Body.String()
	if !strings.Contains(body, "Add the in-flight column") {
		t.Error("the proposed action is not shown")
	}
	// The way back is the conversation, not the board.
	if !strings.Contains(body, "overlay=chat") {
		t.Error("there is no way back to the conversation")
	}
}

func TestEveryChatRouteRequiresSameOrigin(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	repo := chatRepo(t, st)
	chat, err := st.CreateChat(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/chat",
		"/chat/" + itoa(chat.ID) + "/pull",
		"/chat/" + itoa(chat.ID) + "/new",
	} {
		if rec := crossSitePost(t, s, path, url.Values{}); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, rec.Code)
		}
	}
}

func TestTheChatIsNotLoadedWhenItsOverlayIsClosed(t *testing.T) {
	// The page reloads every couple of seconds while anything runs. Reading a
	// conversation that may be months long on every one of those, for an
	// operator looking at the board, is the kind of cost that never shows up
	// as a bug.
	s, st := newTestServer(t)
	chatRepo(t, st)

	d, err := s.build(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Chat != nil {
		t.Error("the chat was assembled for a page that is not showing it")
	}
}
