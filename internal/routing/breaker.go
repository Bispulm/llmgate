package routing

import (
	"sync"
	"time"
)

// BreakerState is a circuit breaker's state.
type BreakerState string

// Breaker states.
const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

// Health is what an attempt says about its route.
type Health int

const (
	// HealthSuccess means the route worked.
	HealthSuccess Health = iota
	// HealthFailure means the route failed.
	HealthFailure
	// HealthNeutral says nothing about the route, such as a client canceling or
	// sending an invalid request.
	HealthNeutral
)

const breakerBuckets = 10

// BreakerConfig configures a circuit breaker. Zero values take defaults.
type BreakerConfig struct {
	// Window is how far back outcomes count.
	Window time.Duration
	// MinRequests is the least evidence that can open the breaker.
	MinRequests int
	// ErrorRate opens the breaker when failures reach this share of outcomes.
	ErrorRate float64
	// Cooldown is how long the breaker stays open before probing.
	Cooldown time.Duration
	// HalfOpenProbes is how many attempts may probe at once. All must succeed for
	// the breaker to close.
	HalfOpenProbes int
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.Window <= 0 {
		c.Window = 60 * time.Second
	}
	if c.MinRequests <= 0 {
		c.MinRequests = 10
	}
	if c.ErrorRate <= 0 {
		c.ErrorRate = 0.5
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 30 * time.Second
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = 2
	}
	return c
}

// Permit lets one attempt proceed. It must be reported exactly once.
type Permit struct {
	generation uint64
}

// BreakerSnapshot is a breaker's state for operators.
type BreakerSnapshot struct {
	State     BreakerState  `json:"state"`
	Successes int           `json:"successes"`
	Failures  int           `json:"failures"`
	RetryIn   time.Duration `json:"retry_in_ns"`
}

type bucket struct {
	index               int64
	successes, failures int
}

// Breaker is a circuit breaker for one route, in one process. Each replica forms
// its own view of a route's health. That can send a few more attempts to a failing
// route than a shared breaker would, but needs no coordination in the request path.
type Breaker struct {
	cfg         BreakerConfig
	now         func() time.Time
	bucketWidth time.Duration

	mu sync.Mutex
	// generation changes with every state change, so that an attempt begun before
	// one can't affect what came after.
	generation     uint64
	state          BreakerState
	openedAt       time.Time
	buckets        [breakerBuckets]bucket
	probesInFlight int
	probeSuccesses int
}

// NewBreaker returns a closed breaker. A nil now uses time.Now.
func NewBreaker(cfg BreakerConfig, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	cfg = cfg.withDefaults()
	width := cfg.Window / breakerBuckets
	if width <= 0 {
		width = 1
	}
	return &Breaker{cfg: cfg, now: now, bucketWidth: width, state: BreakerClosed}
}

// Allow asks to make an attempt. Every permit granted must be reported.
func (b *Breaker) Allow() (Permit, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerOpen:
		if b.now().Sub(b.openedAt) < b.cfg.Cooldown {
			return Permit{}, false
		}
		b.transition(BreakerHalfOpen)
		fallthrough
	case BreakerHalfOpen:
		if b.probesInFlight+b.probeSuccesses >= b.cfg.HalfOpenProbes {
			return Permit{}, false
		}
		b.probesInFlight++
	}
	return Permit{generation: b.generation}, true
}

// Report records how a permitted attempt went. A result from before the latest
// state change is ignored.
func (b *Breaker) Report(p Permit, h Health) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if p.generation != b.generation {
		return
	}
	switch b.state {
	case BreakerClosed:
		b.recordClosed(h)
	case BreakerHalfOpen:
		b.probesInFlight--
		switch h {
		case HealthFailure:
			b.open()
		case HealthSuccess:
			b.probeSuccesses++
			if b.probeSuccesses >= b.cfg.HalfOpenProbes {
				b.transition(BreakerClosed)
			}
		}
	}
}

func (b *Breaker) recordClosed(h Health) {
	if h == HealthNeutral {
		return
	}
	slot := b.currentBucket()
	if h == HealthSuccess {
		slot.successes++
		return
	}
	slot.failures++
	successes, failures := b.windowCounts()
	total := successes + failures
	if total >= b.cfg.MinRequests && float64(failures) >= b.cfg.ErrorRate*float64(total) {
		b.open()
	}
}

func (b *Breaker) open() {
	b.transition(BreakerOpen)
	b.openedAt = b.now()
}

func (b *Breaker) transition(to BreakerState) {
	b.state = to
	b.generation++
	b.probesInFlight, b.probeSuccesses = 0, 0
	b.buckets = [breakerBuckets]bucket{}
}

func (b *Breaker) currentBucket() *bucket {
	index := b.now().UnixNano() / int64(b.bucketWidth)
	slot := &b.buckets[index%breakerBuckets]
	if slot.index != index {
		*slot = bucket{index: index}
	}
	return slot
}

func (b *Breaker) windowCounts() (successes, failures int) {
	current := b.now().UnixNano() / int64(b.bucketWidth)
	for i := range b.buckets {
		if slot := &b.buckets[i]; current-slot.index < breakerBuckets {
			successes += slot.successes
			failures += slot.failures
		}
	}
	return successes, failures
}

// State returns the current state.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// RetryIn returns how long until an open breaker will probe, or 0 if it isn't open.
func (b *Breaker) RetryIn() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.retryIn()
}

func (b *Breaker) retryIn() time.Duration {
	if b.state != BreakerOpen {
		return 0
	}
	return max(b.cfg.Cooldown-b.now().Sub(b.openedAt), 0)
}

// Snapshot returns the breaker's state for operators.
func (b *Breaker) Snapshot() BreakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	successes, failures := b.windowCounts()
	return BreakerSnapshot{State: b.state, Successes: successes, Failures: failures, RetryIn: b.retryIn()}
}
