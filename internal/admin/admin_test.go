package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/admin"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/routing"
	"github.com/Bispulm/llmgate/internal/testenv"
)

const token = "an-admin-token-for-tests"

type env struct {
	t      *testing.T
	url    string
	pool   *pgxpool.Pool
	keys   *auth.KeyStore
	ledger *accounting.Ledger
	client *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testenv.Postgres(t, 8)
	book, err := accounting.EnsurePrices(context.Background(), pool, accounting.PriceList{Label: "v1", Entries: []accounting.PriceEntry{{
		Provider: "p", Model: "m", Prices: accounting.Prices{Input: 1_000_000, CachedInput: 100_000, CacheWrite: 1_000_000, Output: 2_000_000},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, pool: pool,
		keys:   auth.NewKeyStore(auth.KeyStoreOptions{Pool: pool, CacheTTL: time.Minute}),
		ledger: accounting.NewLedger(accounting.LedgerOptions{Pool: pool, Prices: book, UncertainGrace: time.Minute}),
	}
	ts := httptest.NewServer(admin.New(admin.Options{
		Token:  token,
		Teams:  auth.NewTeamStore(pool),
		Keys:   e.keys,
		Ledger: e.ledger,
		Routes: routing.NewTable([]routing.Alias{{Name: "fast", MaxOutputTokens: 10, Routes: []routing.Route{{ProviderName: "p", Model: "m"}}}}, routing.Options{}),
	}))
	t.Cleanup(ts.Close)
	e.url, e.client = ts.URL, ts.Client()
	return e
}

func (e *env) do(method, path, body string, out any) int {
	e.t.Helper()
	return e.doWith(token, method, path, body, out)
}

func (e *env) doWith(bearer, method, path, body string, out any) int {
	e.t.Helper()
	req, err := http.NewRequest(method, e.url+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
	return resp.StatusCode
}

func TestTheAdminAPINeedsItsToken(t *testing.T) {
	e := newEnv(t)
	for name, bearer := range map[string]string{"none": "", "wrong": "not-the-token", "a prefix": token[:10], "a client key": "lgk_0123_abc"} {
		t.Run(name, func(t *testing.T) {
			if got := e.doWith(bearer, http.MethodGet, "/admin/v1/teams", "", nil); got != http.StatusUnauthorized {
				t.Fatalf("status = %d", got)
			}
		})
	}
	if got := e.do(http.MethodGet, "/admin/v1/teams", "", nil); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
}

func TestTeamsKeysAndBudgets(t *testing.T) {
	e := newEnv(t)
	var team map[string]any
	if got := e.do(http.MethodPost, "/admin/v1/teams", `{"id":"acme","budget_limit_micros":5000000,"limits":{"requests_per_minute":60}}`, &team); got != http.StatusCreated {
		t.Fatalf("status = %d: %v", got, team)
	}
	if got := e.do(http.MethodPost, "/admin/v1/teams", `{"id":"acme","budget_limit_micros":1}`, nil); got != http.StatusConflict {
		t.Fatalf("duplicate: status = %d", got)
	}
	if got := e.do(http.MethodPost, "/admin/v1/teams", `{"id":"Bad Name"}`, nil); got != http.StatusBadRequest {
		t.Fatalf("invalid: status = %d", got)
	}
	if got := e.do(http.MethodPost, "/admin/v1/teams", `{"id":"x","surprise":1}`, nil); got != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d", got)
	}

	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if got := e.do(http.MethodPost, "/admin/v1/teams/acme/keys", `{"name":"ci"}`, &created); got != http.StatusCreated {
		t.Fatalf("status = %d", got)
	}
	principal, err := e.keys.Authenticate(context.Background(), created.Key)
	if err != nil || principal.TeamID != "acme" || principal.Limits.RequestsPerMinute != 60 {
		t.Fatalf("principal = %+v, %v", principal, err)
	}
	if got := e.do(http.MethodPost, "/admin/v1/teams/nobody/keys", `{"name":"x"}`, nil); got != http.StatusNotFound {
		t.Fatalf("unknown team: status = %d", got)
	}

	var listed []map[string]any
	if got := e.do(http.MethodGet, "/admin/v1/teams/acme/keys", "", &listed); got != http.StatusOK || len(listed) != 1 {
		t.Fatalf("status = %d: %v", got, listed)
	}
	for field, value := range listed[0] {
		if s, ok := value.(string); ok && strings.Contains(created.Key, s) && len(s) > len(created.ID) {
			t.Fatalf("a listing shows the key's secret in %s", field)
		}
	}

	if got := e.do(http.MethodDelete, "/admin/v1/keys/"+created.ID, "", nil); got != http.StatusNoContent {
		t.Fatalf("revoke: status = %d", got)
	}
	if _, err := e.keys.Authenticate(context.Background(), created.Key); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("a revoked key still works: %v", err)
	}
	if got := e.do(http.MethodDelete, "/admin/v1/keys/lgk_missing", "", nil); got != http.StatusNotFound {
		t.Fatalf("unknown key: status = %d", got)
	}

	if got := e.do(http.MethodPut, "/admin/v1/teams/acme/budget", `{"limit_micros":7000000}`, nil); got != http.StatusNoContent {
		t.Fatalf("budget: status = %d", got)
	}
	if got := e.do(http.MethodPut, "/admin/v1/teams/acme/budget", `{"limit_micros":-1}`, nil); got != http.StatusBadRequest {
		t.Fatalf("negative budget: status = %d", got)
	}
	if got := e.do(http.MethodPut, "/admin/v1/teams/acme/limits", `{"requests_per_minute":5,"tokens_per_minute":0,"max_in_flight":2}`, nil); got != http.StatusNoContent {
		t.Fatalf("limits: status = %d", got)
	}
	var detail struct {
		Team   auth.Team         `json:"team"`
		Budget accounting.Budget `json:"budget"`
	}
	if got := e.do(http.MethodGet, "/admin/v1/teams/acme", "", &detail); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if detail.Budget.LimitMicros != 7_000_000 || detail.Team.Limits.MaxInFlight != 2 || detail.Team.Limits.RequestsPerMinute != 5 {
		t.Fatalf("detail = %+v", detail)
	}
	if got := e.do(http.MethodGet, "/admin/v1/teams/nobody", "", nil); got != http.StatusNotFound {
		t.Fatalf("status = %d", got)
	}
}

func (e *env) settledAttempt(teamID, requestID string) string {
	e.t.Helper()
	ctx := context.Background()
	key, err := e.keys.Create(ctx, teamID, "k")
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := e.ledger.Reserve(ctx, accounting.ReserveRequest{RequestID: requestID, TeamID: teamID, KeyID: key.ID, Alias: "fast", Number: 1, Provider: "p", Model: "m", InputBound: 100, OutputBound: 10, Lease: time.Minute})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.ledger.MarkDispatched(ctx, r.AttemptID, time.Minute); err != nil {
		e.t.Fatal(err)
	}
	return r.AttemptID
}

func TestUsageIsPerTeamAndLateUsageApplies(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"a", "b"} {
		if got := e.do(http.MethodPost, "/admin/v1/teams", `{"id":"`+id+`","budget_limit_micros":100000}`, nil); got != http.StatusCreated {
			t.Fatalf("status = %d", got)
		}
	}
	attempt := e.settledAttempt("a", "req-a")
	if _, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{AttemptID: attempt, Outcome: accounting.OutcomeClientCanceled}); err != nil {
		t.Fatal(err)
	}

	var usage accounting.Usage
	if got := e.do(http.MethodGet, "/admin/v1/teams/a/usage", "", &usage); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if usage.Requests != 1 || usage.Attempts[accounting.StateUncertain] != 1 || usage.Budget.ReservedMicros == 0 {
		t.Fatalf("usage = %+v", usage)
	}
	var other accounting.Usage
	if got := e.do(http.MethodGet, "/admin/v1/teams/b/usage", "", &other); got != http.StatusOK || other.Requests != 0 || other.Budget.ReservedMicros != 0 {
		t.Fatalf("another team's usage leaked: %d %+v", got, other)
	}

	var applied struct {
		Applied bool `json:"applied"`
	}
	if got := e.do(http.MethodPost, "/admin/v1/attempts/"+attempt+"/usage", `{"prompt_tokens":40,"completion_tokens":10}`, &applied); got != http.StatusOK || !applied.Applied {
		t.Fatalf("status = %d, applied = %v", got, applied.Applied)
	}
	var attempts []accounting.AttemptRecord
	if got := e.do(http.MethodGet, "/admin/v1/teams/a/attempts?limit=5", "", &attempts); got != http.StatusOK || len(attempts) != 1 {
		t.Fatalf("status = %d: %+v", got, attempts)
	}
	if attempts[0].ChargeMicros == nil || *attempts[0].ChargeMicros != 60 || attempts[0].ChargeKind != accounting.ChargeReported {
		t.Fatalf("attempt = %+v", attempts[0])
	}
	if got := e.do(http.MethodPost, "/admin/v1/attempts/"+attempt+"/usage", `{"prompt_tokens":-1,"completion_tokens":1}`, nil); got != http.StatusBadRequest {
		t.Fatalf("negative usage: status = %d", got)
	}
	if got := e.do(http.MethodPost, "/admin/v1/attempts/att_missing/usage", `{"prompt_tokens":1,"completion_tokens":1}`, nil); got != http.StatusNotFound {
		t.Fatalf("unknown attempt: status = %d", got)
	}
	if got := e.do(http.MethodGet, "/admin/v1/teams/a/attempts?limit=nope", "", nil); got != http.StatusBadRequest {
		t.Fatalf("bad limit: status = %d", got)
	}
}

func TestReconcileAndProviderStatus(t *testing.T) {
	e := newEnv(t)
	if got := e.do(http.MethodPost, "/admin/v1/teams", `{"id":"a","budget_limit_micros":100000}`, nil); got != http.StatusCreated {
		t.Fatalf("status = %d", got)
	}
	var result accounting.ReconcileResult
	if got := e.do(http.MethodPost, "/admin/v1/reconcile", "", &result); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	var status []struct {
		Alias  string                `json:"alias"`
		Routes []routing.RouteStatus `json:"routes"`
	}
	if got := e.do(http.MethodGet, "/admin/v1/providers", "", &status); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if len(status) != 1 || status[0].Alias != "fast" || len(status[0].Routes) != 1 || status[0].Routes[0].Breaker.State != routing.BreakerClosed {
		t.Fatalf("status = %+v", status)
	}
	_ = chat.UsageReported
}
