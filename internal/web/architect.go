package web

import (
	"context"
	"net/http"
	"strings"

	"overseer/internal/store"
)

// buildDesign assembles the conversation for a proposal being designed.
func (s *Server) buildDesign(ctx context.Context, p store.Proposal) (*DesignPane, error) {
	turns, err := s.store.ArchitectTurns(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	spend, err := s.store.ArchitectSpend(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	d := &DesignPane{
		Who:      "architect",
		Spend:    money(spend),
		Accepted: p.State != store.ProposalDesigning,
		Target:   "a new project",
	}
	if p.RepoPath != "" {
		d.Target = repoName(p.RepoPath)
	}
	for _, t := range turns {
		d.Turns = append(d.Turns, ConvoTurn{
			Speaker: t.Speaker,
			Body:    t.Body,
			When:    humanAge(t.CreatedAt),
			Mine:    t.Speaker == store.SpeakerOperator,
			Err:     t.ErrMsg != "",
		})
	}
	// The operator having spoken last means a reply is still coming.
	if n := len(turns); n > 0 && turns[n-1].Speaker == store.SpeakerOperator {
		d.Busy = true
	}
	return d, nil
}

// handleDesign opens a conversation: a new project, or a change to a
// repository that already exists.
func (s *Server) handleDesign(w http.ResponseWriter, r *http.Request) {
	brief := strings.TrimSpace(r.FormValue("brief"))
	repo := strings.TrimSpace(r.FormValue("repo"))
	if repo == "" {
		repo = strings.TrimSpace(r.FormValue("repo_path"))
	}

	// A new project is created before the conversation starts, so the architect
	// has somewhere real to work and the operator can see where it went.
	newProject := false
	if path := strings.TrimSpace(r.FormValue("new_path")); path != "" {
		created, err := s.eng.CreateProject(r.Context(), path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		repo, newProject = created.Path, true
	}

	p, err := s.eng.StartDesign(r.Context(), repo, brief, newProject)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.redirectToWizard(w, r, p.ID)
}

func (s *Server) handleSay(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.Say(r.Context(), id, r.FormValue("message")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToWizard(w, r, id)
}

func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.Accept(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToWizard(w, r, id)
}
