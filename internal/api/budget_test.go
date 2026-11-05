package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/cache"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/metrics"
	"github.com/Bispulm/llmgate/internal/mock"
	"github.com/Bispulm/llmgate/internal/provider/openai"
	"github.com/Bispulm/llmgate/internal/routing"
	"github.com/Bispulm/llmgate/internal/testenv"
)

// tenantEnv runs real gateways with real keys and a real ledger, on real Postgres.
type tenantEnv struct {
	t        *testing.T
	url      string
	pool     *pgxpool.Pool
	mock     *mock.Server
	upstream *httptest.Server
	book     *accounting.PriceBook
	keys     *auth.KeyStore
	ledger   *accounting.Ledger
}

// priced is a model string with a price. Each token costs 1 micro-USD on input and 2 on
// output. The chat body's prompt has a small input bound, so reservations stay small.
func priceList(models ...string) accounting.PriceList {
	list := accounting.PriceList{Label: "test"}
	for _, m := range models {
		list.Entries = append(list.Entries, accounting.PriceEntry{
			Provider: "openai", Model: m,
			Prices: accounting.Prices{Input: 1_000_000, CachedInput: 100_000, CacheWrite: 1_000_000, Output: 2_000_000},
		})
	}
	return list
}

func newTenantEnv(t *testing.T, models ...string) *tenantEnv {
	t.Helper()
	url := testenv.PostgresURL(t)
	pool := testenv.PoolFor(t, url, 16)
	book, err := accounting.EnsurePrices(context.Background(), pool, priceList(models...))
	if err != nil {
		t.Fatal(err)
	}
	m := mock.New()
	upstream := httptest.NewServer(m)
	t.Cleanup(upstream.Close)
	return &tenantEnv{
		t: t, url: url, pool: pool, mock: m, upstream: upstream, book: book,
		keys:   auth.NewKeyStore(auth.KeyStoreOptions{Pool: pool, CacheTTL: time.Minute}),
		ledger: accounting.NewLedger(accounting.LedgerOptions{Pool: pool, Prices: book, UncertainGrace: time.Minute}),
	}
}

// addTeam creates a team and returns a key for it.
func (e *tenantEnv) addTeam(name string, budgetMicros int64) string {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO teams (id, name, budget_limit_micros) VALUES ($1, $1, $2)`, name, budgetMicros); err != nil {
		e.t.Fatal(err)
	}
	key, err := e.keys.Create(context.Background(), name, "k")
	if err != nil {
		e.t.Fatal(err)
	}
	return key.Raw
}

type instanceOptions struct {
	ledger      Ledger
	limiter     TeamLimiter
	logger      *slog.Logger
	adjust      func(*Limits)
	keyCacheTTL time.Duration
	cache       *cache.Cache
	cacheTTL    time.Duration
	metrics     *metrics.Metrics
}

// instance runs one gateway replica, with its own connection pool and adapters. Alias
// "fast" has one route per model, in tier order.
func (e *tenantEnv) instance(opts instanceOptions, models ...string) (string, *http.Client) {
	e.t.Helper()
	pool := testenv.PoolFor(e.t, e.url, 8)
	adapter, err := openai.New(openai.Options{BaseURL: e.upstream.URL + "/v1", APIKey: testProviderKey, MaxResponseBytes: 1 << 20, MaxEventBytes: 1 << 16})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(adapter.CloseIdleConnections)
	routes := make([]routing.Route, len(models))
	for i, m := range models {
		routes[i] = routing.Route{ProviderName: "openai", Provider: adapter, Model: m, Tier: i}
	}
	ledger := opts.ledger
	if ledger == nil {
		ledger = accounting.NewLedger(accounting.LedgerOptions{Pool: pool, Prices: e.book, UncertainGrace: time.Minute})
	}
	serverLimits := testLimits()
	if opts.adjust != nil {
		opts.adjust(&serverLimits)
	}
	keyCacheTTL := opts.keyCacheTTL
	if keyCacheTTL == 0 {
		keyCacheTTL = time.Minute
	}
	ts := httptest.NewServer(New(Options{
		Logger:      opts.logger,
		Auth:        auth.NewKeyStore(auth.KeyStoreOptions{Pool: pool, CacheTTL: keyCacheTTL}),
		Recorder:    accounting.NewLogRecorder(slog.New(slog.DiscardHandler)),
		Ledger:      ledger,
		TeamLimiter: opts.limiter,
		Cache:       opts.cache,
		Metrics:     opts.metrics,
		Limits:      serverLimits,
		Routes: routing.NewTable([]routing.Alias{{Name: "fast", MaxOutputTokens: 10, Routes: routes, CacheTTL: opts.cacheTTL}},
			routing.Options{Breaker: routing.BreakerConfig{MinRequests: 100000}}),
	}))
	e.t.Cleanup(ts.Close)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 512, MaxConnsPerHost: 0}}
	e.t.Cleanup(client.CloseIdleConnections)
	return ts.URL, client
}

func call(t *testing.T, client *http.Client, url, key, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

type periodRow struct{ limit, settled, reserved int64 }

func (e *tenantEnv) period(team string) periodRow {
	e.t.Helper()
	var p periodRow
	err := e.pool.QueryRow(context.Background(), `
		SELECT limit_micros, settled_micros, reserved_micros FROM budget_periods WHERE team_id = $1
		ORDER BY starts_at DESC LIMIT 1`, team).Scan(&p.limit, &p.settled, &p.reserved)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *tenantEnv) attemptStates(team string) map[string]int {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT state, count(*) FROM attempts WHERE team_id = $1 GROUP BY state`, team)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			e.t.Fatal(err)
		}
		out[state] = count
	}
	return out
}

// waitForStates polls because settlement runs as the handler finishes, just after
// the client has seen the end of the response.
func (e *tenantEnv) waitForStates(team string, want map[string]int) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := e.attemptStates(team)
		if fmt.Sprint(got) == fmt.Sprint(want) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("attempt states = %v, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestACompletedRequestIsChargedForWhatItUsed(t *testing.T) {
	e := newTenantEnv(t, "alpha;tokens=3")
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{}, "alpha;tokens=3")

	resp, body := call(t, client, url, key, chatBody(false, ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get(headerBudgetEnforcement); got != "strict" {
		t.Fatalf("enforcement header = %q", got)
	}
	e.waitForStates("a", map[string]int{accounting.StateReconciled: 1})

	var prompt, completion, charge int64
	var kind string
	err := e.pool.QueryRow(context.Background(), `SELECT prompt_tokens, completion_tokens, charge_micros, charge_kind FROM attempts WHERE team_id = 'a'`).Scan(&prompt, &completion, &charge, &kind)
	if err != nil {
		t.Fatal(err)
	}
	if completion != 3 || charge != prompt+2*completion || kind != accounting.ChargeReported {
		t.Fatalf("attempt = %d %d %d %s", prompt, completion, charge, kind)
	}
	if got := e.period("a"); got.settled != charge || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
}

func TestAStreamIsChargedEvenWhenTheClientDidNotAskForUsage(t *testing.T) {
	e := newTenantEnv(t, "alpha;tokens=3")
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{}, "alpha;tokens=3")

	_, body := call(t, client, url, key, chatBody(true, ""))
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("body = %s", body)
	}
	e.waitForStates("a", map[string]int{accounting.StateReconciled: 1})
	if got := e.period("a"); got.settled == 0 || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
}

func TestAnExhaustedBudgetRefusesBeforeAnyUpstreamCall(t *testing.T) {
	e := newTenantEnv(t, "costly")
	key := e.addTeam("a", 10) // Far less than one reservation.
	url, client := e.instance(instanceOptions{}, "costly")

	resp, body := call(t, client, url, key, chatBody(false, ""))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"code":"budget_exhausted"`) || !strings.Contains(body, `"type":"insufficient_quota"`) {
		t.Fatalf("body = %s", body)
	}
	if got := e.mock.Stats()["costly"].Requests; got != 0 {
		t.Fatalf("the upstream saw %d requests", got)
	}
	if got := e.attemptStates("a"); len(got) != 0 {
		t.Fatalf("attempts = %v", got)
	}
}

func TestBudgetsBelongToTheirTeam(t *testing.T) {
	e := newTenantEnv(t, "alpha;tokens=1")
	exhausted := e.addTeam("exhausted", 10)
	healthy := e.addTeam("healthy", 100_000)
	url, client := e.instance(instanceOptions{}, "alpha;tokens=1")

	if resp, _ := call(t, client, url, exhausted, chatBody(false, "")); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exhausted team: status = %d", resp.StatusCode)
	}
	if resp, body := call(t, client, url, healthy, chatBody(false, "")); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy team: status = %d: %s", resp.StatusCode, body)
	}
	e.waitForStates("healthy", map[string]int{accounting.StateReconciled: 1})
	if got := e.period("healthy"); got.settled == 0 {
		t.Fatalf("period = %+v", got)
	}
}

func TestARevokedKeyStopsWithinTheCacheBound(t *testing.T) {
	const bound = 100 * time.Millisecond
	e := newTenantEnv(t, "alpha;tokens=1")
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{keyCacheTTL: bound}, "alpha;tokens=1")

	if resp, body := call(t, client, url, key, chatBody(false, "")); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	parts := strings.SplitN(key, "_", 3)
	if err := e.keys.Revoke(context.Background(), parts[0]+"_"+parts[1]); err != nil {
		t.Fatal(err)
	}
	revoked := time.Now()
	time.Sleep(bound)
	for {
		resp, _ := call(t, client, url, key, chatBody(false, ""))
		if resp.StatusCode == http.StatusUnauthorized {
			return
		}
		if time.Since(revoked) > bound+2*time.Second {
			t.Fatalf("still status %d long after the bound", resp.StatusCode)
		}
	}
}

type brokenAuth struct{}

func (brokenAuth) Authenticate(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, fmt.Errorf("%w: connection refused", auth.ErrUnavailable)
}

// A valid key isn't invalid just because it couldn't be checked: a client told so might
// throw away a good key, and an operator would look in the wrong place.
func TestAnAuthenticationOutageIsNotAnInvalidKey(t *testing.T) {
	ts := httptest.NewServer(New(Options{Auth: brokenAuth{}, Limits: testLimits()}))
	defer ts.Close()
	resp, body := call(t, ts.Client(), ts.URL, "lgk_abc_def", chatBody(false, ""))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeAuthUnavailable) {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "connection refused") {
		t.Fatalf("an internal error reached the client: %s", body)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("no Retry-After")
	}
}

func TestATeamCantBeChosenByTheRequest(t *testing.T) {
	e := newTenantEnv(t, "alpha;tokens=1")
	key := e.addTeam("mine", 100_000)
	e.addTeam("theirs", 100_000)
	url, client := e.instance(instanceOptions{}, "alpha;tokens=1")

	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(chatBody(false, "")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Team-Id", "theirs")
	req.Header.Set("OpenAI-Organization", "theirs")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	e.waitForStates("mine", map[string]int{accounting.StateReconciled: 1})
	if got := e.attemptStates("theirs"); len(got) != 0 {
		t.Fatalf("another team was charged: %v", got)
	}
}

// Every attempt is accounted, including one that failed before the one that served.
func TestFallbackAccountsForEachAttemptSeparately(t *testing.T) {
	const failing, working = "primary;fail=status;status=500", "backup;tokens=2"
	e := newTenantEnv(t, failing, working)
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{}, failing, working)

	resp, body := call(t, client, url, key, chatBody(false, ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	e.waitForStates("a", map[string]int{accounting.StateReconciled: 2})

	rows, err := e.pool.Query(context.Background(), `SELECT number, charge_kind, charge_micros, upstream_status FROM attempts WHERE team_id = 'a' ORDER BY number`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		number int
		kind   string
		charge int64
		status *int
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.number, &r.kind, &r.charge, &r.status); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 || got[0].kind != accounting.ChargeRejected || got[0].charge != 0 || got[0].status == nil || *got[0].status != 500 {
		t.Fatalf("attempts = %+v", got)
	}
	if got[1].kind != accounting.ChargeReported || got[1].charge == 0 {
		t.Fatalf("attempts = %+v", got)
	}
}

// A failed attempt whose cost is unknown keeps its liability, and the fallback has to fit
// alongside it.
func TestAFallbackMustFitAlongsideWhatAFailedAttemptMayHaveCost(t *testing.T) {
	const failing, working = "primary;fail=drop_before_headers", "backup;tokens=1"
	e := newTenantEnv(t, failing, working)
	url, client := e.instance(instanceOptions{}, failing, working)

	// Enough for one reservation, but not two.
	req, err := chat.DecodeRequest([]byte(chatBody(false, "")))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := accounting.Prices{Input: 1_000_000, CachedInput: 100_000, CacheWrite: 1_000_000, Output: 2_000_000}.
		Reservation(accounting.InputBound(req), 10)
	if err != nil {
		t.Fatal(err)
	}
	key := e.addTeam("a", reservation+reservation/2)

	resp, body := call(t, client, url, key, chatBody(false, ""))
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "budget_exhausted") {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := e.mock.Stats()["backup"].Requests; got != 0 {
		t.Fatalf("the fallback ran without enough budget: %d", got)
	}
	e.waitForStates("a", map[string]int{accounting.StateUncertain: 1})
	if got := e.period("a"); got.reserved != reservation {
		t.Fatalf("period = %+v: the failed attempt's liability must be held", got)
	}
}

func TestUnknownCostsKeepTheirLiability(t *testing.T) {
	for name, model := range map[string]string{
		"no usage reported":     "a;usage=none;tokens=1",
		"stream interrupted":    "a;fail=disconnect;after=1;tokens=3",
		"connection dropped":    "a;fail=drop_before_headers",
		"missing end of stream": "a;terminator=none;usage=none;tokens=1",
	} {
		t.Run(name, func(t *testing.T) {
			e := newTenantEnv(t, model)
			key := e.addTeam("a", 100_000)
			url, client := e.instance(instanceOptions{}, model)
			call(t, client, url, key, chatBody(true, ""))
			e.waitForStates("a", map[string]int{accounting.StateUncertain: 1})
			if got := e.period("a"); got.reserved == 0 || got.settled != 0 {
				t.Fatalf("period = %+v", got)
			}
		})
	}
}

func TestACanceledRequestIsStillAccounted(t *testing.T) {
	const model = "slow;tokens=1000;interval=10ms"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{}, model)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(chatBody(true, "")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	cancel()

	// Accounting uses a context of its own, so the cancellation can't prevent it.
	e.waitForStates("a", map[string]int{accounting.StateUncertain: 1})
}

func TestUnpricedRoutesAreSkipped(t *testing.T) {
	const priced = "priced;tokens=1"
	e := newTenantEnv(t, priced)
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{}, "unpriced", priced)

	resp, body := call(t, client, url, key, chatBody(false, ""))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "[priced]") {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := e.mock.Stats()["unpriced"].Requests; got != 0 {
		t.Fatalf("a route with no price saw %d requests", got)
	}
	if got := resp.Header.Get(headerAttempts); got != "1" {
		t.Fatalf("attempts header = %q", got)
	}

	t.Run("with nothing else it's a clear error", func(t *testing.T) {
		url, client := e.instance(instanceOptions{}, "unpriced")
		resp, body := call(t, client, url, key, chatBody(false, ""))
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "no_route_available") {
			t.Fatalf("status = %d: %s", resp.StatusCode, body)
		}
	})
}

// faultyLedger fails on chosen calls.
type faultyLedger struct {
	Ledger
	reserveErr, dispatchErr atomic.Pointer[error]
	finishFailures          atomic.Int64
	finishCalls             atomic.Int64
}

func (l *faultyLedger) Reserve(ctx context.Context, r accounting.ReserveRequest) (*accounting.Reservation, error) {
	if err := l.reserveErr.Load(); err != nil {
		return nil, *err
	}
	return l.Ledger.Reserve(ctx, r)
}

func (l *faultyLedger) MarkDispatched(ctx context.Context, id string, lease time.Duration) error {
	if err := l.dispatchErr.Load(); err != nil {
		return *err
	}
	return l.Ledger.MarkDispatched(ctx, id, lease)
}

func (l *faultyLedger) Finish(ctx context.Context, f accounting.FinishRequest) (accounting.Settlement, error) {
	l.finishCalls.Add(1)
	if l.finishFailures.Load() > 0 {
		l.finishFailures.Add(-1)
		return accounting.Settlement{}, accounting.ErrUnavailable
	}
	return l.Ledger.Finish(ctx, f)
}

func TestALedgerOutageFailsClosed(t *testing.T) {
	const model = "alpha;tokens=1"
	t.Run("nothing is sent if a reservation can't be made", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		faulty := &faultyLedger{Ledger: e.ledger}
		outage := fmt.Errorf("%w: connection refused", accounting.ErrUnavailable)
		faulty.reserveErr.Store(&outage)
		url, client := e.instance(instanceOptions{ledger: faulty}, model)

		resp, body := call(t, client, url, key, chatBody(false, ""))
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "accounting_unavailable") {
			t.Fatalf("status = %d: %s", resp.StatusCode, body)
		}
		if strings.Contains(body, "connection refused") {
			t.Fatalf("an internal error reached the client: %s", body)
		}
		if got := e.mock.Stats()["alpha"].Requests; got != 0 {
			t.Fatalf("the upstream saw %d requests with no way to account for them", got)
		}
	})
	t.Run("nothing is sent if dispatch can't be recorded", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		faulty := &faultyLedger{Ledger: e.ledger}
		outage := fmt.Errorf("%w: connection refused", accounting.ErrUnavailable)
		faulty.dispatchErr.Store(&outage)
		url, client := e.instance(instanceOptions{ledger: faulty}, model)

		resp, body := call(t, client, url, key, chatBody(false, ""))
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "accounting_unavailable") {
			t.Fatalf("status = %d: %s", resp.StatusCode, body)
		}
		if got := e.mock.Stats()["alpha"].Requests; got != 0 {
			t.Fatalf("the upstream saw %d requests", got)
		}
		// This replica knows nothing was sent, so the money comes straight back.
		if got := e.attemptStates("a"); got[accounting.StateReleased] != 1 {
			t.Fatalf("states = %v", got)
		}
		if got := e.period("a"); got.reserved != 0 || got.settled != 0 {
			t.Fatalf("period = %+v", got)
		}
	})
	t.Run("an attempt released by a reconciler isn't sent", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		faulty := &faultyLedger{Ledger: e.ledger}
		released := accounting.ErrNotReserved
		faulty.dispatchErr.Store(&released)
		url, client := e.instance(instanceOptions{ledger: faulty}, model)
		if resp, _ := call(t, client, url, key, chatBody(false, "")); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := e.mock.Stats()["alpha"].Requests; got != 0 {
			t.Fatalf("the upstream saw %d requests", got)
		}
	})
}

func TestSettlementRetriesAndLeavesTheAttemptSafeIfItCant(t *testing.T) {
	const model = "alpha;tokens=1"
	t.Run("a brief failure is retried", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		faulty := &faultyLedger{Ledger: e.ledger}
		faulty.finishFailures.Store(2)
		url, client := e.instance(instanceOptions{ledger: faulty}, model)
		if resp, _ := call(t, client, url, key, chatBody(false, "")); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		e.waitForStates("a", map[string]int{accounting.StateReconciled: 1})
		if got := faulty.finishCalls.Load(); got != 3 {
			t.Fatalf("finish was called %d times", got)
		}
	})
	t.Run("if it keeps failing the client still gets its answer, and the liability stays", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		faulty := &faultyLedger{Ledger: e.ledger}
		faulty.finishFailures.Store(1000)
		url, client := e.instance(instanceOptions{ledger: faulty}, model)
		resp, body := call(t, client, url, key, chatBody(false, ""))
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "[alpha]") {
			t.Fatalf("status = %d: %s", resp.StatusCode, body)
		}
		// Left dispatched, a reconciler treats it as uncertain and charges conservatively.
		if got := e.attemptStates("a"); got[accounting.StateDispatched] != 1 {
			t.Fatalf("states = %v", got)
		}
		if got := e.period("a"); got.reserved == 0 {
			t.Fatalf("period = %+v", got)
		}
	})
}

type denyingLimiter struct {
	decision limits.Decision
	err      error
	debited  atomic.Int64
	released atomic.Int64
}

func (l *denyingLimiter) Admit(context.Context, string, limits.TeamLimits, string, time.Duration) (limits.Decision, func(), error) {
	if l.err != nil {
		return limits.Decision{}, func() {}, l.err
	}
	return l.decision, func() { l.released.Add(1) }, nil
}

func (l *denyingLimiter) DebitTokens(_ context.Context, _ string, _ limits.TeamLimits, tokens int64) error {
	l.debited.Add(tokens)
	return nil
}

func TestTeamLimitsComeFirst(t *testing.T) {
	const model = "alpha;tokens=1"
	for name, reason := range map[string]string{"request rate": limits.ReasonRequests, "tokens": limits.ReasonTokens, "in flight": limits.ReasonInFlight} {
		t.Run("over the "+name+" limit", func(t *testing.T) {
			e := newTenantEnv(t, model)
			key := e.addTeam("a", 100_000)
			url, client := e.instance(instanceOptions{limiter: &denyingLimiter{decision: limits.Decision{Reason: reason, RetryAfter: 3 * time.Second}}}, model)
			resp, body := call(t, client, url, key, chatBody(false, ""))
			if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, `"type":"rate_limit_error"`) {
				t.Fatalf("status = %d: %s", resp.StatusCode, body)
			}
			if got := resp.Header.Get("Retry-After"); got != "3" {
				t.Fatalf("Retry-After = %q", got)
			}
			if got := e.mock.Stats()["alpha"].Requests; got != 0 {
				t.Fatalf("the upstream saw %d requests", got)
			}
			if got := e.attemptStates("a"); len(got) != 0 {
				t.Fatalf("attempts = %v", got)
			}
		})
	}
	t.Run("an outage fails closed", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		url, client := e.instance(instanceOptions{limiter: &denyingLimiter{err: limits.ErrLimiterUnavailable}}, model)
		resp, body := call(t, client, url, key, chatBody(false, ""))
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "rate_limit_unavailable") {
			t.Fatalf("status = %d: %s", resp.StatusCode, body)
		}
		if got := e.mock.Stats()["alpha"].Requests; got != 0 {
			t.Fatalf("the upstream saw %d requests", got)
		}
	})
	t.Run("an allowed request gives its slot back and spends its tokens", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		limiter := &denyingLimiter{decision: limits.Decision{Allowed: true}}
		url, client := e.instance(instanceOptions{limiter: limiter}, model)
		if resp, _ := call(t, client, url, key, chatBody(false, "")); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		e.waitForStates("a", map[string]int{accounting.StateReconciled: 1})
		deadline := time.Now().Add(5 * time.Second)
		for limiter.released.Load() != 1 || limiter.debited.Load() == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("released %d, debited %d", limiter.released.Load(), limiter.debited.Load())
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

// With real Redis, an outage of the rate limiter must leave budgets exactly as enforced.
func TestARedisOutageNeverBypassesBudgets(t *testing.T) {
	const model = "alpha;tokens=1"
	e := newTenantEnv(t, model)
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	defer func() { _ = dead.Close() }()
	failOpen := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: dead, Prefix: "x:", FailOpen: true})

	if _, err := e.pool.Exec(context.Background(), `INSERT INTO teams (id, name, budget_limit_micros, requests_per_minute) VALUES ('poor', 'poor', 10, 1000)`); err != nil {
		t.Fatal(err)
	}
	key, err := e.keys.Create(context.Background(), "poor", "k")
	if err != nil {
		t.Fatal(err)
	}
	url, client := e.instance(instanceOptions{limiter: failOpen}, model)

	resp, body := call(t, client, url, key.Raw, chatBody(false, ""))
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "budget_exhausted") {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := e.mock.Stats()["alpha"].Requests; got != 0 {
		t.Fatalf("the upstream saw %d requests", got)
	}
}

func TestTeamLimitsAreSharedAcrossReplicas(t *testing.T) {
	const model = "alpha;tokens=1"
	e := newTenantEnv(t, model)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO teams (id, name, budget_limit_micros, requests_per_minute) VALUES ('a', 'a', 100000, 2)`); err != nil {
		t.Fatal(err)
	}
	key, err := e.keys.Create(context.Background(), "a", "k")
	if err != nil {
		t.Fatal(err)
	}
	client, prefix := testenv.Redis(t)
	limiter := func() TeamLimiter {
		return limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: prefix})
	}
	firstURL, first := e.instance(instanceOptions{limiter: limiter()}, model)
	secondURL, second := e.instance(instanceOptions{limiter: limiter()}, model)

	if resp, _ := call(t, first, firstURL, key.Raw, chatBody(false, "")); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp, _ := call(t, second, secondURL, key.Raw, chatBody(false, "")); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp, body := call(t, first, firstURL, key.Raw, chatBody(false, ""))
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, codeRateLimitExceeded) {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
}

// settlingLedger holds settlement until told to continue.
type settlingLedger struct {
	Ledger
	proceed chan struct{}
}

func (l *settlingLedger) Finish(ctx context.Context, f accounting.FinishRequest) (accounting.Settlement, error) {
	select {
	case <-l.proceed:
	case <-ctx.Done():
	}
	return l.Ledger.Finish(ctx, f)
}

// A client's answer doesn't wait for the books, or a database having a bad moment would
// slow down every response that had already succeeded. A stream's last chunk of HTTP
// framing does wait, since it's written when the handler returns, but by then the client
// has the [DONE] marker that SSE clients stop at.
func TestSettlingDoesntDelayTheResponse(t *testing.T) {
	const model = "alpha;tokens=1"
	for name, stream := range map[string]bool{"non-streaming": false, "streaming": true} {
		t.Run(name, func(t *testing.T) {
			e := newTenantEnv(t, model)
			key := e.addTeam("a", 100_000)
			held := &settlingLedger{Ledger: e.ledger, proceed: make(chan struct{})}
			url, client := e.instance(instanceOptions{ledger: held}, model)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(chatBody(stream, "")))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("the response waited for settlement: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			want := "[alpha]"
			if stream {
				want = "data: [DONE]"
			}
			var got strings.Builder
			buf := make([]byte, 4096)
			for !strings.Contains(got.String(), want) {
				n, err := resp.Body.Read(buf)
				got.Write(buf[:n])
				if err != nil {
					if !strings.Contains(got.String(), want) {
						t.Fatalf("the response waited for settlement: %v, read %q", err, got.String())
					}
					break
				}
			}
			if !stream {
				if rest, err := io.ReadAll(resp.Body); err != nil {
					t.Fatalf("the response waited for settlement: %v, read %q", err, rest)
				}
			}
			close(held.proceed)
			e.waitForStates("a", map[string]int{accounting.StateReconciled: 1})
		})
	}
}

// hungLedger never answers, like a database that has stopped responding.
type hungLedger struct{ Ledger }

func (hungLedger) Reserve(ctx context.Context, _ accounting.ReserveRequest) (*accounting.Reservation, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("%w: %w", accounting.ErrUnavailable, ctx.Err())
}

func TestARequestRunningOutOfTimeDuringAdmissionIsATimeout(t *testing.T) {
	const model = "alpha;tokens=1"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{ledger: hungLedger{e.ledger}, adjust: func(l *Limits) { l.RequestTimeout = 50 * time.Millisecond }}, model)
	resp, body := call(t, client, url, key, chatBody(false, ""))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := e.mock.Stats()["alpha"].Requests; got != 0 {
		t.Fatalf("the upstream saw %d requests", got)
	}
}

// The budget check can't be left waiting on a database that has hung.
func TestAHungLedgerFailsClosedBeforeTheRequestDeadline(t *testing.T) {
	const model = "alpha;tokens=1"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{ledger: hungLedger{e.ledger}, adjust: func(l *Limits) { l.RequestTimeout = time.Minute }}, model)
	start := time.Now()
	resp, body := call(t, client, url, key, chatBody(false, ""))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeAccountingUnavailable) {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed > 2*defaultAdmissionTimeout {
		t.Fatalf("took %v", elapsed)
	}
}

// Tokens a provider may have used count against a team's limit even when it never said
// how many, or cancelling just before the usage report would make the limit optional.
// Like the budget, the count is conservative: the most the attempt could have used.
func TestUnknownUsageCountsAgainstTokenLimits(t *testing.T) {
	req, err := chat.DecodeRequest([]byte(chatBody(true, "")))
	if err != nil {
		t.Fatal(err)
	}
	bound := accounting.InputBound(req) + 10
	for name, tt := range map[string]struct {
		model string
		want  int64
	}{
		"an interrupted stream":          {"cut;fail=disconnect;after=1;tokens=3", bound},
		"a stream with no usage":         {"quiet;usage=none;tokens=1", bound},
		"a refusal costs nothing":        {"refused;fail=status;status=400", 0},
		"a reported count is used as is": {"counted;tokens=3", -1},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTenantEnv(t, tt.model)
			key := e.addTeam("a", 100_000)
			limiter := &denyingLimiter{decision: limits.Decision{Allowed: true}}
			url, client := e.instance(instanceOptions{limiter: limiter}, tt.model)
			call(t, client, url, key, chatBody(true, ""))
			deadline := time.Now().Add(5 * time.Second)
			for limiter.released.Load() != 1 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			got := limiter.debited.Load()
			switch {
			case tt.want < 0 && (got <= 0 || got >= bound):
				t.Fatalf("debited %d, want the reported count", got)
			case tt.want >= 0 && got != tt.want:
				t.Fatalf("debited %d, want %d", got, tt.want)
			}
		})
	}
}
