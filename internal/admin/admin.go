// Package admin is the operator's API: teams, keys, budgets, usage, and reconciliation.
// It has its own listener and its own token, and is bound to loopback unless the
// configuration says otherwise. Nothing here is reachable with a client's key.
package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/routing"
)

const (
	maxBodyBytes        = 64 << 10
	defaultAttemptLimit = 50
	maxAttemptLimit     = 500
	reconcileBatch      = 500
)

// Options configures the admin API.
type Options struct {
	// Token is the bearer token every request needs.
	Token  string
	Logger *slog.Logger
	Teams  *auth.TeamStore
	Keys   *auth.KeyStore
	Ledger *accounting.Ledger
	Routes *routing.Table
}

type server struct {
	digest [sha256.Size]byte
	logger *slog.Logger
	teams  *auth.TeamStore
	keys   *auth.KeyStore
	ledger *accounting.Ledger
	routes *routing.Table
}

// New returns the admin API's handler. An empty token refuses every request.
func New(opts Options) http.Handler {
	s := &server{
		digest: sha256.Sum256([]byte(opts.Token)),
		logger: opts.Logger,
		teams:  opts.Teams,
		keys:   opts.Keys,
		ledger: opts.Ledger,
		routes: opts.Routes,
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/teams", s.listTeams)
	mux.HandleFunc("POST /admin/v1/teams", s.createTeam)
	mux.HandleFunc("GET /admin/v1/teams/{team}", s.getTeam)
	mux.HandleFunc("PUT /admin/v1/teams/{team}/budget", s.setBudget)
	mux.HandleFunc("PUT /admin/v1/teams/{team}/limits", s.setLimits)
	mux.HandleFunc("GET /admin/v1/teams/{team}/keys", s.listKeys)
	mux.HandleFunc("POST /admin/v1/teams/{team}/keys", s.createKey)
	mux.HandleFunc("DELETE /admin/v1/keys/{key}", s.revokeKey)
	mux.HandleFunc("GET /admin/v1/teams/{team}/usage", s.usage)
	mux.HandleFunc("GET /admin/v1/teams/{team}/attempts", s.attempts)
	mux.HandleFunc("POST /admin/v1/attempts/{attempt}/usage", s.lateUsage)
	mux.HandleFunc("GET /admin/v1/providers", s.providers)
	mux.HandleFunc("POST /admin/v1/reconcile", s.reconcile)

	empty := opts.Token == ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if empty || !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "the admin token is missing or wrong")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		mux.ServeHTTP(w, r)
	})
}

// authorized compares fixed-size digests, so that the time taken says nothing about the
// token's length or contents.
func (s *server) authorized(r *http.Request) bool {
	got := sha256.Sum256([]byte(auth.BearerToken(r.Header)))
	return subtle.ConstantTimeCompare(got[:], s.digest[:]) == 1
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail answers with the status an error deserves. Internal details are logged, never
// returned, since a database error can name tables, hosts, or values.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrUnknownTeam), errors.Is(err, accounting.ErrUnknownTeam):
		writeError(w, http.StatusNotFound, "unknown_team", "no such team")
	case errors.Is(err, auth.ErrUnknownKey):
		writeError(w, http.StatusNotFound, "unknown_key", "no such key")
	case errors.Is(err, accounting.ErrUnknownAttempt):
		writeError(w, http.StatusNotFound, "unknown_attempt", "no such attempt")
	case errors.Is(err, auth.ErrTeamExists):
		writeError(w, http.StatusConflict, "team_exists", "a team with this ID already exists")
	case errors.Is(err, auth.ErrInvalidTeam):
		writeError(w, http.StatusBadRequest, "invalid_team",
			"team IDs are 1 to 63 lowercase letters, digits, hyphens, or underscores, and limits can't be negative")
	default:
		s.logger.LogAttrs(r.Context(), slog.LevelError, "an admin request failed", slog.String("path", r.Pattern), slog.Any("error", err))
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the request could not be completed")
	}
}

// decode reads a JSON body strictly, so that a misspelled field is an error rather than a
// setting silently left at zero.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "the body isn't valid JSON for this request")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_body", "the body has data after the JSON value")
		return false
	}
	return true
}

func (s *server) audit(ctx context.Context, action string, attrs ...slog.Attr) {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "admin action", append([]slog.Attr{slog.String("action", action)}, attrs...)...)
}

func (s *server) listTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.teams.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, teams)
}

func (s *server) createTeam(w http.ResponseWriter, r *http.Request) {
	var team auth.Team
	if !decode(w, r, &team) {
		return
	}
	created, err := s.teams.Create(r.Context(), team)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), "create_team", slog.String("team_id", created.ID), slog.Int64("budget_limit_micros", created.BudgetLimitMicros))
	writeJSON(w, http.StatusCreated, created)
}

func (s *server) getTeam(w http.ResponseWriter, r *http.Request) {
	team, err := s.teams.Get(r.Context(), r.PathValue("team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	budget, err := s.ledger.Budget(r.Context(), team.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Team   auth.Team         `json:"team"`
		Budget accounting.Budget `json:"budget"`
	}{team, budget})
}

func (s *server) setBudget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LimitMicros *int64 `json:"limit_micros"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.LimitMicros == nil || *body.LimitMicros < 0 {
		writeError(w, http.StatusBadRequest, "invalid_budget", "limit_micros is required and can't be negative")
		return
	}
	team := r.PathValue("team")
	if err := s.ledger.SetLimit(r.Context(), team, *body.LimitMicros); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), "set_budget", slog.String("team_id", team), slog.Int64("limit_micros", *body.LimitMicros))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) setLimits(w http.ResponseWriter, r *http.Request) {
	var limits auth.TeamLimits
	if !decode(w, r, &limits) {
		return
	}
	team := r.PathValue("team")
	if err := s.teams.SetLimits(r.Context(), team, limits); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), "set_limits", slog.String("team_id", team))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listKeys(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if _, err := s.teams.Get(r.Context(), team); err != nil {
		s.fail(w, r, err)
		return
	}
	keys, err := s.keys.List(r.Context(), team)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// createKey returns the raw key. This is the only time it exists outside the client's
// hands: only its hash is stored, and it's never logged.
func (s *server) createKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	team := r.PathValue("team")
	key, err := s.keys.Create(r.Context(), team, body.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), "create_key", slog.String("team_id", team), slog.String("key_id", key.ID))
	writeJSON(w, http.StatusCreated, struct {
		ID     string `json:"id"`
		TeamID string `json:"team_id"`
		Name   string `json:"name"`
		Key    string `json:"key"`
	}{key.ID, team, body.Name, key.Raw})
}

func (s *server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("key")
	if err := s.keys.Revoke(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), "revoke_key", slog.String("key_id", id))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.ledger.Usage(r.Context(), r.PathValue("team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *server) attempts(w http.ResponseWriter, r *http.Request) {
	limit := defaultAttemptLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxAttemptLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and "+strconv.Itoa(maxAttemptLimit))
			return
		}
		limit = n
	}
	team := r.PathValue("team")
	if _, err := s.teams.Get(r.Context(), team); err != nil {
		s.fail(w, r, err)
		return
	}
	attempts, err := s.ledger.Attempts(r.Context(), team, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, attempts)
}

// lateUsage applies a usage report that arrived after an attempt ended, such as one
// copied from a provider's usage export. It corrects a charge at most once.
func (s *server) lateUsage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PromptTokens           int64  `json:"prompt_tokens"`
		CompletionTokens       int64  `json:"completion_tokens"`
		CachedPromptTokens     *int64 `json:"cached_prompt_tokens"`
		CacheWritePromptTokens *int64 `json:"cache_write_prompt_tokens"`
	}
	if !decode(w, r, &body) {
		return
	}
	negative := body.PromptTokens < 0 || body.CompletionTokens < 0 ||
		(body.CachedPromptTokens != nil && *body.CachedPromptTokens < 0) || (body.CacheWritePromptTokens != nil && *body.CacheWritePromptTokens < 0)
	if negative {
		writeError(w, http.StatusBadRequest, "invalid_usage", "token counts can't be negative")
		return
	}
	usage := &chat.Usage{
		State:                  chat.UsageReported,
		PromptTokens:           body.PromptTokens,
		CompletionTokens:       body.CompletionTokens,
		TotalTokens:            body.PromptTokens + body.CompletionTokens,
		CachedPromptTokens:     body.CachedPromptTokens,
		CacheWritePromptTokens: body.CacheWritePromptTokens,
	}
	id := r.PathValue("attempt")
	applied, err := s.ledger.ApplyLateUsage(r.Context(), id, usage)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), "late_usage", slog.String("attempt_id", id), slog.Bool("applied", applied))
	writeJSON(w, http.StatusOK, struct {
		Applied bool `json:"applied"`
	}{applied})
}

// providers reports each route's breaker and hold, as seen by this replica. Breakers are
// per process, so another replica can see something different.
func (s *server) providers(w http.ResponseWriter, _ *http.Request) {
	type aliasStatus struct {
		Alias  string                `json:"alias"`
		Routes []routing.RouteStatus `json:"routes"`
	}
	out := []aliasStatus{}
	for _, a := range s.routes.Aliases() {
		out = append(out, aliasStatus{Alias: a.Name, Routes: a.Status()})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) reconcile(w http.ResponseWriter, r *http.Request) {
	result, err := s.ledger.Reconcile(r.Context(), reconcileBatch)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), "reconcile", slog.Int("released", result.Released), slog.Int("marked_uncertain", result.MarkedUncertain),
		slog.Int("charged_conservatively", result.ChargedConservatively))
	writeJSON(w, http.StatusOK, result)
}
