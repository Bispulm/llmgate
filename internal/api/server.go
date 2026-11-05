// Package api is the client-facing HTTP surface of the gateway. It owns the
// downstream commitment point described in
// docs/adr/0003-downstream-commitment-point.md.
package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/routing"
)

const waitForRequestsInterval = 10 * time.Millisecond

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
	// CompletionTimeout bounds one non-streaming attempt, leaving time for a fallback.
	CompletionTimeout time.Duration
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
	inflight atomic.Int64
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
		s.routes = routing.NewTable(nil, routing.Options{})
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
//
// The timeout limits how long one write can block, not how long a provider can
// take, so anything writing after a wait must call extendWriteDeadline first.
//
// The read deadline bounds the request body, including when no handler reads it:
// net/http drains an unread body before writing a response, and would otherwise
// wait on a stalled client indefinitely. If that drain fails, net/http closes the
// connection, so the unread bytes can't be parsed as another request.
//
// net/http clears the deadline itself once the body is consumed, when it starts
// watching for the client disconnecting. For a request with no body that watch has
// already started, and a deadline would cancel the request when it expired.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)

	rc := http.NewResponseController(w)
	s.extendWriteDeadline(rc)
	if s.limits.BodyReadTimeout > 0 && r.ContentLength != 0 {
		_ = rc.SetReadDeadline(time.Now().Add(s.limits.BodyReadTimeout))
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) extendWriteDeadline(rc *http.ResponseController) {
	if s.limits.ClientWriteTimeout > 0 {
		// Unsupported only for test recorders, which have no connection to stall.
		_ = rc.SetWriteDeadline(time.Now().Add(s.limits.ClientWriteTimeout))
	}
}

// WaitForRequests blocks until no request is being handled, or ctx ends. A handler
// records its attempt before it finishes, so a process that waits for this before
// exiting doesn't lose those records.
func (s *Server) WaitForRequests(ctx context.Context) error {
	ticker := time.NewTicker(waitForRequestsInterval)
	defer ticker.Stop()
	for s.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for requests to finish: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
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
	w.Header().Set("X-Request-Id", rand.Text())
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
