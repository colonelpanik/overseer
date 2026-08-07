// Package web serves overseer's dashboard: a board of task cards and a
// per-task timeline of Claude turns and Codex reviews.
package web

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"strconv"

	"overseer/internal/config"
	"overseer/internal/engine"
	"overseer/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server serves the dashboard.
type Server struct {
	cfg   config.Config
	store *store.Store
	eng   *engine.Engine
	tpl   map[string]*template.Template
	hub   *Hub
}

// New builds a Server with its templates parsed and its SSE hub wired to the
// engine's change hook.
func New(cfg config.Config, st *store.Store, eng *engine.Engine) *Server {
	s := &Server{cfg: cfg, store: st, eng: eng, hub: NewHub()}
	s.tpl = map[string]*template.Template{
		"board": mustParse("board.html"),
		"task":  mustParse("task.html"),
	}
	eng.OnChange = func(taskID int64) { s.hub.Broadcast(taskID) }
	return s
}

func mustParse(page string) *template.Template {
	return template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/"+page))
}

// ListenAndServe runs the HTTP server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.ListenAddr, Handler: s.routes()}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleBoard)
	mux.HandleFunc("GET /task/{id}", s.handleTask)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /tasks", s.handleCreateTask)
	mux.HandleFunc("POST /task/{id}/continue", s.handleContinue)
	mux.HandleFunc("POST /task/{id}/abandon", s.handleAbandon)
	mux.HandleFunc("GET /task/{id}/transcript/{stepID}", s.handleTranscript)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tasks, err := s.store.ListTasks(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	view := BoardView{Title: "board"}
	for _, t := range tasks {
		totals, err := s.store.TaskTotals(r.Context(), t.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		view.Tasks = append(view.Tasks, TaskCard{
			Task: t, Totals: totals, Badge: Badge(t.State),
			Progress: Progress(t), Elapsed: elapsed(t),
		})
	}
	s.render(w, "board", view)
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	task, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	totals, err := s.store.TaskTotals(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	steps, err := s.store.ListSteps(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	view := TaskView{
		Title: task.Slug, Task: task, Totals: totals,
		Badge: Badge(task.State), Progress: Progress(task),
		TakeOver: s.eng.TakeOverHint(task),
	}
	for _, step := range steps {
		findings, err := s.store.ListFindings(r.Context(), step.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var blocking []store.Finding
		for _, f := range findings {
			if f.Blocking {
				blocking = append(blocking, f)
			} else {
				view.PunchList = append(view.PunchList, f)
			}
		}
		view.Timeline = append(view.Timeline, TimelineEntry{
			Step: step, Findings: blocking, Duration: stepDuration(step),
		})
	}
	s.render(w, "task", view)
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl[page].ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
