package web

import (
	"net/http"
	"strconv"
	"strings"

	"overseer/internal/engine"
	"overseer/internal/store"
)

// stopOpts reads how far a stop should go.
//
// "now" is the escalation, and it is always explicit: a soft stop costs at most
// one agent turn and wastes nothing, so it is not something to do by accident.
func stopOpts(r *http.Request) engine.StopOpts {
	return engine.StopOpts{
		Now:    r.FormValue("now") == "1",
		Reason: strings.TrimSpace(r.FormValue("reason")),
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.Stop(r.Context(), id, stopOpts(r)); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToTask(w, r, id)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.eng.Start(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToTask(w, r, id)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	opts := engine.RestartOpts{
		StopOpts: stopOpts(r),
		Goal:     strings.TrimSpace(r.FormValue("goal")),
	}
	for _, line := range strings.Split(r.FormValue("constraints"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			opts.Constraints = append(opts.Constraints, line)
		}
	}
	if err := s.eng.Restart(r.Context(), id, opts); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.redirectToTask(w, r, id)
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// The filename is never taken from the request: the engine hardcodes it.
	// Accepting one would make this an arbitrary write into any worktree.
	if err := s.eng.WritePlan(r.Context(), id, r.FormValue("plan")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/task/"+strconv.FormatInt(id, 10)+"?tab="+TabPlan, http.StatusSeeOther)
}

// handleStopAll is the panic button: stop claiming anything, and optionally
// stop what is already running.
//
// Persisted, so a restart does not quietly resume everything the operator just
// stopped. The authentication pause is deliberately not persisted alongside it:
// that is a condition that may have cleared, and a restart is exactly when it
// should be retried.
func (s *Server) handleStopAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.FormValue("clear") == "1" {
		if err := s.store.SetSetting(ctx, store.SettingStopAll, ""); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.eng.Resume()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "stopped by the operator"
	}
	if err := s.store.SetSetting(ctx, store.SettingStopAll, reason); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.eng.Pause(reason)

	// Tasks already mid-step park at their next boundary on their own. "Now"
	// additionally kills the agents, for the case where waiting out a wedged
	// step is the thing being avoided.
	if r.FormValue("now") == "1" {
		if err := s.eng.StopAllRunning(ctx, reason); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) redirectToTask(w http.ResponseWriter, r *http.Request, id int64) {
	http.Redirect(w, r, "/task/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
