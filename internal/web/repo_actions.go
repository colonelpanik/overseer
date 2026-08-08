package web

import (
	"net/http"
	"strconv"
	"strings"
)

// handleAddRepo registers a repository, or writes its inherited defaults.
//
// It is the same EnsureRepo every submit and every analysis calls, so "add a
// repo" is not a separate concept with its own validation — a repository added
// here and one that registered itself on first use are the same row.
func (s *Server) handleAddRepo(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.FormValue("path"))
	if path == "" {
		http.Error(w, "a repository needs a path", http.StatusBadRequest)
		return
	}
	repo, err := s.eng.EnsureRepo(r.Context(), path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The defaults fields are only present when the form was the edit form,
	// not the bare add box. Writing zeroes for absent fields would silently
	// clear settings the operator never touched.
	if r.Form.Has("verify") || r.Form.Has("blocking_severity") || r.Form.Has("cost_cap") {
		var capUSD float64
		if raw := strings.TrimSpace(r.FormValue("cost_cap")); raw != "" {
			capUSD, err = strconv.ParseFloat(raw, 64)
			if err != nil {
				http.Error(w, "cost cap must be a number", http.StatusBadRequest)
				return
			}
		}
		err = s.eng.SetRepoDefaults(r.Context(), repo.ID,
			r.FormValue("verify"), strings.TrimSpace(r.FormValue("blocking_severity")), capUSD)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	http.Redirect(w, r, "/?overlay=repos", http.StatusSeeOther)
}

func (s *Server) handleArchiveRepo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// Unarchiving goes through the same route, because the button is the same
	// button with the opposite label.
	archived := r.FormValue("archived") != "0"
	if err := s.eng.ArchiveRepo(r.Context(), id, archived); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/?overlay=repos", http.StatusSeeOther)
}

func (s *Server) handleAddBacklog(w http.ResponseWriter, r *http.Request) {
	repoID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("repo_id")), 10, 64)
	if err != nil || repoID <= 0 {
		http.Error(w, "a backlog item needs a repository", http.StatusBadRequest)
		return
	}
	_, err = s.eng.AddBacklogItem(r.Context(), repoID,
		r.FormValue("title"), r.FormValue("detail"),
		strings.TrimSpace(r.FormValue("severity")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.redirectToBacklog(w, r, repoID)
}

func (s *Server) handleQueueBacklog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	task, err := s.eng.PromoteBacklogItem(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// Straight to the task, not back to the list: the operator just created
	// work, and what they want to see is the work.
	http.Redirect(w, r, "/task/"+strconv.FormatInt(task.ID, 10), http.StatusSeeOther)
}

func (s *Server) handleDismissBacklog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	item, err := s.store.GetBacklogItem(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// The same route reopens, because dismissing is the kind of thing an
	// operator changes their mind about.
	if r.FormValue("reopen") == "1" {
		err = s.eng.ReopenBacklogItem(r.Context(), id)
	} else {
		err = s.eng.DismissBacklogItem(r.Context(), id)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToBacklog(w, r, item.RepoID)
}

func (s *Server) redirectToBacklog(w http.ResponseWriter, r *http.Request, repoID int64) {
	http.Redirect(w, r,
		"/?overlay=backlog&repo="+strconv.FormatInt(repoID, 10), http.StatusSeeOther)
}
