package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/metrics"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/routing"
)

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	ts := httptest.NewServer(m.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestRequestsAndAttemptsAreCounted(t *testing.T) {
	m := metrics.New(metrics.Options{})
	m.RequestFinished("fast", true, http.StatusOK, 120*time.Millisecond)
	m.RequestFinished("fast", false, http.StatusTooManyRequests, time.Millisecond)
	m.Attempt(accounting.Attempt{
		Provider: "openai", Model: "m1", Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindServer,
		Reason: accounting.ReasonPrimary, Duration: 30 * time.Millisecond,
	})
	m.Attempt(accounting.Attempt{
		Provider: "openai", Model: "m2", Outcome: accounting.OutcomeCompleted, Reason: accounting.ReasonFallback,
		Duration: 40 * time.Millisecond, TimeToFirstEvent: 10 * time.Millisecond,
	})
	m.CacheResult("fast", "hit")
	m.RouteSkipped("fast", map[routing.SkipReason]int{routing.SkipBreakerOpen: 2})
	m.TeamLimited(limits.ReasonRequests)
	m.BudgetRefused()
	m.Charged("openai", "m2", accounting.ChargeReported, 1234)
	m.SettlementFailed()
	m.Reconciled(accounting.ReconcileResult{Released: 1, MarkedUncertain: 2, ChargedConservatively: 3})
	m.DependencyError("postgres", "reserve")

	out := scrape(t, m)
	for _, want := range []string{
		`llmgate_requests_total{alias="fast",status="200",stream="true"} 1`,
		`llmgate_requests_total{alias="fast",status="429",stream="false"} 1`,
		`llmgate_attempts_total{error_kind="server",model="m1",outcome="failed",provider="openai",reason="primary"} 1`,
		`llmgate_attempts_total{error_kind="",model="m2",outcome="completed",provider="openai",reason="fallback"} 1`,
		`llmgate_fallbacks_total{model="m2",provider="openai"} 1`,
		`llmgate_cache_requests_total{alias="fast",result="hit"} 1`,
		`llmgate_route_skips_total{alias="fast",reason="breaker_open"} 2`,
		`llmgate_team_limit_denials_total{reason="requests"} 1`,
		`llmgate_budget_refusals_total 1`,
		`llmgate_charged_micros_total{kind="reported",model="m2",provider="openai"} 1234`,
		`llmgate_settlement_failures_total 1`,
		`llmgate_reconciled_attempts_total{action="charged_conservatively"} 3`,
		`llmgate_dependency_errors_total{dependency="postgres",operation="reserve"} 1`,
		`llmgate_request_duration_seconds_count{alias="fast",stream="true"} 1`,
		`llmgate_time_to_first_event_seconds_count{model="m2",provider="openai"} 1`,
		"go_goroutines",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// A nil Metrics does nothing, so code can call it whether or not metrics are configured.
func TestNilMetricsAreSafe(t *testing.T) {
	var m *metrics.Metrics
	m.RequestFinished("a", false, 200, time.Second)
	m.Attempt(accounting.Attempt{})
	m.CacheResult("a", "hit")
	m.RouteSkipped("a", nil)
	m.TeamLimited("requests")
	m.BudgetRefused()
	m.Charged("p", "m", "reported", 1)
	m.SettlementFailed()
	m.Reconciled(accounting.ReconcileResult{})
	m.DependencyError("redis", "admit")
}

func TestBreakersAndQueuesAreReadAtScrapeTime(t *testing.T) {
	now := time.Unix(1000, 0)
	limiter := limits.NewFairLimiter(limits.Config{MaxConcurrency: 1, MaxQueue: 4})
	table := routing.NewTable([]routing.Alias{
		{Name: "a", MaxOutputTokens: 10, Routes: []routing.Route{{ProviderName: "p", Model: "m", Limiter: limiter}}},
		{Name: "b", MaxOutputTokens: 10, Routes: []routing.Route{{ProviderName: "p", Model: "m", Limiter: limiter}}},
	}, routing.Options{Now: func() time.Time { return now }, Breaker: routing.BreakerConfig{MinRequests: 1, ErrorRate: 0.5}})
	m := metrics.New(metrics.Options{})
	m.WatchRoutes(table, map[string]*limits.FairLimiter{"p": limiter})

	alias, _ := table.Resolve("a")
	plan := alias.Plan(&chat.Request{})
	selected, ok := plan.Next()
	if !ok {
		t.Fatal("no route")
	}
	selected.Finish(&provider.Error{Kind: provider.KindServer})

	out := scrape(t, m)
	// The route is shared by two aliases, and is reported once.
	if got := strings.Count(out, `llmgate_breaker_state{model="m",provider="p",state="open"} 1`); got != 1 {
		t.Fatalf("breaker state reported %d times:\n%s", got, out)
	}
	if !strings.Contains(out, `llmgate_limiter_in_flight{provider="p"} 0`) {
		t.Fatalf("no limiter gauge:\n%s", out)
	}
	if n := testutil.CollectAndCount(m.Registry(), "llmgate_breaker_state"); n != 3 {
		t.Fatalf("%d breaker series, want one per state", n)
	}
}

// Labels have to stay bounded, so nothing that comes from a client becomes one.
func TestLabelsAreBounded(t *testing.T) {
	m := metrics.New(metrics.Options{})
	for i := range 50 {
		m.Attempt(accounting.Attempt{Provider: "p", Model: "m", Outcome: accounting.Outcome("made-up-" + string(rune('a'+i%26))), ErrorKind: provider.Kind("weird")})
		m.CacheResult("a", "something-else")
		m.TeamLimited("odd")
	}
	out := scrape(t, m)
	for _, unbounded := range []string{"made-up-", "weird", "something-else", `reason="odd"`} {
		if strings.Contains(out, unbounded) {
			t.Errorf("an unexpected value became a label: %s", unbounded)
		}
	}
}
