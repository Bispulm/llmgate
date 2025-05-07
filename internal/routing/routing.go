// Package routing decides which upstream routes may serve a request, in what order,
// and how an attempt's outcome should affect that. See
// docs/adr/0004-routing-and-failure-handling.md.
package routing

import (
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/provider"
)

// MaxHold caps how long a provider's Retry-After hint can keep a route out of use,
// so that a bad hint can't take a route out of service for long.
const MaxHold = 60 * time.Second

// Route is one way to serve an alias.
type Route struct {
	ProviderName string
	Provider     provider.Provider
	Model        string
	// Tier orders fallback. Lower tiers are tried first.
	Tier int
	// Weight shares traffic within a tier. Zero means 1.
	Weight int
	// Limiter bounds concurrent attempts, usually shared by a provider's routes. Nil
	// means no limit.
	Limiter *limits.FairLimiter
}

// Alias is a client-visible model name.
type Alias struct {
	Name            string
	MaxOutputTokens int64
	Routes          []Route
	// MaxAttempts bounds attempts for one request. Zero means one per route.
	MaxAttempts int

	table       *Table
	routeStates []*routeState
}

// Attempts returns how many upstream attempts one request may make.
func (a *Alias) Attempts() int {
	if a.MaxAttempts > 0 {
		return a.MaxAttempts
	}
	return len(a.Routes)
}

// routeState is what's known about a provider and model, shared by every alias that
// uses them.
type routeState struct {
	route     Route
	breaker   *Breaker
	holdUntil atomic.Int64 // Unix nanoseconds; 0 means no hold
}

// Options configures a Table. Zero values take defaults.
type Options struct {
	Breaker BreakerConfig
	// Now replaces the clock, for tests.
	Now func() time.Time
	// Rand replaces the random source, for tests.
	Rand *rand.Rand
}

// Table holds the configured aliases. Its configuration doesn't change after
// creation, and it is safe for concurrent use.
type Table struct {
	aliases map[string]*Alias
	now     func() time.Time

	randMu sync.Mutex
	rand   *rand.Rand
}

// NewTable returns a table of the given aliases.
func NewTable(aliases []Alias, opts Options) *Table {
	t := &Table{aliases: make(map[string]*Alias, len(aliases)), now: opts.Now, rand: opts.Rand}
	if t.now == nil {
		t.now = time.Now
	}
	states := make(map[[2]string]*routeState)
	for i := range aliases {
		alias := &aliases[i]
		alias.table = t
		alias.routeStates = make([]*routeState, len(alias.Routes))
		for j, route := range alias.Routes {
			if route.Weight <= 0 {
				route.Weight = 1
				alias.Routes[j].Weight = 1
			}
			key := [2]string{route.ProviderName, route.Model}
			state, ok := states[key]
			if !ok {
				state = &routeState{route: route, breaker: NewBreaker(opts.Breaker, t.now)}
				states[key] = state
			}
			// The state is shared, but tier and weight belong to this alias.
			alias.routeStates[j] = state
		}
		t.aliases[alias.Name] = alias
	}
	return t
}

// Resolve looks up an alias by its exact name.
func (t *Table) Resolve(name string) (*Alias, bool) {
	a, ok := t.aliases[name]
	return a, ok
}

func (t *Table) randomFloat() float64 {
	if t.rand == nil {
		return rand.Float64() //nolint:gosec // Traffic sharing has no security need for strong randomness.
	}
	t.randMu.Lock()
	defer t.randMu.Unlock()
	return t.rand.Float64()
}

// SkipReason says why a route wasn't offered. The values are a small fixed set, so
// they are safe as metric labels.
type SkipReason string

// Skip reasons.
const (
	SkipCapability  SkipReason = "capability"
	SkipBreakerOpen SkipReason = "breaker_open"
	SkipHeld        SkipReason = "rate_limit_hold"
	SkipSaturated   SkipReason = "saturated"
)

// Plan is the routes a request may use, in order. It isn't safe for concurrent use,
// but belongs to one request.
type Plan struct {
	alias   *Alias
	ordered []planEntry
	cursor  int
	skipped map[SkipReason]int
}

type planEntry struct {
	state *routeState
	route Route
}

// Plan orders the routes that can honor req. Whether each is usable is only decided
// when it's offered, because time may pass between attempts.
func (a *Alias) Plan(req *chat.Request) *Plan {
	plan := &Plan{alias: a, ordered: make([]planEntry, 0, len(a.Routes))}
	needsSampling := req.Temperature != nil || req.TopP != nil

	for i, route := range a.Routes {
		if needsSampling && !route.Provider.Capabilities().Sampling {
			plan.skip(SkipCapability)
			continue
		}
		plan.ordered = append(plan.ordered, planEntry{state: a.routeStates[i], route: route})
	}
	a.shuffle(plan.ordered)
	return plan
}

// shuffle orders by tier, then by weighted random choice without replacement.
func (a *Alias) shuffle(entries []planEntry) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].route.Tier < entries[j].route.Tier })
	for start := 0; start < len(entries); {
		end := start + 1
		for end < len(entries) && entries[end].route.Tier == entries[start].route.Tier {
			end++
		}
		for i := start; i < end-1; i++ {
			total := 0
			for _, e := range entries[i:end] {
				total += e.route.Weight
			}
			pick := a.table.randomFloat() * float64(total)
			for j := i; j < end; j++ {
				pick -= float64(entries[j].route.Weight)
				if pick < 0 {
					entries[i], entries[j] = entries[j], entries[i]
					break
				}
			}
		}
		start = end
	}
}

func (p *Plan) skip(reason SkipReason) {
	if p.skipped == nil {
		p.skipped = make(map[SkipReason]int, 3)
	}
	p.skipped[reason]++
}

// NoteSaturated records that an offered route had no capacity.
func (p *Plan) NoteSaturated() { p.skip(SkipSaturated) }

// Skipped counts why routes weren't offered.
func (p *Plan) Skipped() map[SkipReason]int { return p.skipped }

// NoCapableRoute reports that no configured route can honor the request at all, as
// opposed to being unhealthy just now.
func (p *Plan) NoCapableRoute() bool { return len(p.ordered) == 0 }

// Next offers the next usable route, or false if none remain. A route that's offered
// must be finished with Finish or Abandon.
func (p *Plan) Next() (*Selected, bool) {
	now := p.alias.table.now()
	for p.cursor < len(p.ordered) {
		entry := p.ordered[p.cursor]
		p.cursor++
		if until := entry.state.holdUntil.Load(); until != 0 && now.UnixNano() < until {
			p.skip(SkipHeld)
			continue
		}
		permit, ok := entry.state.breaker.Allow()
		if !ok {
			p.skip(SkipBreakerOpen)
			continue
		}
		return &Selected{Route: entry.route, state: entry.state, permit: permit, table: p.alias.table}, true
	}
	return nil, false
}

// Rewind lets routes be offered again, for a retry after a hint has passed.
func (p *Plan) Rewind() { p.cursor = 0 }

// EarliestRetry returns the shortest time until a held route in this plan becomes
// usable, or 0 if none is held.
func (p *Plan) EarliestRetry() time.Duration {
	now := p.alias.table.now().UnixNano()
	var earliest time.Duration
	for _, entry := range p.ordered {
		until := entry.state.holdUntil.Load()
		if until <= now {
			continue
		}
		if wait := time.Duration(until - now); earliest == 0 || wait < earliest {
			earliest = wait
		}
	}
	return earliest
}

// RetryAfter returns the shortest time until any route in this plan may be usable,
// for telling a client when to try again. It returns 0 if that's unknown.
func (p *Plan) RetryAfter() time.Duration {
	earliest := p.EarliestRetry()
	for _, entry := range p.ordered {
		if wait := entry.state.breaker.RetryIn(); wait > 0 && (earliest == 0 || wait < earliest) {
			earliest = wait
		}
	}
	return earliest
}

// Selected is a route offered for one attempt.
type Selected struct {
	Route  Route
	state  *routeState
	permit Permit
	table  *Table
	done   bool
}

// Finish reports the attempt's outcome. A nil failure means success.
func (s *Selected) Finish(failure *provider.Error) {
	if s.done {
		return
	}
	s.done = true
	if failure == nil {
		s.state.breaker.Report(s.permit, HealthSuccess)
		return
	}
	effect := Classify(failure)
	s.state.breaker.Report(s.permit, effect.Health)
	if failure.Kind == provider.KindRateLimited && failure.RetryAfter > 0 {
		hold := min(failure.RetryAfter, MaxHold)
		s.state.holdUntil.Store(s.table.now().Add(hold).UnixNano())
	}
}

// Abandon ends an attempt without saying anything about the route, such as when the
// client went away.
func (s *Selected) Abandon() {
	if s.done {
		return
	}
	s.done = true
	s.state.breaker.Report(s.permit, HealthNeutral)
}

// Effect is what a failed attempt means for routing.
type Effect struct {
	// Failover means another route may be tried, if nothing has reached the client.
	Failover bool
	Health   Health
}

// Classify decides how a failure affects routing. See ADR 0004.
func Classify(failure *provider.Error) Effect {
	switch failure.Kind {
	case provider.KindInvalidRequest, provider.KindCanceled:
		// These are about the request or the client, and another route won't change
		// them. Counting them would let a client damage a provider's health.
		return Effect{Failover: false, Health: HealthNeutral}
	case provider.KindRateLimited:
		// The provider is working, and asked for fewer requests. A hold respects that
		// without treating it as broken.
		return Effect{Failover: true, Health: HealthNeutral}
	default:
		return Effect{Failover: true, Health: HealthFailure}
	}
}

// RouteStatus describes a route for operators.
type RouteStatus struct {
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Tier     int             `json:"tier"`
	Weight   int             `json:"weight"`
	Breaker  BreakerSnapshot `json:"breaker"`
	HeldFor  time.Duration   `json:"held_for_ns"`
}

// Status returns each route's current state.
func (a *Alias) Status() []RouteStatus {
	now := a.table.now().UnixNano()
	out := make([]RouteStatus, len(a.Routes))
	for i, route := range a.Routes {
		state := a.routeStates[i]
		status := RouteStatus{
			Provider: route.ProviderName,
			Model:    route.Model,
			Tier:     route.Tier,
			Weight:   route.Weight,
			Breaker:  state.breaker.Snapshot(),
		}
		if until := state.holdUntil.Load(); until > now {
			status.HeldFor = time.Duration(until - now)
		}
		out[i] = status
	}
	return out
}
