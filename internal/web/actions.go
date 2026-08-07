package web

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"overseer/internal/engine"
)

// extraIterations is the budget granted when the operator presses Continue.
const extraIterations = 10

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.FormValue("repo"))
	goal := strings.TrimSpace(r.FormValue("goal"))
	if repo == "" || goal == "" {
		http.Error(w, "repo and goal are both required", http.StatusBadRequest)
		return
	}
	bt := engine.BatchTask{
		Repo:             repo,
		Goal:             goal,
		BlockingSeverity: strings.TrimSpace(r.FormValue("blocking_severity")),
	}
	if _, err := s.eng.Submit(r.Context(), bt); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.ContinueEscalated(r.Context(), id, extraIterations); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/task/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleAbandon(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.Abandon(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/task/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleTranscript serves one step's raw JSONL. The step is checked to
// belong to the task in the URL, so the endpoint cannot be used to read an
// arbitrary path recorded against a different task.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	taskID, ok := pathID(w, r)
	if !ok {
		return
	}
	stepID, err := strconv.ParseInt(r.PathValue("stepID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	steps, err := s.store.ListSteps(r.Context(), taskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, step := range steps {
		if step.ID != stepID {
			continue
		}
		if step.TranscriptPath == "" {
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(step.TranscriptPath)
		if err != nil {
			http.Error(w, "transcript not on disk", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(raw)
		return
	}
	http.NotFound(w, r)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}
