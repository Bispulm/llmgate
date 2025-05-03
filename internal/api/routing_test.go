package api

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/mock"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/provider/anthropic"
	"github.com/Bispulm/llmgate/internal/provider/openai"
	"github.com/Bispulm/llmgate/internal/routing"
)

// testRoute describes one route for a routed gateway.
type testRoute struct {
	model     string
	anthropic bool
	tier      int
	weight    int
}

type routedOptions struct {
	breaker     routing.BreakerConfig
	maxAttempts int
	limits      func(*Limits)
}

// newRoutedGateway runs a gateway whose alias "fast" has several real routes, all on
// one mock, which tells them apart by the label in the model string.
func newRoutedGateway(t *testing.T, routes []testRoute, opts routedOptions) *gateway {
	t.Helper()
	m := mock.New()
	upstream := httptest.NewServer(m)
	t.Cleanup(upstream.Close)

	viaOpenAI, err := openai.New(openai.Options{BaseURL: upstream.URL + "/v1", APIKey: testProviderKey, MaxResponseBytes: 1 << 20, MaxEventBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(viaOpenAI.CloseIdleConnections)
	viaAnthropic, err := anthropic.New(anthropic.Options{BaseURL: upstream.URL, APIKey: testProviderKey, MaxResponseBytes: 1 << 20, MaxEventBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(viaAnthropic.CloseIdleConnections)

	aliasRoutes := make([]routing.Route, len(routes))
	for i, r := range routes {
		route := routing.Route{ProviderName: "openai", Provider: viaOpenAI, Model: r.model, Tier: r.tier, Weight: r.weight}
		if r.anthropic {
			route.ProviderName, route.Provider = "anthropic", viaAnthropic
		}
		aliasRoutes[i] = route
	}
	breaker := opts.breaker
	if breaker == (routing.BreakerConfig{}) {
		// Hard to trip by accident, so that tests about something else aren't affected.
		breaker = routing.BreakerConfig{MinRequests: 1000}
	}
	limits := testLimits()
	if opts.limits != nil {
		opts.limits(&limits)
	}
	rec := &attemptRecorder{attempts: make(chan recordedAttempt, 64)}
	ts := httptest.NewServer(New(Options{
		Auth:     auth.NewDevKey(testAPIKey),
		Recorder: rec,
		Limits:   limits,
		Routes: routing.NewTable([]routing.Alias{{
			Name:            "fast",
			MaxOutputTokens: 100,
			MaxAttempts:     opts.maxAttempts,
			Routes:          aliasRoutes,
		}}, routing.Options{Breaker: breaker, Rand: rand.New(rand.NewPCG(1, 2))}),
	}))
	t.Cleanup(ts.Close)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	return &gateway{url: ts.URL, mock: m, recorder: rec, client: client}
}

func tiers(models ...string) []testRoute {
	out := make([]testRoute, len(models))
	for i, m := range models {
		out[i] = testRoute{model: m, tier: i}
	}
	return out
}

func completionText(t *testing.T, resp *http.Response) string {
	t.Helper()
	var got struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("response = %+v", got)
	}
	return got.Choices[0].Message.Content
}

func TestFallbackBeforeCommit(t *testing.T) {
	failures := []struct {
		name, scenario string
		kind           provider.Kind
	}{
		{"server error", "fail=status;status=500", provider.KindServer},
		{"overloaded", "fail=status;status=529", provider.KindOverloaded},
		{"rate limited", "fail=status;status=429", provider.KindRateLimited},
		{"bad credentials", "fail=status;status=401", provider.KindAuth},
		{"billing", "fail=status;status=402", provider.KindBilling},
		{"unknown model", "fail=status;status=404", provider.KindNotFound},
		{"connection dropped", "fail=drop_before_headers", provider.KindConnection},
	}
	streamOnly := []struct {
		name, scenario string
		kind           provider.Kind
	}{
		{"clean end right after headers", "fail=eof_before_first_event", provider.KindInterrupted},
		{"abrupt end right after headers", "fail=disconnect_before_first_event", provider.KindInterrupted},
		{"failure after the prelude", "fail=disconnect;after=0", provider.KindInterrupted},
		{"malformed first event", "fail=malformed;after=0", provider.KindProtocol},
		{"error event first", "fail=stream_error;after=0", provider.KindServer},
	}

	run := func(t *testing.T, scenario string, kind provider.Kind, stream bool) {
		t.Helper()
		g := newRoutedGateway(t, tiers("primary;"+scenario, "backup;tokens=2"), routedOptions{})
		resp := g.post(t, chatBody(stream, ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if stream {
			s := readStream(t, resp.Body)
			if !s.done || s.text != "[backup] token-1" {
				t.Fatalf("stream = %+v", s)
			}
		} else if got := completionText(t, resp); got != "[backup] token-1" {
			t.Fatalf("text = %q", got)
		}
		if got := resp.Header.Get("X-Llmgate-Attempts"); got != "2" {
			t.Errorf("attempts header = %q", got)
		}
		if got := resp.Header.Get("X-Llmgate-Route"); got != "openai/backup;tokens=2" {
			t.Errorf("route header = %q", got)
		}

		first, second := g.recorder.wait(t), g.recorder.wait(t)
		if first.Number != 1 || first.Committed || first.Outcome != accounting.OutcomeFailed || first.ErrorKind != kind {
			t.Errorf("first attempt = %+v", first.Attempt)
		}
		if second.Number != 2 || !second.Committed || second.Outcome != accounting.OutcomeCompleted {
			t.Errorf("second attempt = %+v", second.Attempt)
		}
		if first.RequestID == "" || first.RequestID != second.RequestID {
			t.Errorf("request IDs: %q and %q", first.RequestID, second.RequestID)
		}
		if first.Reason != accounting.ReasonPrimary || second.Reason != accounting.ReasonFallback {
			t.Errorf("reasons: %q and %q", first.Reason, second.Reason)
		}
	}
	for _, f := range failures {
		t.Run(f.name+"/non-streaming", func(t *testing.T) { run(t, f.scenario, f.kind, false) })
		t.Run(f.name+"/streaming", func(t *testing.T) { run(t, f.scenario, f.kind, true) })
	}
	for _, f := range streamOnly {
		t.Run(f.name, func(t *testing.T) { run(t, f.scenario, f.kind, true) })
	}

	t.Run("a slow first event falls back within the deadline", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("primary;ttfb=30s", "backup"), routedOptions{limits: func(l *Limits) {
			l.FirstEventTimeout = 100 * time.Millisecond
		}})
		s := readStream(t, g.post(t, chatBody(true, "")).Body)
		if !s.done || !strings.HasPrefix(s.text, "[backup]") {
			t.Fatalf("stream = %+v", s)
		}
		waitForMock(t, g, "primary", canceledUpstream)
		if a := g.recorder.wait(t); a.ErrorKind != provider.KindTimeout {
			t.Fatalf("attempt = %+v", a.Attempt)
		}
	})
	t.Run("across providers, with the same contract", func(t *testing.T) {
		g := newRoutedGateway(t, []testRoute{
			{model: "primary;fail=status;status=500", tier: 0},
			{model: "backup;tokens=2;thinking=1", tier: 1, anthropic: true},
		}, routedOptions{})
		s := readStream(t, g.post(t, chatBody(true, "")).Body)
		if !s.done || s.text != "[backup] token-1" || s.finish != "stop" {
			t.Fatalf("stream = %+v", s)
		}
	})
	t.Run("all routes failing returns the last failure", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("a;fail=status;status=500", "b;fail=status;status=529"), routedOptions{})
		resp := g.post(t, chatBody(false, ""))
		expectError(t, resp, 503, "upstream_error", "upstream_overloaded")
		if got := resp.Header.Get("X-Llmgate-Attempts"); got != "2" {
			t.Errorf("attempts header = %q", got)
		}
	})
}

// After commit no other provider may be contacted. The mock's counters prove it,
// rather than only what the client saw.
func TestNoFallbackAfterCommit(t *testing.T) {
	for name, scenario := range map[string]string{
		"disconnect":                "fail=disconnect;after=2;tokens=5",
		"malformed event":           "fail=malformed;after=1",
		"oversized event":           "fail=oversized;after=1;size=100000",
		"error event":               "fail=stream_error;after=1",
		"missing completion marker": "terminator=none;tokens=2",
	} {
		t.Run(name, func(t *testing.T) {
			g := newRoutedGateway(t, tiers("primary;"+scenario, "backup"), routedOptions{})
			resp := g.post(t, chatBody(true, ""))
			s := readStream(t, resp.Body)
			if s.done || s.err == nil {
				t.Fatalf("stream = %+v", s)
			}
			if strings.Contains(s.text, "[backup]") {
				t.Fatalf("output was spliced: %q", s.text)
			}
			if got := g.mock.Stats()["backup"].Requests; got != 0 {
				t.Fatalf("the fallback route saw %d requests after commit", got)
			}
			a := g.recorder.wait(t)
			if !a.Committed || a.Outcome != accounting.OutcomeInterrupted {
				t.Fatalf("attempt = %+v", a.Attempt)
			}
		})
	}
	t.Run("a stall after commit", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("primary;tokens=5;interval=30s", "backup"), routedOptions{limits: func(l *Limits) {
			l.StreamIdleTimeout = 100 * time.Millisecond
		}})
		s := readStream(t, g.post(t, chatBody(true, "")).Body)
		if s.done || s.err == nil {
			t.Fatalf("stream = %+v", s)
		}
		if got := g.mock.Stats()["backup"].Requests; got != 0 {
			t.Fatalf("the fallback route saw %d requests", got)
		}
	})
}

func TestNoFallbackForRequestProblems(t *testing.T) {
	t.Run("the provider rejected the request", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("primary;fail=status;status=400", "backup"), routedOptions{})
		expectError(t, g.post(t, chatBody(false, "")), 400, "invalid_request_error", "upstream_rejected")
		if got := g.mock.Stats()["backup"].Requests; got != 0 {
			t.Fatalf("the fallback route saw %d requests", got)
		}
	})
	t.Run("the client went away", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("primary;latency=30s", "backup"), routedOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errs := make(chan error, 1)
		go func() {
			resp, err := g.send(ctx, chatBody(false, ""))
			if err == nil {
				_ = resp.Body.Close()
			}
			errs <- err
		}()
		waitForMock(t, g, "primary", func(s mock.LabelStats) bool { return s.Requests == 1 })
		cancel()
		waitForMock(t, g, "primary", canceledUpstream)
		if a := g.recorder.wait(t); a.Outcome != accounting.OutcomeClientCanceled {
			t.Fatalf("attempt = %+v", a.Attempt)
		}
		if err := <-errs; err == nil {
			t.Error("the canceled request reported success")
		}
		if got := g.mock.Stats()["backup"].Requests; got != 0 {
			t.Fatalf("the fallback route saw %d requests", got)
		}
	})
}

func TestAttemptsAreBounded(t *testing.T) {
	t.Run("by the attempt limit", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("a;fail=status;status=500", "b;fail=status;status=500", "c"), routedOptions{maxAttempts: 2})
		resp := g.post(t, chatBody(false, ""))
		expectError(t, resp, 502, "upstream_error", "upstream_failed")
		if got := g.mock.Stats()["c"].Requests; got != 0 {
			t.Fatalf("a third attempt was made: %d", got)
		}
	})
	t.Run("by one end-to-end deadline", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("a;latency=30s", "b"), routedOptions{limits: func(l *Limits) {
			l.RequestTimeout = 150 * time.Millisecond
		}})
		start := time.Now()
		expectError(t, g.post(t, chatBody(false, "")), 504, "upstream_error", "upstream_timeout")
		if got := time.Since(start); got > 5*time.Second {
			t.Fatalf("took %v", got)
		}
		if got := g.mock.Stats()["b"].Requests; got != 0 {
			t.Fatalf("an attempt was started after the deadline: %d", got)
		}
	})
}

func TestCapabilityFiltering(t *testing.T) {
	t.Run("a request with temperature skips a route that can't honor it", func(t *testing.T) {
		g := newRoutedGateway(t, []testRoute{
			{model: "claude-route", anthropic: true, tier: 0},
			{model: "other-route", tier: 1},
		}, routedOptions{})
		got := completionText(t, g.post(t, chatBody(false, `,"temperature":0.5`)))
		if !strings.HasPrefix(got, "[other-route]") {
			t.Fatalf("text = %q", got)
		}
		if requests := g.mock.Stats()["claude-route"].Requests; requests != 0 {
			t.Fatalf("the incapable route saw %d requests", requests)
		}
	})
	t.Run("without one, the client is told why", func(t *testing.T) {
		g := newRoutedGateway(t, []testRoute{{model: "claude-route", anthropic: true}}, routedOptions{})
		got := expectError(t, g.post(t, chatBody(false, `,"temperature":0.5`)), 400, "invalid_request_error", "no_capable_route")
		if !strings.Contains(got.Message, "temperature") {
			t.Fatalf("message = %q", got.Message)
		}
		if requests := g.mock.Stats()["claude-route"].Requests; requests != 0 {
			t.Fatalf("the incapable route saw %d requests", requests)
		}
	})
	t.Run("a plain request can use any route", func(t *testing.T) {
		g := newRoutedGateway(t, []testRoute{{model: "claude-route", anthropic: true}}, routedOptions{})
		if got := completionText(t, g.post(t, chatBody(false, ""))); !strings.HasPrefix(got, "[claude-route]") {
			t.Fatalf("text = %q", got)
		}
	})
}

func TestWeightedRoutingThroughTheGateway(t *testing.T) {
	g := newRoutedGateway(t, []testRoute{{model: "heavy;tokens=1", weight: 4}, {model: "light;tokens=1", weight: 1}}, routedOptions{})
	const requests = 200
	for range requests {
		resp := g.post(t, chatBody(false, ""))
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		g.recorder.wait(t)
	}
	heavy := g.mock.Stats()["heavy"].Requests
	if heavy < 140 || heavy > 180 {
		t.Fatalf("heavy served %d of %d, want about 160", heavy, requests)
	}
	if got := heavy + g.mock.Stats()["light"].Requests; got != requests {
		t.Fatalf("total = %d", got)
	}
}

func TestRetryHints(t *testing.T) {
	t.Run("a route with a hint is retried once the hint passes", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("only;tokens=1;fail=status;status=429;retry_after=1;fail_first=1"), routedOptions{maxAttempts: 2})
		start := time.Now()
		resp := g.post(t, chatBody(false, ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := time.Since(start); got < time.Second {
			t.Fatalf("retried after %v, before the hint", got)
		}
		if got := resp.Header.Get("X-Llmgate-Attempts"); got != "2" {
			t.Fatalf("attempts header = %q", got)
		}
		g.recorder.wait(t)
		if a := g.recorder.wait(t); a.Reason != accounting.ReasonRetryAfter {
			t.Fatalf("reason = %q", a.Reason)
		}
	})
	t.Run("a hint past the deadline isn't waited for", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("only;fail=status;status=429;retry_after=30"), routedOptions{maxAttempts: 2, limits: func(l *Limits) {
			l.RequestTimeout = 2 * time.Second
		}})
		start := time.Now()
		resp := g.post(t, chatBody(false, ""))
		expectError(t, resp, 429, "rate_limit_error", "upstream_rate_limited")
		if got := time.Since(start); got > time.Second {
			t.Fatalf("waited %v for a hint that couldn't help", got)
		}
		if got := resp.Header.Get("Retry-After"); got != "30" {
			t.Fatalf("Retry-After = %q", got)
		}
		if got := g.mock.Stats()["only"].Requests; got != 1 {
			t.Fatalf("attempts = %d", got)
		}
	})
	t.Run("the attempt limit still applies", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("only;fail=status;status=429;retry_after=1"), routedOptions{maxAttempts: 1})
		expectError(t, g.post(t, chatBody(false, "")), 429, "rate_limit_error", "upstream_rate_limited")
		if got := g.mock.Stats()["only"].Requests; got != 1 {
			t.Fatalf("attempts = %d", got)
		}
	})
	t.Run("a rate limited route is held for later requests", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("limited;fail=status;status=429;retry_after=30", "backup"), routedOptions{})
		for range 3 {
			resp := g.post(t, chatBody(false, ""))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
		}
		if got := g.mock.Stats()["limited"].Requests; got != 1 {
			t.Fatalf("the held route saw %d requests", got)
		}
	})
	t.Run("everything held answers quickly and says when to retry", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("limited;fail=status;status=429;retry_after=30"), routedOptions{limits: func(l *Limits) {
			l.RequestTimeout = 2 * time.Second
		}})
		expectError(t, g.post(t, chatBody(false, "")), 429, "rate_limit_error", "upstream_rate_limited")
		start := time.Now()
		resp := g.post(t, chatBody(false, ""))
		expectError(t, resp, 503, "upstream_error", "no_route_available")
		if got := time.Since(start); got > time.Second {
			t.Fatalf("took %v", got)
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Fatal("no Retry-After")
		}
		if got := g.mock.Stats()["limited"].Requests; got != 1 {
			t.Fatalf("attempts = %d", got)
		}
	})
}

func TestCircuitBreakerThroughTheGateway(t *testing.T) {
	const cooldown = 300 * time.Millisecond
	breaker := routing.BreakerConfig{Window: time.Minute, MinRequests: 4, ErrorRate: 0.5, Cooldown: cooldown, HalfOpenProbes: 2}

	t.Run("it opens, stops sending traffic, probes, and recovers", func(t *testing.T) {
		// Fails exactly the requests that open the breaker, then works.
		g := newRoutedGateway(t, tiers("flaky;tokens=1;fail=status;status=500;fail_first=4", "backup;tokens=1"), routedOptions{breaker: breaker})

		for range 4 {
			if resp := g.post(t, chatBody(false, "")); resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
		}
		if got := g.mock.Stats()["flaky"].Requests; got != 4 {
			t.Fatalf("flaky saw %d requests", got)
		}

		// Open: requests go straight to the backup, without touching the failing route.
		for range 5 {
			resp := g.post(t, chatBody(false, ""))
			if got := resp.Header.Get("X-Llmgate-Attempts"); got != "1" {
				t.Fatalf("attempts header = %q", got)
			}
		}
		if got := g.mock.Stats()["flaky"].Requests; got != 4 {
			t.Fatalf("an open breaker let traffic through: %d", got)
		}

		// After the cooldown it probes, and enough successes close it.
		time.Sleep(cooldown + 50*time.Millisecond)
		for range 2 {
			if got := completionText(t, g.post(t, chatBody(false, ""))); !strings.HasPrefix(got, "[flaky]") {
				t.Fatalf("probe was served by %q", got)
			}
		}
		for range 10 {
			if got := completionText(t, g.post(t, chatBody(false, ""))); !strings.HasPrefix(got, "[flaky]") {
				t.Fatalf("served by %q after recovery", got)
			}
		}
	})
	t.Run("midstream failures count", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("flaky;fail=disconnect;after=1;tokens=3", "backup;tokens=1"), routedOptions{breaker: breaker})
		for range 4 {
			_ = readStream(t, g.post(t, chatBody(true, "")).Body)
		}
		if got := completionText(t, g.post(t, chatBody(false, ""))); !strings.HasPrefix(got, "[backup]") {
			t.Fatalf("served by %q", got)
		}
		if got := g.mock.Stats()["flaky"].Requests; got != 4 {
			t.Fatalf("flaky saw %d requests", got)
		}
	})
	t.Run("invalid requests don't open it", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("picky;fail=status;status=400"), routedOptions{breaker: breaker})
		for range 20 {
			expectError(t, g.post(t, chatBody(false, "")), 400, "invalid_request_error", "upstream_rejected")
		}
		if got := g.mock.Stats()["picky"].Requests; got != 20 {
			t.Fatalf("requests = %d", got)
		}
	})
	t.Run("client cancellations don't open it", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("slow;latency=30s"), routedOptions{breaker: breaker})
		for i := range 8 {
			ctx, cancel := context.WithCancel(context.Background())
			errs := make(chan error, 1)
			go func() {
				resp, err := g.send(ctx, chatBody(false, ""))
				if err == nil {
					_ = resp.Body.Close()
				}
				errs <- err
			}()
			want := int64(i + 1)
			waitForMock(t, g, "slow", func(s mock.LabelStats) bool { return s.Requests == want })
			cancel()
			if err := <-errs; err == nil {
				t.Error("the canceled request reported success")
			}
			waitForMock(t, g, "slow", func(s mock.LabelStats) bool { return s.Canceled == want })
			g.recorder.wait(t)
		}
		// Still closed, so this reaches the route.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if resp, err := g.send(ctx, chatBody(false, "")); err == nil {
			_ = resp.Body.Close()
		}
		waitForMock(t, g, "slow", func(s mock.LabelStats) bool { return s.Requests == 9 })
	})
	t.Run("rate limits don't open it", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("busy;fail=status;status=429"), routedOptions{breaker: breaker})
		for range 20 {
			expectError(t, g.post(t, chatBody(false, "")), 429, "rate_limit_error", "upstream_rate_limited")
		}
		if got := g.mock.Stats()["busy"].Requests; got != 20 {
			t.Fatalf("requests = %d", got)
		}
	})
	t.Run("all routes open fails fast with a retry hint", func(t *testing.T) {
		g := newRoutedGateway(t, tiers("down;fail=status;status=500"), routedOptions{breaker: breaker})
		for range 4 {
			expectError(t, g.post(t, chatBody(false, "")), 502, "upstream_error", "upstream_failed")
		}
		start := time.Now()
		resp := g.post(t, chatBody(false, ""))
		expectError(t, resp, 503, "upstream_error", "no_route_available")
		if got := time.Since(start); got > 100*time.Millisecond {
			t.Fatalf("took %v", got)
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Fatal("no Retry-After")
		}
		if got := g.mock.Stats()["down"].Requests; got != 4 {
			t.Fatalf("requests = %d", got)
		}
	})
}

// A fallback attempt must get its own full window, so the first attempt's slowness
// can't eat into it, and an attempt's cancellation must not leak into the next.
func TestAttemptsDoNotShareTimers(t *testing.T) {
	g := newRoutedGateway(t, tiers("first;ttfb=30s", "second;tokens=2;ttfb=100ms"), routedOptions{limits: func(l *Limits) {
		l.FirstEventTimeout = 150 * time.Millisecond
	}})
	s := readStream(t, g.post(t, chatBody(true, "")).Body)
	if !s.done || !strings.HasPrefix(s.text, "[second]") {
		t.Fatalf("stream = %+v", s)
	}
	first, second := g.recorder.wait(t), g.recorder.wait(t)
	if first.ErrorKind != provider.KindTimeout || second.Outcome != accounting.OutcomeCompleted {
		t.Fatalf("attempts = %+v, %+v", first.Attempt, second.Attempt)
	}
	if second.Usage == nil || second.Usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", second.Usage)
	}
	if first.Usage != nil {
		t.Fatalf("the failed attempt's usage = %+v", first.Usage)
	}
}

func TestUsageFromAFailedAttemptIsKeptOnThatAttempt(t *testing.T) {
	g := newRoutedGateway(t, []testRoute{
		{model: "primary;thinking=1;fail=disconnect;after=0", anthropic: true, tier: 0},
		{model: "backup;tokens=2", tier: 1},
	}, routedOptions{})
	s := readStream(t, g.post(t, chatBody(true, `,"stream_options":{"include_usage":true}`)).Body)
	if !s.done {
		t.Fatalf("stream = %+v", s)
	}
	first, second := g.recorder.wait(t), g.recorder.wait(t)
	if first.Usage == nil || first.Usage.State != chat.UsagePartial {
		t.Fatalf("first usage = %+v", first.Usage)
	}
	if second.Usage == nil || second.Usage.State != chat.UsageReported {
		t.Fatalf("second usage = %+v", second.Usage)
	}
	last := s.chunks[len(s.chunks)-1]
	if !strings.Contains(string(last.Usage), `"completion_tokens":2`) {
		t.Fatalf("the client's usage should be the serving attempt's: %s", last.Usage)
	}
}
