// Package metrics exports Prometheus metrics. Every label takes values from a small fixed
// set, or from configuration, such as alias, provider, and model names. Nothing a client
// sends, and no request ID, key, or team, becomes a label.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/routing"
)

// other replaces a value outside the expected set, so a label can't grow without bound.
const other = "other"

// Latency buckets in seconds, from a millisecond to two minutes.
var latencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}

var (
	outcomes = set(string(accounting.OutcomeCompleted), string(accounting.OutcomeFailed), string(accounting.OutcomeInterrupted),
		string(accounting.OutcomeClientCanceled), string(accounting.OutcomeClientStalled), string(accounting.OutcomeNotSent), "")
	kinds = set(string(provider.KindConnection), string(provider.KindTimeout), string(provider.KindCanceled), string(provider.KindRateLimited),
		string(provider.KindOverloaded), string(provider.KindServer), string(provider.KindAuth), string(provider.KindBilling),
		string(provider.KindNotFound), string(provider.KindInvalidRequest), string(provider.KindProtocol), string(provider.KindInterrupted), "")
	reasons      = set(string(accounting.ReasonPrimary), string(accounting.ReasonFallback), string(accounting.ReasonRetryAfter), "")
	cacheResults = set("hit", "miss", "bypass")
	limitReasons = set(limits.ReasonRequests, limits.ReasonTokens, limits.ReasonInFlight)
	skipReasons  = set(string(routing.SkipCapability), string(routing.SkipBreakerOpen), string(routing.SkipHeld),
		string(routing.SkipSaturated), string(routing.SkipUnpriced))
	chargeKinds  = set(accounting.ChargeReported, accounting.ChargeRejected, accounting.ChargeConservative)
	dependencies = set("postgres", "redis")
	breakerState = []routing.BreakerState{routing.BreakerClosed, routing.BreakerHalfOpen, routing.BreakerOpen}
)

func set(values ...string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func bounded(allowed map[string]bool, v string) string {
	if allowed[v] {
		return v
	}
	return other
}

// Options configures Metrics.
type Options struct{}

// Metrics holds the gateway's metrics. A nil *Metrics records nothing.
type Metrics struct {
	registry *prometheus.Registry

	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	attempts        *prometheus.CounterVec
	attemptDuration *prometheus.HistogramVec
	firstEvent      *prometheus.HistogramVec
	fallbacks       *prometheus.CounterVec
	skips           *prometheus.CounterVec
	cache           *prometheus.CounterVec
	teamLimits      *prometheus.CounterVec
	budgetRefusals  prometheus.Counter
	charged         *prometheus.CounterVec
	settleFailures  prometheus.Counter
	reconciled      *prometheus.CounterVec
	dependencyErrs  *prometheus.CounterVec
}

// New returns Metrics on a registry of their own, with Go runtime and process metrics.
func New(Options) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_requests_total", Help: "Chat completion requests, by alias, HTTP status, and whether they streamed.",
		}, []string{"alias", "status", "stream"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgate_request_duration_seconds", Help: "Time to finish a chat completion request.", Buckets: latencyBuckets,
		}, []string{"alias", "stream"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_attempts_total", Help: "Upstream attempts, by route, outcome, failure kind, and why they were made.",
		}, []string{"provider", "model", "outcome", "error_kind", "reason"}),
		attemptDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgate_attempt_duration_seconds", Help: "Time taken by each upstream attempt.", Buckets: latencyBuckets,
		}, []string{"provider", "model"}),
		firstEvent: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgate_time_to_first_event_seconds", Help: "Time from a request arriving to the first content of a stream.", Buckets: latencyBuckets,
		}, []string{"provider", "model"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_fallbacks_total", Help: "Attempts made because an earlier attempt for the same request failed before commit.",
		}, []string{"provider", "model"}),
		skips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_route_skips_total", Help: "Routes not used for a request, by why.",
		}, []string{"alias", "reason"}),
		cache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_cache_requests_total", Help: "Requests that opted in to the cache, by result.",
		}, []string{"alias", "result"}),
		teamLimits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_team_limit_denials_total", Help: "Requests refused by a team limit, by which one.",
		}, []string{"reason"}),
		budgetRefusals: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "llmgate_budget_refusals_total", Help: "Attempts refused because a team's budget couldn't cover them.",
		}),
		charged: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_charged_micros_total", Help: "Micro-USD charged when attempts settled, by route and how the charge was decided.",
		}, []string{"provider", "model", "kind"}),
		settleFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "llmgate_settlement_failures_total", Help: "Attempts left for the reconciler because settling them failed.",
		}),
		reconciled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_reconciled_attempts_total", Help: "Attempts resolved by the reconciler, by what it did.",
		}, []string{"action"}),
		dependencyErrs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_dependency_errors_total", Help: "Failed calls to Postgres or Redis, by operation.",
		}, []string{"dependency", "operation"}),
	}
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.requestDuration, m.attempts, m.attemptDuration, m.firstEvent, m.fallbacks, m.skips, m.cache,
		m.teamLimits, m.budgetRefusals, m.charged, m.settleFailures, m.reconciled, m.dependencyErrs)
	return m
}

// Handler serves the metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry returns the registry, for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// RequestFinished records a chat completion request.
func (m *Metrics) RequestFinished(alias string, stream bool, status int, d time.Duration) {
	if m == nil {
		return
	}
	streamed := strconv.FormatBool(stream)
	m.requests.WithLabelValues(alias, strconv.Itoa(status), streamed).Inc()
	m.requestDuration.WithLabelValues(alias, streamed).Observe(d.Seconds())
}

// Attempt records an upstream attempt as it ends.
func (m *Metrics) Attempt(a accounting.Attempt) {
	if m == nil {
		return
	}
	m.attempts.WithLabelValues(a.Provider, a.Model, bounded(outcomes, string(a.Outcome)), bounded(kinds, string(a.ErrorKind)),
		bounded(reasons, string(a.Reason))).Inc()
	m.attemptDuration.WithLabelValues(a.Provider, a.Model).Observe(a.Duration.Seconds())
	if a.TimeToFirstEvent > 0 {
		m.firstEvent.WithLabelValues(a.Provider, a.Model).Observe(a.TimeToFirstEvent.Seconds())
	}
	if a.Reason == accounting.ReasonFallback {
		m.fallbacks.WithLabelValues(a.Provider, a.Model).Inc()
	}
}

// CacheResult records what the cache did for a request that opted in.
func (m *Metrics) CacheResult(alias, result string) {
	if m == nil {
		return
	}
	m.cache.WithLabelValues(alias, bounded(cacheResults, result)).Inc()
}

// RouteSkipped records why routes weren't used for a request.
func (m *Metrics) RouteSkipped(alias string, skipped map[routing.SkipReason]int) {
	if m == nil {
		return
	}
	for reason, n := range skipped {
		m.skips.WithLabelValues(alias, bounded(skipReasons, string(reason))).Add(float64(n))
	}
}

// TeamLimited records a request refused by a team limit.
func (m *Metrics) TeamLimited(reason string) {
	if m == nil {
		return
	}
	m.teamLimits.WithLabelValues(bounded(limitReasons, reason)).Inc()
}

// BudgetRefused records an attempt the budget couldn't cover.
func (m *Metrics) BudgetRefused() {
	if m == nil {
		return
	}
	m.budgetRefusals.Inc()
}

// Charged records a settled charge.
func (m *Metrics) Charged(providerName, model, kind string, micros int64) {
	if m == nil || micros < 0 {
		return
	}
	m.charged.WithLabelValues(providerName, model, bounded(chargeKinds, kind)).Add(float64(micros))
}

// SettlementFailed records an attempt left for the reconciler.
func (m *Metrics) SettlementFailed() {
	if m == nil {
		return
	}
	m.settleFailures.Inc()
}

// Reconciled records what a reconciliation pass did.
func (m *Metrics) Reconciled(r accounting.ReconcileResult) {
	if m == nil {
		return
	}
	m.reconciled.WithLabelValues("released").Add(float64(r.Released))
	m.reconciled.WithLabelValues("marked_uncertain").Add(float64(r.MarkedUncertain))
	m.reconciled.WithLabelValues("charged_conservatively").Add(float64(r.ChargedConservatively))
}

// DependencyError records a failed call to Postgres or Redis.
func (m *Metrics) DependencyError(dependency, operation string) {
	if m == nil {
		return
	}
	m.dependencyErrs.WithLabelValues(bounded(dependencies, dependency), operation).Inc()
}

// WatchRoutes reports each route's breaker, and each provider's queue, when scraped.
// Breakers and queues are per replica, so each replica reports its own.
func (m *Metrics) WatchRoutes(table *routing.Table, limiters map[string]*limits.FairLimiter) {
	if m == nil {
		return
	}
	m.registry.MustRegister(&routeCollector{table: table, limiters: limiters})
}

var (
	breakerDesc = prometheus.NewDesc("llmgate_breaker_state",
		"1 for each route's current circuit breaker state on this replica, and 0 for the others.", []string{"provider", "model", "state"}, nil)
	holdDesc = prometheus.NewDesc("llmgate_route_hold_seconds",
		"How much longer a provider's retry hint keeps a route out of use on this replica.", []string{"provider", "model"}, nil)
	inFlightDesc = prometheus.NewDesc("llmgate_limiter_in_flight", "Attempts running on a provider on this replica.", []string{"provider"}, nil)
	queuedDesc   = prometheus.NewDesc("llmgate_limiter_queued", "Attempts waiting for a provider on this replica.", []string{"provider"}, nil)
	shedDesc     = prometheus.NewDesc("llmgate_limiter_shed_total", "Attempts a provider's queue turned away on this replica.", []string{"provider"}, nil)
)

type routeCollector struct {
	table    *routing.Table
	limiters map[string]*limits.FairLimiter
}

func (c *routeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{breakerDesc, holdDesc, inFlightDesc, queuedDesc, shedDesc} {
		ch <- d
	}
}

func (c *routeCollector) Collect(ch chan<- prometheus.Metric) {
	seen := map[[2]string]bool{}
	for _, alias := range c.table.Aliases() {
		for _, r := range alias.Status() {
			key := [2]string{r.Provider, r.Model}
			if seen[key] {
				continue // Routes are shared between aliases.
			}
			seen[key] = true
			for _, s := range breakerState {
				value := 0.0
				if r.Breaker.State == s {
					value = 1
				}
				ch <- prometheus.MustNewConstMetric(breakerDesc, prometheus.GaugeValue, value, r.Provider, r.Model, string(s))
			}
			ch <- prometheus.MustNewConstMetric(holdDesc, prometheus.GaugeValue, r.HeldFor.Seconds(), r.Provider, r.Model)
		}
	}
	for name, l := range c.limiters {
		stats := l.Stats()
		ch <- prometheus.MustNewConstMetric(inFlightDesc, prometheus.GaugeValue, float64(stats.InFlight), name)
		ch <- prometheus.MustNewConstMetric(queuedDesc, prometheus.GaugeValue, float64(stats.Queued), name)
		ch <- prometheus.MustNewConstMetric(shedDesc, prometheus.CounterValue, float64(stats.Shed), name)
	}
}
