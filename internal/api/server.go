// Package api is the client-facing HTTP surface of the gateway.
package api

import (
	"log/slog"
	"net/http"
	"sync/atomic"
)

// Options configures a Server.
type Options struct {
	Logger *slog.Logger
}

// Server handles client requests. Use New to create one.
type Server struct {
	logger   *slog.Logger
	mux      *http.ServeMux
	draining atomic.Bool
}

// New returns a Server ready to handle requests.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{logger: logger, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.HandleFunc("/", s.handleNotFound)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// SetDraining makes readiness fail, so that load balancers stop sending new
// requests while in-flight ones finish.
func (s *Server) SetDraining() { s.draining.Store(true) }

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, "ok")
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		writeStatus(w, http.StatusServiceUnavailable, "draining")
		return
	}
	writeStatus(w, http.StatusOK, "ready")
}

func (s *Server) handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, &apiError{
		Status:  http.StatusNotFound,
		Type:    errTypeInvalidRequest,
		Code:    "not_found",
		Message: "no such endpoint",
	})
}

func writeStatus(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
}
