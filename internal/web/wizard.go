package web

import (
	"net/http"
	"strconv"
	"strings"
)

// handleAnalyse opens a wizard against a local path or a URL to clone.
func (s *Server) handleAnalyse(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.FormValue("repo"))
	url := strings.TrimSpace(r.FormValue("url"))

	// One or the other, never both: a form that submitted both would leave it
	// to this handler to guess which the operator meant.
	if (repo == "") == (url == "") {
		http.Error(w, "give either a repository path or a URL to clone, not both",
			http.StatusBadRequest)
		return
	}

	var id int64
	if url != "" {
		p, err := s.eng.ImportProposal(r.Context(), url)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id = p.ID
	} else {
		p, err := s.eng.StartProposal(r.Context(), repo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id = p.ID
	}
	s.redirectToWizard(w, r, id)
}

// handleAnalyseFocus saves the steering and starts the analysis.
func (s *Server) handleAnalyseFocus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var focus []string
	for _, f := range r.Form["focus"] {
		if f = strings.TrimSpace(f); f != "" {
			focus = append(focus, f)
		}
	}
	maxTasks, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_tasks")))
	if err != nil {
		http.Error(w, "max tasks must be a whole number", http.StatusBadRequest)
		return
	}
	err = s.eng.ConfigureProposal(r.Context(), id, focus,
		strings.TrimSpace(r.FormValue("notes")), maxTasks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.redirectToWizard(w, r, id)
}

// handleAnalyseRegenerate re-runs the analysis with the operator's feedback.
func (s *Server) handleAnalyseRegenerate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.eng.RegenerateProposal(r.Context(), id,
		strings.TrimSpace(r.FormValue("feedback")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.redirectToWizard(w, r, id)
}

// handleAnalyseTask toggles or edits one proposed task.
//
// Editing is deliberately limited to the fields that change what the task
// does. The key and the ordering are what dependencies are resolved through,
// so they are not the operator's to rewrite here.
func (s *Server) handleAnalyseTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	taskID, err := strconv.ParseInt(r.PathValue("taskID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Loading through the proposal is the authorisation check: a hand-edited
	// URL cannot reach a row belonging to a different analysis.
	row, err := s.store.GetProposalTask(r.Context(), id, taskID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch r.FormValue("action") {
	case "toggle":
		row.Selected = !row.Selected
	case "save":
		goal := strings.TrimSpace(r.FormValue("goal"))
		if goal == "" {
			http.Error(w, "a task needs a goal", http.StatusBadRequest)
			return
		}
		row.Goal = goal
		row.Verify = strings.TrimSpace(r.FormValue("verify"))
		if sev := strings.TrimSpace(r.FormValue("severity")); sev != "" {
			row.Severity = sev
		}
		if raw := strings.TrimSpace(r.FormValue("cost_cap")); raw != "" {
			cap, err := strconv.ParseFloat(raw, 64)
			if err != nil || cap < 0 {
				http.Error(w, "the cost cap must be a number that is not negative",
					http.StatusBadRequest)
				return
			}
			row.CostCap = cap
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	if err := s.store.SaveProposalTask(r.Context(), row); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.redirectToWizard(w, r, id)
}

// handleAnalyseQueue turns the selection into real tasks.
func (s *Server) handleAnalyseQueue(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	created, err := s.eng.QueueProposal(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Land on the first task queued rather than a bare board: the operator
	// just made a decision and the result of it should be in front of them.
	target := "/"
	if len(created) > 0 {
		target = "/task/" + strconv.FormatInt(created[0].ID, 10)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleAnalyseDiscard throws the proposal away.
func (s *Server) handleAnalyseDiscard(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.DiscardProposal(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// redirectToWizard returns to the board with the wizard open, preserving the
// rest of the view state the operator was looking at.
func (s *Server) redirectToWizard(w http.ResponseWriter, r *http.Request, id int64) {
	q := ParseQuery(r)
	http.Redirect(w, r, q.URL("wizard", id), http.StatusSeeOther)
}
