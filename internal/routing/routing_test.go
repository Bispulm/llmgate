package routing

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
)

// stubProvider only answers capability questions. Routing never calls the rest.
type stubProvider struct {
	provider.Provider
	sampling bool
}

func (s stubProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Sampling: s.sampling}
}

func route(name string, tier, weight int) Route {
	return Route{ProviderName: name, Provider: stubProvider{sampling: true}, Model: "m", Tier: tier, Weight: weight}
}

func testTable(t *testing.T, routes ...Route) (*Alias, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	table := NewTable([]Alias{{Name: "fast", MaxOutputTokens: 10, Routes: routes}}, Options{
		Breaker: testBreakerConfig(),
		Now:     clock.Now,
		Rand:    rand.New(rand.NewPCG(1, 2)),
	})
	alias, ok := table.Resolve("fast")
	if !ok {
		t.Fatal("alias not found")
	}
	return alias, clock
}

// order drains a plan, returning provider names in the order offered.
func order(plan *Plan) []string {
	var out []string
	for {
		next, ok := plan.Next()
		if !ok {
			return out
		}
		out = append(out, next.Route.ProviderName)
	}
}

func TestResolve(t *testing.T) {
	alias, _ := testTable(t, route("a", 0, 1))
	if alias.MaxOutputTokens != 10 || alias.Routes[0].Model != "m" {
		t.Fatalf("alias = %+v", alias)
	}
	table := NewTable([]Alias{{Name: "fast"}}, Options{})
	if _, ok := table.Resolve("missing"); ok {
		t.Fatal("found an alias that doesn't exist")
	}
	if _, ok := table.Resolve("FAST"); ok {
		t.Fatal("alias lookup must be exact")
	}
}

func TestPlanTriesLowerTiersFirst(t *testing.T) {
	alias, _ := testTable(t, route("fallback", 2, 1), route("primary", 0, 1), route("secondary", 1, 1))
	got := order(alias.Plan(&chat.Request{}))
	want := []string{"primary", "secondary", "fallback"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestPlanOffersEachRouteOnce(t *testing.T) {
	alias, _ := testTable(t, route("a", 0, 5), route("b", 0, 1), route("c", 1, 1))
	got := order(alias.Plan(&chat.Request{}))
	if len(got) != 3 {
		t.Fatalf("order = %v", got)
	}
	seen := map[string]bool{}
	for _, name := range got {
		if seen[name] {
			t.Fatalf("offered %s twice in %v", name, got)
		}
		seen[name] = true
	}
}

func TestPlanRespectsWeights(t *testing.T) {
	alias, _ := testTable(t, route("heavy", 0, 3), route("light", 0, 1))
	const trials = 10000
	first := map[string]int{}
	for range trials {
		next, ok := alias.Plan(&chat.Request{}).Next()
		if !ok {
			t.Fatal("no route")
		}
		first[next.Route.ProviderName]++
	}
	share := float64(first["heavy"]) / trials
	if math.Abs(share-0.75) > 0.02 {
		t.Fatalf("heavy was first in %.3f of plans, want about 0.75", share)
	}
}

func TestPlanFiltersByCapability(t *testing.T) {
	sampling := route("sampling", 1, 1)
	noSampling := Route{ProviderName: "no-sampling", Provider: stubProvider{sampling: false}, Model: "m", Weight: 1}
	alias, _ := testTable(t, noSampling, sampling)
	v := 0.5

	t.Run("a sampling request skips the incapable route", func(t *testing.T) {
		plan := alias.Plan(&chat.Request{Temperature: &v})
		if got := order(plan); len(got) != 1 || got[0] != "sampling" {
			t.Fatalf("order = %v", got)
		}
		if got := plan.Skipped()[SkipCapability]; got != 1 {
			t.Fatalf("capability skips = %d", got)
		}
	})
	t.Run("top_p counts too", func(t *testing.T) {
		if got := order(alias.Plan(&chat.Request{TopP: &v})); len(got) != 1 {
			t.Fatalf("order = %v", got)
		}
	})
	t.Run("a plain request can use either", func(t *testing.T) {
		if got := order(alias.Plan(&chat.Request{})); len(got) != 2 {
			t.Fatalf("order = %v", got)
		}
	})
	t.Run("no capable route at all is distinguishable", func(t *testing.T) {
		only, _ := testTable(t, noSampling)
		plan := only.Plan(&chat.Request{Temperature: &v})
		if !plan.NoCapableRoute() {
			t.Fatal("NoCapableRoute = false")
		}
		if _, ok := plan.Next(); ok {
			t.Fatal("offered an incapable route")
		}
	})
}

func TestPlanSkipsOpenBreakers(t *testing.T) {
	alias, clock := testTable(t, route("broken", 0, 1), route("healthy", 1, 1))
	broken := alias.routeStates[0]
	if broken.route.ProviderName != "broken" {
		t.Fatalf("route order = %+v", alias.routeStates)
	}
	for range 4 {
		permit, _ := broken.breaker.Allow()
		broken.breaker.Report(permit, HealthFailure)
	}

	plan := alias.Plan(&chat.Request{})
	if got := order(plan); len(got) != 1 || got[0] != "healthy" {
		t.Fatalf("order = %v", got)
	}
	if plan.Skipped()[SkipBreakerOpen] != 1 {
		t.Fatalf("skips = %v", plan.Skipped())
	}

	clock.Advance(30 * time.Second)
	if got := order(alias.Plan(&chat.Request{})); got[0] != "broken" {
		t.Fatalf("no probe after the cooldown: %v", got)
	}
}

func TestPlanSkipsHeldRoutesUntilTheHintPasses(t *testing.T) {
	alias, clock := testTable(t, route("limited", 0, 1), route("other", 1, 1))
	next, _ := alias.Plan(&chat.Request{}).Next()
	next.Finish(&provider.Error{Kind: provider.KindRateLimited, RetryAfter: 5 * time.Second})

	plan := alias.Plan(&chat.Request{})
	if got := order(plan); len(got) != 1 || got[0] != "other" {
		t.Fatalf("order = %v", got)
	}
	if plan.Skipped()[SkipHeld] != 1 {
		t.Fatalf("skips = %v", plan.Skipped())
	}
	if got := plan.RetryAfter(); got != 5*time.Second {
		t.Fatalf("RetryAfter = %v", got)
	}
	// Another request's hint is a reason to skip the route, but not to wait for it.
	if got := plan.EarliestRetry(); got != 0 {
		t.Fatalf("EarliestRetry = %v", got)
	}

	clock.Advance(5 * time.Second)
	if got := order(alias.Plan(&chat.Request{})); got[0] != "limited" {
		t.Fatalf("order = %v", got)
	}
}

func TestHoldIsCapped(t *testing.T) {
	alias, clock := testTable(t, route("limited", 0, 1))
	next, _ := alias.Plan(&chat.Request{}).Next()
	next.Finish(&provider.Error{Kind: provider.KindRateLimited, RetryAfter: 24 * time.Hour})

	clock.Advance(MaxHold)
	if _, ok := alias.Plan(&chat.Request{}).Next(); !ok {
		t.Fatal("a provider's hint held a route past the cap")
	}
}

// There is no same-route retry except for a hint this request was given.
func TestRewindOnlyOffersRoutesThatGaveThisPlanAHint(t *testing.T) {
	alias, clock := testTable(t, route("failed", 0, 1), route("hinted", 1, 1), route("worked", 2, 1))
	plan := alias.Plan(&chat.Request{})

	failed, _ := plan.Next()
	failed.Finish(&provider.Error{Kind: provider.KindServer})
	hinted, _ := plan.Next()
	hinted.Finish(&provider.Error{Kind: provider.KindRateLimited, RetryAfter: 2 * time.Second})
	worked, _ := plan.Next()
	worked.Abandon()

	if _, ok := plan.Next(); ok {
		t.Fatal("offered a route twice without a rewind")
	}
	if got := plan.EarliestRetry(); got != 2*time.Second {
		t.Fatalf("EarliestRetry = %v", got)
	}

	clock.Advance(2 * time.Second)
	plan.Rewind()
	if got := order(plan); len(got) != 1 || got[0] != "hinted" {
		t.Fatalf("a rewind offered %v", got)
	}
	if got := plan.EarliestRetry(); got != 0 {
		t.Fatalf("EarliestRetry after the retry = %v", got)
	}
}

func TestARateLimitWithoutAHintGivesNothingToWaitFor(t *testing.T) {
	alias, _ := testTable(t, route("a", 0, 1))
	plan := alias.Plan(&chat.Request{})
	next, _ := plan.Next()
	next.Finish(&provider.Error{Kind: provider.KindRateLimited})
	if got := plan.EarliestRetry(); got != 0 {
		t.Fatalf("EarliestRetry = %v", got)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		kind     provider.Kind
		failover bool
		health   Health
	}{
		{provider.KindConnection, true, HealthFailure},
		{provider.KindTimeout, true, HealthFailure},
		{provider.KindServer, true, HealthFailure},
		{provider.KindOverloaded, true, HealthFailure},
		{provider.KindInterrupted, true, HealthFailure},
		{provider.KindProtocol, true, HealthFailure},
		{provider.KindAuth, true, HealthFailure},
		{provider.KindNotFound, true, HealthFailure},
		{provider.KindBilling, true, HealthFailure},
		// A rate limit says the provider is working and busy. It sets a hold instead.
		{provider.KindRateLimited, true, HealthNeutral},
		// These are about the request, and say nothing about the provider.
		{provider.KindInvalidRequest, false, HealthNeutral},
		{provider.KindCanceled, false, HealthNeutral},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			got := Classify(&provider.Error{Kind: tt.kind})
			if got.Failover != tt.failover || got.Health != tt.health {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestFinishReportsToTheBreaker(t *testing.T) {
	alias, _ := testTable(t, route("a", 0, 1))
	state := alias.routeStates[0]

	for range 4 {
		next, ok := alias.Plan(&chat.Request{}).Next()
		if !ok {
			t.Fatal("no route")
		}
		next.Finish(&provider.Error{Kind: provider.KindServer})
	}
	if state.breaker.State() != BreakerOpen {
		t.Fatalf("state = %s", state.breaker.State())
	}
}

func TestInvalidRequestsAndCancellationsDoNotAffectHealth(t *testing.T) {
	alias, _ := testTable(t, route("a", 0, 1))
	state := alias.routeStates[0]
	for range 50 {
		next, _ := alias.Plan(&chat.Request{}).Next()
		next.Finish(&provider.Error{Kind: provider.KindInvalidRequest})
		next, _ = alias.Plan(&chat.Request{}).Next()
		next.Finish(&provider.Error{Kind: provider.KindCanceled})
	}
	if state.breaker.State() != BreakerClosed {
		t.Fatalf("state = %s", state.breaker.State())
	}
	if got := state.breaker.Snapshot(); got.Failures != 0 || got.Successes != 0 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestOperationalStatus(t *testing.T) {
	alias, _ := testTable(t, route("a", 0, 1))
	next, _ := alias.Plan(&chat.Request{}).Next()
	next.Finish(nil)

	statuses := alias.Status()
	if len(statuses) != 1 || statuses[0].Provider != "a" || statuses[0].Breaker.Successes != 1 {
		t.Fatalf("status = %+v", statuses)
	}
}
