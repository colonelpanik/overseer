package web

import (
	"fmt"
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
		Subject:          strings.TrimSpace(r.FormValue("subject")),
		Goal:             goal,
		BlockingSeverity: strings.TrimSpace(r.FormValue("blocking_severity")),
		Verify:           strings.TrimSpace(r.FormValue("verify")),
	}
	for _, line := range strings.Split(r.FormValue("constraints"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			bt.Constraints = append(bt.Constraints, line)
		}
	}
	if raw := strings.TrimSpace(r.FormValue("cost_cap")); raw != "" {
		cap, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			http.Error(w, "cost cap must be a number", http.StatusBadRequest)
			return
		}
		bt.CostCap = cap
	}

	task, err := s.eng.Submit(r.Context(), bt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Dependencies come back from the form as IDs rather than slugs: the
	// dialog offers existing tasks, and an ID cannot go stale between the
	// render and the submit the way a slug typed by hand can.
	depIDs, err := parseIDList(r.Form["depends_on"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(depIDs) > 0 {
		if err := s.store.SetTaskDeps(r.Context(), task.ID, depIDs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	http.Redirect(w, r, "/task/"+strconv.FormatInt(task.ID, 10), http.StatusSeeOther)
}

// handleBulk applies one action to every selected task. A run with ten parked
// tasks is a real state, and clicking through ten task pages to say the same
// thing ten times is how an operator ends up not saying it at all.
func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	ids, err := parseIDList(strings.Split(r.FormValue("ids"), ","))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(ids) == 0 {
		http.Error(w, "no tasks selected", http.StatusBadRequest)
		return
	}

	action := r.FormValue("action")
	var failures []string
	for _, id := range ids {
		var err error
		switch action {
		case "continue":
			err = s.eng.ContinueEscalated(r.Context(), id, extraIterations)
		case "abandon":
			err = s.eng.Abandon(r.Context(), id, engine.StopOpts{})
		default:
			http.Error(w, "unknown bulk action "+action, http.StatusBadRequest)
			return
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("task %d: %v", id, err))
		}
	}
	// Report what did not work rather than redirecting to a board that
	// silently kept some of the tasks in the state the operator just asked to
	// change. The ones that did work have already been applied.
	if len(failures) > 0 {
		http.Error(w, strings.Join(failures, "\n"), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleSeverity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sev := strings.TrimSpace(r.FormValue("blocking_severity"))
	if err := s.eng.SetBlockingSeverity(r.Context(), id, sev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/task/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleCap(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	cap, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("cost_cap")), 64)
	if err != nil {
		http.Error(w, "cost cap must be a number", http.StatusBadRequest)
		return
	}
	if err := s.eng.RaiseCap(r.Context(), id, cap); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/task/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.ReleaseDeps(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/task/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// parseIDList reads a list of task IDs, skipping blanks and rejecting
// anything that is not a positive integer.
func parseIDList(raw []string) ([]int64, error) {
	var out []int64
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a task id", s)
		}
		out = append(out, id)
	}
	return out, nil
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

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.eng.Resume()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleAbandon(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.Abandon(r.Context(), id, engine.StopOpts{}); err != nil {
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
