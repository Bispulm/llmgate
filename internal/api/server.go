// Package api is the client-facing HTTP surface of the gateway. It owns the
// downstream commitment point described in
// docs/adr/0003-downstream-commitment-point.md.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/routing"
)

// Limits bounds what one request can consume. A zero duration disables that
// timer. Configuration always sets them, so this only affects tests.
type Limits struct {
	MaxRequestBytes int64
	// RequestTimeout is the deadline for the whole logical request.
	RequestTimeout time.Duration
	// FirstEventTimeout bounds the wait for the first content event.
	FirstEventTimeout time.Duration
	// StreamIdleTimeout bounds the wait between content events after commit.
	StreamIdleTimeout time.Duration
	// ClientWriteTimeout bounds each write to the client.
	ClientWriteTimeout time.Duration
	// BodyReadTimeout bounds reading the request body.
	BodyReadTimeout time.Duration
}

// Options configures a Server.
type Options struct {
	Logger   *slog.Logger
	Auth     auth.Authenticator
	Routes   *routing.Table
	Recorder accounting.Recorder
	Limits   Limits
}

// Server handles client requests. Use New to create one.
type Server struct {
	logger   *slog.Logger
	auth     auth.Authenticator
	routes   *routing.Table
	recorder accounting.Recorder
	limits   Limits
	mux      *http.ServeMux
	draining atomic.Bool
}

// New returns a Server. Without an Authenticator every request is rejected, so a
// partly configured server can't serve traffic by accident.
func New(opts Options) *Server {
	s := &Server{
		logger:   opts.Logger,
		auth:     opts.Auth,
		routes:   opts.Routes,
		recorder: opts.Recorder,
		limits:   opts.Limits,
		mux:      http.NewServeMux(),
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.auth == nil {
		s.auth = denyAll{}
	}
	if s.routes == nil {
		s.routes = routing.NewTable(nil)
	}
	if s.recorder == nil {
		s.recorder = accounting.NewLogRecorder(s.logger)
	}
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("/", s.handleNotFound)
	return s
}

// ServeHTTP implements http.Handler.
//
// A write deadline belongs to the connection, so it is set at the start of every
// request. Otherwise a deadline left by an earlier response would fail a later
// request on the same connection. It stays set after the handler returns because
// that bounds the final write of the response, even to a client that has stalled.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.extendWriteDeadline(http.NewResponseController(w))
	s.mux.ServeHTTP(w, r)
}

func (s *Server) extendWriteDeadline(rc *http.ResponseController) {
	if s.limits.ClientWriteTimeout > 0 {
		// Unsupported only for test recorders, which have no connection to stall.
		_ = rc.SetWriteDeadline(time.Now().Add(s.limits.ClientWriteTimeout))
	}
}

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

type denyAll struct{}

func (denyAll) Authenticate(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, auth.ErrInvalidCredential
}
