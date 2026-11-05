package limits

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustAcquire(t *testing.T, l *FairLimiter, key string) func() {
	t.Helper()
	release, err := l.Acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("acquire for %s: %v", key, err)
	}
	return release
}

// waiter starts an Acquire and reports when it's granted.
type waiter struct {
	key     string
	granted chan struct{}
	err     chan error
	release func()
}

func startWaiter(t *testing.T, ctx context.Context, l *FairLimiter, key string) *waiter {
	t.Helper()
	w := &waiter{key: key, granted: make(chan struct{}), err: make(chan error, 1)}
	before := l.Stats().Queued
	go func() {
		release, err := l.Acquire(ctx, key)
		if err != nil {
			w.err <- err
			return
		}
		w.release = release
		close(w.granted)
	}()
	// Wait until it's queued, so that the order of waiters is the order of calls.
	deadline := time.Now().Add(5 * time.Second)
	for l.Stats().Queued == before {
		if time.Now().After(deadline) {
			t.Fatal("waiter never queued")
		}
		time.Sleep(time.Millisecond)
	}
	return w
}

func (w *waiter) mustBeGranted(t *testing.T) {
	t.Helper()
	select {
	case <-w.granted:
	case err := <-w.err:
		t.Fatalf("%s failed: %v", w.key, err)
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was never granted", w.key)
	}
}

func (w *waiter) mustBeWaiting(t *testing.T) {
	t.Helper()
	select {
	case <-w.granted:
		t.Fatalf("%s was granted early", w.key)
	case err := <-w.err:
		t.Fatalf("%s failed: %v", w.key, err)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestAcquireUpToCapacityWithoutWaiting(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 2, MaxQueue: 1})
	first := mustAcquire(t, l, "a")
	second := mustAcquire(t, l, "b")
	if got := l.Stats(); got.InFlight != 2 || got.Queued != 0 {
		t.Fatalf("stats = %+v", got)
	}
	first()
	second()
	if got := l.Stats(); got.InFlight != 0 {
		t.Fatalf("stats after release = %+v", got)
	}
}

func TestNilLimiterAdmitsEverything(t *testing.T) {
	var l *FairLimiter
	release, err := l.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got := l.Stats(); got != (Stats{}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 1, MaxQueue: 1})
	release := mustAcquire(t, l, "a")
	release()
	release()
	if got := l.Stats(); got.InFlight != 0 {
		t.Fatalf("a repeated release went negative: %+v", got)
	}
	second := mustAcquire(t, l, "a")
	third := startWaiter(t, context.Background(), l, "a")
	release() // Must not free the slot second holds.
	third.mustBeWaiting(t)
	second()
	third.mustBeGranted(t)
	third.release()
}

func TestShedsWhenTheQueueIsFull(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 1, MaxQueue: 1})
	release := mustAcquire(t, l, "a")
	w := startWaiter(t, context.Background(), l, "b")

	if _, err := l.Acquire(context.Background(), "c"); !errors.Is(err, ErrShed) {
		t.Fatalf("error = %v", err)
	}
	if got := l.Stats(); got.Shed != 1 {
		t.Fatalf("stats = %+v", got)
	}
	release()
	w.mustBeGranted(t)
	w.release()
}

func TestNoQueueShedsImmediately(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 1})
	release := mustAcquire(t, l, "a")
	defer release()
	if _, err := l.Acquire(context.Background(), "b"); !errors.Is(err, ErrShed) {
		t.Fatalf("error = %v", err)
	}
}

func TestAKeyCantFillTheWholeQueue(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 1, MaxQueue: 10, MaxQueuePerKey: 2})
	release := mustAcquire(t, l, "greedy")
	first := startWaiter(t, context.Background(), l, "greedy")
	second := startWaiter(t, context.Background(), l, "greedy")

	if _, err := l.Acquire(context.Background(), "greedy"); !errors.Is(err, ErrShed) {
		t.Fatalf("error = %v", err)
	}
	other := startWaiter(t, context.Background(), l, "other")
	other.mustBeWaiting(t)

	release()
	first.mustBeGranted(t)
	first.release()
	other.mustBeGranted(t)
	other.release()
	second.mustBeGranted(t)
	second.release()
}

func TestWaitersWithinAKeyAreFIFO(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 1, MaxQueue: 3})
	release := mustAcquire(t, l, "a")
	first := startWaiter(t, context.Background(), l, "a")
	second := startWaiter(t, context.Background(), l, "a")
	third := startWaiter(t, context.Background(), l, "a")

	release()
	first.mustBeGranted(t)
	second.mustBeWaiting(t)
	first.release()
	second.mustBeGranted(t)
	third.mustBeWaiting(t)
	second.release()
	third.mustBeGranted(t)
	third.release()
}

// One team queueing many requests must not make another wait behind all of them.
func TestKeysAreServedRoundRobin(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 1, MaxQueue: 20})
	release := mustAcquire(t, l, "greedy")

	var greedy []*waiter
	for range 5 {
		greedy = append(greedy, startWaiter(t, context.Background(), l, "greedy"))
	}
	quiet := startWaiter(t, context.Background(), l, "quiet")

	release()
	greedy[0].mustBeGranted(t)
	greedy[1].mustBeWaiting(t)
	quiet.mustBeWaiting(t)

	greedy[0].release()
	quiet.mustBeGranted(t) // Second in line, despite arriving last of six.
	greedy[1].mustBeWaiting(t)
	quiet.release()

	for _, w := range greedy[1:] {
		w.mustBeGranted(t)
		w.release()
	}
}

func TestPerKeyInFlightLimit(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 4, MaxQueue: 10, MaxInFlightPerKey: 2})
	first := mustAcquire(t, l, "greedy")
	second := mustAcquire(t, l, "greedy")

	third := startWaiter(t, context.Background(), l, "greedy")
	third.mustBeWaiting(t)

	// Capacity remains, so another key isn't affected.
	other := mustAcquire(t, l, "other")
	third.mustBeWaiting(t)

	first()
	third.mustBeGranted(t)
	third.release()
	second()
	other()
}

func TestAWaiterAtItsLimitDoesntBlockOthers(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 2, MaxQueue: 10, MaxInFlightPerKey: 1})
	greedy := mustAcquire(t, l, "greedy")
	blocking := mustAcquire(t, l, "other")

	stuck := startWaiter(t, context.Background(), l, "greedy")
	quiet := startWaiter(t, context.Background(), l, "quiet")

	blocking()
	quiet.mustBeGranted(t) // greedy's waiter is first, but greedy is at its limit.
	stuck.mustBeWaiting(t)
	quiet.release()
	stuck.mustBeWaiting(t)

	greedy()
	stuck.mustBeGranted(t)
	stuck.release()
}

func TestCanceledWaiterLeavesTheQueue(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 1, MaxQueue: 2})
	release := mustAcquire(t, l, "a")
	ctx, cancel := context.WithCancel(context.Background())
	gone := startWaiter(t, ctx, l, "b")
	next := startWaiter(t, context.Background(), l, "c")

	cancel()
	select {
	case err := <-gone.err:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled waiter kept waiting")
	}
	if got := l.Stats(); got.Queued != 1 {
		t.Fatalf("stats = %+v", got)
	}

	// Its place doesn't swallow the slot.
	release()
	next.mustBeGranted(t)
	next.release()
	if got := l.Stats(); got.InFlight != 0 || got.Queued != 0 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestQueueTimeoutShedsSoAnotherRouteCanBeTried(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 1, MaxQueue: 2, QueueTimeout: 30 * time.Millisecond})
	release := mustAcquire(t, l, "a")
	defer release()

	start := time.Now()
	_, err := l.Acquire(context.Background(), "b")
	if !errors.Is(err, ErrShed) {
		t.Fatalf("error = %v", err)
	}
	if got := time.Since(start); got < 30*time.Millisecond {
		t.Fatalf("gave up after %v", got)
	}
	if got := l.Stats(); got.Queued != 0 {
		t.Fatalf("stats = %+v", got)
	}
}

// A grant racing with cancellation must not lose the slot.
func TestNoSlotsLeakUnderContention(t *testing.T) {
	const capacity = 4
	l := NewFairLimiter(Config{MaxConcurrency: capacity, MaxQueue: 16, MaxQueuePerKey: 8, MaxInFlightPerKey: 3})

	var running, peak, served atomic.Int64
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			key := fmt.Sprintf("team-%d", i%4)
			for j := range 300 {
				ctx, cancel := context.WithCancel(context.Background())
				if j%3 == 0 {
					cancel()
				}
				release, err := l.Acquire(ctx, key)
				if err == nil {
					now := running.Add(1)
					for {
						p := peak.Load()
						if now <= p || peak.CompareAndSwap(p, now) {
							break
						}
					}
					served.Add(1)
					running.Add(-1)
					release()
				}
				cancel()
			}
		})
	}
	wg.Wait()

	if got := peak.Load(); got > capacity {
		t.Fatalf("%d ran at once with capacity %d", got, capacity)
	}
	if served.Load() == 0 {
		t.Fatal("nothing was served")
	}
	got := l.Stats()
	if got.InFlight != 0 || got.Queued != 0 {
		t.Fatalf("slots leaked: %+v", got)
	}
	// Every slot must still be free. The keys differ because of the per-key limit.
	for i := range capacity {
		defer mustAcquire(t, l, fmt.Sprintf("final-%d", i))()
	}
}

func TestUnderSaturationAQuietKeyStillGetsItsTurn(t *testing.T) {
	l := NewFairLimiter(Config{MaxConcurrency: 2, MaxQueue: 100, MaxQueuePerKey: 50})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				release, err := l.Acquire(context.Background(), "greedy")
				if err != nil {
					time.Sleep(time.Millisecond)
					continue
				}
				time.Sleep(2 * time.Millisecond)
				release()
			}
		})
	}
	defer func() {
		close(stop)
		wg.Wait()
	}()

	time.Sleep(20 * time.Millisecond)
	for i := range 20 {
		start := time.Now()
		release, err := l.Acquire(context.Background(), "quiet")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		release()
		// With 2 slots of about 2ms, a quiet key's turn comes within a few grants. Far
		// more than that means it waited behind greedy's whole queue.
		if got := time.Since(start); got > 500*time.Millisecond {
			t.Fatalf("request %d waited %v", i, got)
		}
	}
}
