package routing

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testBreakerConfig() BreakerConfig {
	return BreakerConfig{
		Window:         10 * time.Second,
		MinRequests:    4,
		ErrorRate:      0.5,
		Cooldown:       30 * time.Second,
		HalfOpenProbes: 2,
	}
}

func newTestBreaker(t *testing.T) (*Breaker, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	return NewBreaker(testBreakerConfig(), clock.Now), clock
}

// record reports an outcome through a fresh permit, requiring that one is granted.
func record(t *testing.T, b *Breaker, outcome Health) {
	t.Helper()
	permit, ok := b.Allow()
	if !ok {
		t.Fatalf("breaker refused an attempt in state %s", b.State())
	}
	b.Report(permit, outcome)
}

func openBreaker(t *testing.T, b *Breaker) {
	t.Helper()
	for range 4 {
		record(t, b, HealthFailure)
	}
	if b.State() != BreakerOpen {
		t.Fatalf("state = %s, want open", b.State())
	}
}

func TestBreakerNeedsMinimumEvidence(t *testing.T) {
	b, _ := newTestBreaker(t)
	for range 3 {
		record(t, b, HealthFailure)
	}
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s: three samples are below the minimum of four", b.State())
	}
	record(t, b, HealthFailure)
	if b.State() != BreakerOpen {
		t.Fatalf("state = %s", b.State())
	}
}

func TestBreakerStaysClosedBelowTheErrorRate(t *testing.T) {
	b, _ := newTestBreaker(t)
	for range 3 {
		record(t, b, HealthSuccess)
	}
	record(t, b, HealthFailure)
	record(t, b, HealthFailure)
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s at 2 of 5", b.State())
	}
	record(t, b, HealthFailure)
	if b.State() != BreakerOpen {
		t.Fatalf("state = %s at 3 of 6, which reaches the rate", b.State())
	}
}

func TestBreakerNeutralOutcomesDoNotCount(t *testing.T) {
	b, _ := newTestBreaker(t)
	for range 100 {
		record(t, b, HealthNeutral)
	}
	for range 3 {
		record(t, b, HealthFailure)
	}
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s: neutral outcomes must not count as evidence", b.State())
	}
}

func TestBreakerWindowForgetsOldSamples(t *testing.T) {
	b, clock := newTestBreaker(t)
	for range 3 {
		record(t, b, HealthFailure)
	}
	clock.Advance(11 * time.Second)
	record(t, b, HealthFailure)
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s: earlier failures left the window", b.State())
	}
}

func TestBreakerWindowSlidesRatherThanResets(t *testing.T) {
	b, clock := newTestBreaker(t)
	for range 2 {
		record(t, b, HealthFailure)
	}
	clock.Advance(5 * time.Second)
	for range 2 {
		record(t, b, HealthFailure)
	}
	if b.State() != BreakerOpen {
		t.Fatalf("state = %s: all four failures are within the window", b.State())
	}
}

func TestBreakerRefusesWhileOpenThenProbes(t *testing.T) {
	b, clock := newTestBreaker(t)
	openBreaker(t, b)

	if _, ok := b.Allow(); ok {
		t.Fatal("an open breaker allowed an attempt")
	}
	clock.Advance(29 * time.Second)
	if _, ok := b.Allow(); ok {
		t.Fatal("the breaker allowed an attempt before the cooldown ended")
	}
	if got := b.RetryIn(); got != time.Second {
		t.Fatalf("RetryIn = %v", got)
	}

	clock.Advance(time.Second)
	if _, ok := b.Allow(); !ok {
		t.Fatal("no probe was allowed after the cooldown")
	}
	if b.State() != BreakerHalfOpen {
		t.Fatalf("state = %s", b.State())
	}
}

func TestBreakerBoundsConcurrentProbes(t *testing.T) {
	b, clock := newTestBreaker(t)
	openBreaker(t, b)
	clock.Advance(30 * time.Second)

	for i := range 2 {
		if _, ok := b.Allow(); !ok {
			t.Fatalf("probe %d was refused", i)
		}
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("a third concurrent probe was allowed")
	}
}

func TestBreakerClosesAfterAllProbesSucceed(t *testing.T) {
	b, clock := newTestBreaker(t)
	openBreaker(t, b)
	clock.Advance(30 * time.Second)

	first, _ := b.Allow()
	second, _ := b.Allow()
	b.Report(first, HealthSuccess)
	if b.State() != BreakerHalfOpen {
		t.Fatalf("state = %s after one of two probes", b.State())
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("a succeeded probe's slot must not admit more attempts than the probe count")
	}
	b.Report(second, HealthSuccess)
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s after all probes", b.State())
	}

	// Closing starts with clean evidence.
	for range 3 {
		record(t, b, HealthFailure)
	}
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s: failures from before recovery must not count", b.State())
	}
}

// When the cooldown is shorter than the window, the failures that opened the breaker
// are still inside the window after it recovers. They must not count, or one more
// failure would open it again at once.
func TestRecoveryStartsWithCleanEvidence(t *testing.T) {
	clock := newFakeClock()
	cfg := testBreakerConfig()
	cfg.Window = time.Minute
	cfg.Cooldown = time.Second
	b := NewBreaker(cfg, clock.Now)
	openBreaker(t, b)

	clock.Advance(time.Second)
	first, _ := b.Allow()
	second, _ := b.Allow()
	b.Report(first, HealthSuccess)
	b.Report(second, HealthSuccess)
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s", b.State())
	}

	for range 3 {
		record(t, b, HealthFailure)
	}
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s: failures from before recovery counted", b.State())
	}
}

func TestBreakerReopensOnAFailedProbe(t *testing.T) {
	b, clock := newTestBreaker(t)
	openBreaker(t, b)
	clock.Advance(30 * time.Second)

	first, _ := b.Allow()
	second, _ := b.Allow()
	b.Report(first, HealthFailure)
	if b.State() != BreakerOpen {
		t.Fatalf("state = %s", b.State())
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("the cooldown did not restart")
	}

	// The other probe was already running. It must not change anything.
	b.Report(second, HealthSuccess)
	if b.State() != BreakerOpen {
		t.Fatalf("state = %s: a stale probe closed the breaker", b.State())
	}
}

func TestBreakerNeutralProbeReleasesItsSlot(t *testing.T) {
	b, clock := newTestBreaker(t)
	openBreaker(t, b)
	clock.Advance(30 * time.Second)

	first, _ := b.Allow()
	second, _ := b.Allow()
	if _, ok := b.Allow(); ok {
		t.Fatal("too many probes")
	}
	b.Report(first, HealthNeutral)
	third, ok := b.Allow()
	if !ok {
		t.Fatal("a canceled probe didn't release its slot, which could leave the breaker stuck half-open")
	}
	b.Report(second, HealthSuccess)
	b.Report(third, HealthSuccess)
	if b.State() != BreakerClosed {
		t.Fatalf("state = %s", b.State())
	}
}

func TestBreakerIgnoresResultsFromBeforeAStateChange(t *testing.T) {
	b, clock := newTestBreaker(t)
	slow, _ := b.Allow()
	openBreaker(t, b)
	clock.Advance(30 * time.Second)

	probe, _ := b.Allow()
	b.Report(slow, HealthFailure)
	if b.State() != BreakerHalfOpen {
		t.Fatalf("state = %s: a stale failure reopened the breaker", b.State())
	}
	b.Report(probe, HealthSuccess)
	if b.State() != BreakerHalfOpen {
		t.Fatalf("state = %s", b.State())
	}
}

func TestBreakerSnapshot(t *testing.T) {
	b, clock := newTestBreaker(t)
	record(t, b, HealthSuccess)
	record(t, b, HealthFailure)
	got := b.Snapshot()
	if got.State != BreakerClosed || got.Successes != 1 || got.Failures != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
	clock.Advance(11 * time.Second)
	if got := b.Snapshot(); got.Successes != 0 || got.Failures != 0 {
		t.Fatalf("snapshot after the window = %+v", got)
	}
}

func TestBreakerIsSafeForConcurrentUse(t *testing.T) {
	b, clock := newTestBreaker(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 2000 {
				if permit, ok := b.Allow(); ok {
					switch (i + j) % 3 {
					case 0:
						b.Report(permit, HealthSuccess)
					case 1:
						b.Report(permit, HealthFailure)
					default:
						b.Report(permit, HealthNeutral)
					}
				}
				if j%100 == 0 {
					clock.Advance(time.Second)
				}
			}
		})
	}
	wg.Wait()
	_ = b.State()
}
