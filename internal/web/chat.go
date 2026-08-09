package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"overseer/internal/store"
)

// chatRender bounds how much of a conversation the overlay draws.
//
// The page reloads on every state event — every couple of seconds while
// anything is running — and a conversation is durable and unbounded. Rendering
// all of it would put the whole transcript through SQLite and into the page
// several times a minute.
const chatRender = 200

// ChatView is the per-repository chat overlay.
type ChatView struct {
	RepoID   int64
	RepoSlug string
	// ChatID is zero when this repository has never been talked to. Rendering
	// must not create one — see buildChat.
	ChatID int64
	// Who labels the other side's turns, for the shared conversation block.
	Who   string
	Turns []ConvoTurn
	// Busy is true while a reply is owed: the operator spoke last. The compose
	// box stays open anyway, because thinking of the next question while it
	// answers is the normal way to use this.
	Busy bool
	// Truncated says the conversation is longer than what is drawn.
	Truncated string
	Spend     string
	// Pull is what happened to the last "pull actions from this conversation",
	// or nil if there has never been one.
	Pull *ChatPull
	// Repos is the switcher, the same chips the backlog uses.
	Repos    []RepoChoice
	CloseURL string
	BoardURL string
	// Empty is "no repositories registered at all", which is a different screen
	// from "this repository has nothing said in it yet".
	Empty bool
}

// ChatPull is the state of the most recent pull out of a conversation.
type ChatPull struct {
	ProposalID int64
	Running    bool
	// Label reads "2 actions ready", or says why there are none.
	Label string
	// ReviewURL opens the wizard's review pane. It deliberately closes the
	// chat: reviewing a task list is a different job from talking.
	ReviewURL string
	Err       string
}

// buildChat assembles the chat overlay for the repository in the URL.
func (s *Server) buildChat(ctx context.Context, q Query) (*ChatView, error) {
	repos, err := s.store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	v := &ChatView{Who: "overseer", CloseURL: q.URL("overlay", "")}
	if len(repos) == 0 {
		v.Empty = true
		return v, nil
	}

	// A chat is always a particular repository's. Landing here with none
	// chosen picks the first that is not archived, so a bare ?overlay=chat
	// renders something rather than erroring.
	repoID := q.Repo
	if repoID == 0 {
		for _, r := range repos {
			if !r.Archived() {
				repoID = r.ID
				break
			}
		}
	}
	if repoID == 0 {
		v.Empty = true
		return v, nil
	}

	repo, err := s.store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	v.RepoID = repo.ID
	v.RepoSlug = repo.Slug
	v.BoardURL = q.URL("repo", repo.ID, "overlay", "")

	for _, r := range repos {
		if r.Archived() && r.ID != repo.ID {
			continue
		}
		v.Repos = append(v.Repos, RepoChoice{
			ID: r.ID, Slug: r.Slug, Path: r.Path,
			On:  r.ID == repo.ID,
			URL: q.URL("repo", r.ID, "overlay", "chat"),
		})
	}
	sort.Slice(v.Repos, func(i, j int) bool { return v.Repos[i].Slug < v.Repos[j].Slug })

	// The non-creating lookup. Never OpenChat: this runs on every render.
	chat, err := s.store.LiveChat(ctx, repo.ID)
	if errors.Is(err, store.ErrNotFound) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	v.ChatID = chat.ID

	turns, err := s.store.ChatTurns(ctx, chat.ID, chatRender)
	if err != nil {
		return nil, err
	}
	total, err := s.store.CountChatTurns(ctx, chat.ID)
	if err != nil {
		return nil, err
	}
	if total > len(turns) {
		v.Truncated = fmt.Sprintf("showing the last %d of %d turns", len(turns), total)
	}
	spend, err := s.store.ChatSpend(ctx, chat.ID)
	if err != nil {
		return nil, err
	}
	v.Spend = money(spend)

	for _, t := range turns {
		v.Turns = append(v.Turns, ConvoTurn{
			Speaker: t.Speaker,
			Body:    t.Body,
			When:    humanAge(t.CreatedAt),
			Mine:    t.Speaker == store.SpeakerOperator,
			Err:     t.ErrMsg != "",
		})
	}
	// The operator having spoken last means a reply is still coming.
	if n := len(turns); n > 0 && turns[n-1].Speaker == store.SpeakerOperator {
		v.Busy = true
	}

	if v.Pull, err = s.chatPull(ctx, chat.ID, q); err != nil {
		return nil, err
	}
	return v, nil
}

// chatPull reads the most recent pull out of this conversation.
//
// Through the chat link rather than ListProposals, which excludes queued and
// discarded rows — and a pull that found nothing is discarded, which is
// precisely the outcome the conversation most needs to be able to report.
func (s *Server) chatPull(ctx context.Context, chatID int64, q Query) (*ChatPull, error) {
	proposals, err := s.store.ChatProposals(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if len(proposals) == 0 {
		return nil, nil
	}
	p := proposals[0]

	pull := &ChatPull{ProposalID: p.ID}
	switch p.State {
	case store.ProposalAnalysing:
		pull.Running = true
		pull.Label = "Pulling the actions out of this conversation…"
		pull.ReviewURL = q.URL("wizard", p.ID)
	case store.ProposalReady:
		rows, err := s.store.ProposalTasks(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		pull.Label = plural(len(rows), "action") + " ready to review"
		pull.ReviewURL = q.URL("wizard", p.ID)
	case store.ProposalFailed:
		pull.Err = p.ErrMsg
		pull.Label = "That pull failed."
	case store.ProposalDiscarded:
		// Either it found nothing, or the operator threw the list away. Both
		// are ordinary, and the reason is on the row.
		pull.Label = p.ErrMsg
		if pull.Label == "" {
			pull.Label = "That pull was discarded."
		}
	case store.ProposalQueued:
		// Already acted on. Nothing left to say about it here, and the tasks
		// are on the board.
		return nil, nil
	}
	return pull, nil
}

// handleChatSay asks a question, opening the conversation if this is the first.
//
// Keyed by repository rather than by chat: there is no chat row until somebody
// says something, and OpenChat is idempotent, so one route serves the first
// message and the thousandth.
func (s *Server) handleChatSay(w http.ResponseWriter, r *http.Request) {
	repoID, err := strconv.ParseInt(r.FormValue("repo_id"), 10, 64)
	if err != nil || repoID == 0 {
		http.Error(w, "which repository?", http.StatusBadRequest)
		return
	}
	repo, err := s.store.GetRepo(r.Context(), repoID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chat, err := s.eng.OpenChat(r.Context(), repo.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.eng.Ask(r.Context(), chat.ID, r.FormValue("message")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToChat(w, r, repoID)
}

// handleChatPull extracts the actions and comes straight back to the
// conversation.
//
// Deliberately not a redirect into the wizard. The pull runs in the background,
// so at this moment there is nothing to review yet; and opening the wizard
// closes this overlay, which would leave the operator with no way back to what
// they were saying.
func (s *Server) handleChatPull(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	chat, err := s.store.GetChat(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if _, err := s.eng.PullActions(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToChat(w, r, chat.RepoID)
}

// handleChatNew retires the conversation and opens a fresh one.
func (s *Server) handleChatNew(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	chat, err := s.store.GetChat(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if _, err := s.eng.NewChat(r.Context(), chat.RepoID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToChat(w, r, chat.RepoID)
}

// redirectToChat sends the operator back to the conversation they were in.
//
// The URL is built by hand rather than through Query.URL: a POST's r.URL
// carries no query string, so ParseQuery would yield an empty Query and the
// overlay would be lost. Same reason redirectToBacklog does it this way.
func (s *Server) redirectToChat(w http.ResponseWriter, r *http.Request, repoID int64) {
	url := "/?overlay=chat&repo=" + strconv.FormatInt(repoID, 10)
	http.Redirect(w, r, url, http.StatusSeeOther)
}
