// Package web serves overseer's dashboard.
package web

import (
	"context"
	"net/http"

	"overseer/internal/config"
	"overseer/internal/engine"
	"overseer/internal/store"
)

// Server serves the dashboard.
type Server struct {
	cfg   config.Config
	store *store.Store
	eng   *engine.Engine
}

// New builds a Server.
func New(cfg config.Config, st *store.Store, eng *engine.Engine) *Server {
	return &Server{cfg: cfg, store: st, eng: eng}
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return mux
}
