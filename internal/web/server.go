// Package web serves overseer's dashboard: a board of task cards and a
// per-task timeline of Claude turns and Codex reviews.
package web

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"overseer/internal/config"
	"overseer/internal/engine"
	"overseer/internal/store"
)

//go:embed templates/*.html templates/*.css
var templateFS embed.FS

// Server serves the dashboard.
type Server struct {
	// cfg is the configuration as loaded at startup and never mutated. The
	// two tables the settings pane can change — providers and roles — live
	// on the engine instead, behind its lock, because worker goroutines read
	// them on every agent invocation.
	cfg config.Config
	// cfgPath is the file the settings pane writes to. Empty means the
	// daemon was started without one, and the pane is read-only.
	cfgPath string
	store   *store.Store
	eng     *engine.Engine
	tpl     *template.Template
	css     []byte
	hub     *Hub
}

// New builds a Server with its templates parsed and its SSE hub wired to the
// engine's change hook. cfgPath is the config file the settings pane edits;
// pass "" for a daemon with no file to write back to.
func New(cfg config.Config, st *store.Store, eng *engine.Engine, cfgPath string) *Server {
	s := &Server{cfg: cfg, store: st, eng: eng, hub: NewHub(), cfgPath: cfgPath}
	s.tpl = template.Must(template.New("dashboard").
		Funcs(templateFuncs()).
		ParseFS(templateFS, "templates/dashboard.html"))
	css, err := templateFS.ReadFile("templates/style.css")
	if err != nil {
		panic("web: stylesheet missing from the embedded templates: " + err.Error())
	}
	s.css = css
	eng.OnChange = func(taskID int64) { s.hub.Broadcast(taskID) }
	return s
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// px turns a bar height into a CSS length. Doing the arithmetic in Go
		// and the unit here keeps the template free of string concatenation.
		"px": func(n int) template.CSS { return template.CSS(strconv.Itoa(n) + "px") },
		"join": func(sep string, parts []string) string {
			return strings.Join(parts, sep)
		},
		// The wizard's "not created yet" id, so the template names the
		// constant rather than repeating a bare -1.
		"wizardNew":    func() int64 { return WizardNew },
		"wizardDesign": func() int64 { return WizardDesign },
	}
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
	mux.HandleFunc("GET /style.css", s.handleCSS)
	mux.HandleFunc("POST /tasks", s.requireSameOrigin(s.handleCreateTask))
	mux.HandleFunc("POST /tasks/bulk", s.requireSameOrigin(s.handleBulk))
	mux.HandleFunc("POST /task/{id}/continue", s.requireSameOrigin(s.handleContinue))
	mux.HandleFunc("POST /task/{id}/abandon", s.requireSameOrigin(s.handleAbandon))
	mux.HandleFunc("POST /task/{id}/severity", s.requireSameOrigin(s.handleSeverity))
	mux.HandleFunc("POST /task/{id}/cap", s.requireSameOrigin(s.handleCap))
	mux.HandleFunc("POST /task/{id}/release", s.requireSameOrigin(s.handleRelease))
	mux.HandleFunc("POST /task/{id}/stop", s.requireSameOrigin(s.handleStop))
	mux.HandleFunc("POST /task/{id}/start", s.requireSameOrigin(s.handleStart))
	mux.HandleFunc("POST /task/{id}/restart", s.requireSameOrigin(s.handleRestart))
	mux.HandleFunc("POST /task/{id}/plan", s.requireSameOrigin(s.handlePlan))
	mux.HandleFunc("POST /stopall", s.requireSameOrigin(s.handleStopAll))
	mux.HandleFunc("POST /resume", s.requireSameOrigin(s.handleResume))
	mux.HandleFunc("POST /settings", s.requireSameOrigin(s.handleSettings))
	mux.HandleFunc("POST /analyse", s.requireSameOrigin(s.handleAnalyse))
	mux.HandleFunc("POST /design", s.requireSameOrigin(s.handleDesign))
	mux.HandleFunc("POST /design/{id}/say", s.requireSameOrigin(s.handleSay))
	mux.HandleFunc("POST /design/{id}/accept", s.requireSameOrigin(s.handleAccept))
	mux.HandleFunc("POST /analyse/{id}/focus", s.requireSameOrigin(s.handleAnalyseFocus))
	mux.HandleFunc("POST /analyse/{id}/regenerate", s.requireSameOrigin(s.handleAnalyseRegenerate))
	mux.HandleFunc("POST /analyse/{id}/task/{taskID}", s.requireSameOrigin(s.handleAnalyseTask))
	mux.HandleFunc("POST /analyse/{id}/queue", s.requireSameOrigin(s.handleAnalyseQueue))
	mux.HandleFunc("POST /analyse/{id}/discard", s.requireSameOrigin(s.handleAnalyseDiscard))
	mux.HandleFunc("POST /chat", s.requireSameOrigin(s.handleChatSay))
	mux.HandleFunc("POST /chat/{id}/pull", s.requireSameOrigin(s.handleChatPull))
	mux.HandleFunc("POST /chat/{id}/new", s.requireSameOrigin(s.handleChatNew))
	mux.HandleFunc("POST /repos", s.requireSameOrigin(s.handleAddRepo))
	mux.HandleFunc("POST /repos/{id}/archive", s.requireSameOrigin(s.handleArchiveRepo))
	mux.HandleFunc("POST /backlog", s.requireSameOrigin(s.handleAddBacklog))
	mux.HandleFunc("POST /backlog/{id}/queue", s.requireSameOrigin(s.handleQueueBacklog))
	mux.HandleFunc("POST /backlog/{id}/dismiss", s.requireSameOrigin(s.handleDismissBacklog))
	mux.HandleFunc("GET /task/{id}/transcript/{stepID}", s.handleTranscript)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Server) handleCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	// The sheet is compiled into the binary, so it only ever changes when the
	// daemon does; a long cache saves a round trip on every SSE reload.
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Write(s.css)
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.renderDashboard(w, r, ParseQuery(r))
}

// handleTask is the deep link into one task. The board and the detail are the
// same page, so this is the same render with the selection forced — which
// keeps every /task/{id} link, bookmark and redirect from the CLI working.
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
	q := ParseQuery(r)
	q.Sel = id
	if !r.URL.Query().Has("tab") {
		q.Tab = defaultTab(task)
	}
	s.renderDashboard(w, r, q)
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, q Query) {
	view, err := s.build(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "dashboard.html", view); err != nil {
		writeError(w, err)
	}
}
