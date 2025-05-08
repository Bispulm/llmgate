// Package limits bounds how much work the gateway sends upstream, and who gets to
// send it when there isn't room for everyone.
package limits

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrShed means the limiter refused the work rather than queue it any longer, so
// that the caller can try elsewhere or fail fast.
var ErrShed = errors.New("limits: over capacity")

// Config configures a FairLimiter.
type Config struct {
	// MaxConcurrency is how many may run at once.
	MaxConcurrency int
	// MaxQueue is how many may wait in total. Zero sheds as soon as capacity is full.
	MaxQueue int
	// MaxQueuePerKey is how many one key may have waiting. Zero means no separate
	// limit. Without one, a key could fill the queue and shed everyone else.
	MaxQueuePerKey int
	// MaxInFlightPerKey is how many one key may have running. Zero means no limit.
	MaxInFlightPerKey int
	// QueueTimeout is how long to wait before shedding, leaving time to try elsewhere
	// within the request's deadline. Zero means only the context limits the wait.
	QueueTimeout time.Duration
}

// Stats is a limiter's current state for operators.
type Stats struct {
	InFlight int   `json:"in_flight"`
	Queued   int   `json:"queued"`
	Shed     int64 `json:"shed"`
}

// FairLimiter limits concurrency, serving waiting keys round robin and each key's
// waiters in order. A key queueing hundreds of requests gets one turn in each round,
// the same as a key with one, so that it can't starve the others. A nil FairLimiter
// admits everything.
//
// It holds state for this process only, and each replica has its own. Team-wide limits
// that must hold across replicas are enforced elsewhere.
type FairLimiter struct {
	cfg Config

	mu       sync.Mutex
	inFlight int
	queued   int
	shed     int64
	keys     map[string]*keyState
	ring     []*keyState // keys with waiters, in round-robin order
	next     int
}

type keyState struct {
	key      string
	inFlight int
	waiters  []*queuedWaiter
}

type queuedWaiter struct {
	state   *keyState
	ready   chan struct{}
	granted bool // guarded by the limiter's mutex
}

// NewFairLimiter returns a limiter.
func NewFairLimiter(cfg Config) *FairLimiter {
	return &FairLimiter{cfg: cfg, keys: make(map[string]*keyState)}
}

// Acquire waits for a slot for key. On success, the returned function gives the slot
// back, and can safely be called more than once.
func (l *FairLimiter) Acquire(ctx context.Context, key string) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("waiting for capacity: %w", err)
	}

	l.mu.Lock()
	state := l.keyFor(key)
	// After any release, slots are handed out until capacity is full or no waiter is
	// eligible. So a free slot here can't be jumping ahead of an eligible waiter.
	if l.inFlight < l.cfg.MaxConcurrency && l.eligible(state) {
		l.grant(state)
		l.mu.Unlock()
		return l.releaser(state), nil
	}
	if l.queued >= l.cfg.MaxQueue || (l.cfg.MaxQueuePerKey > 0 && len(state.waiters) >= l.cfg.MaxQueuePerKey) {
		l.shed++
		l.forgetIfIdle(state)
		l.mu.Unlock()
		return nil, ErrShed
	}
	w := &queuedWaiter{state: state, ready: make(chan struct{})}
	if len(state.waiters) == 0 {
		l.ring = append(l.ring, state)
	}
	state.waiters = append(state.waiters, w)
	l.queued++
	l.mu.Unlock()

	var timeout <-chan time.Time
	if l.cfg.QueueTimeout > 0 {
		timer := time.NewTimer(l.cfg.QueueTimeout)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-w.ready:
		return l.releaser(state), nil
	case <-ctx.Done():
		if l.withdraw(w) {
			return nil, fmt.Errorf("waiting for capacity: %w", ctx.Err())
		}
		// The slot was granted as the context ended, so it has to be given back.
		l.releaser(state)()
		return nil, fmt.Errorf("waiting for capacity: %w", ctx.Err())
	case <-timeout:
		if l.withdraw(w) {
			l.mu.Lock()
			l.shed++
			l.mu.Unlock()
			return nil, ErrShed
		}
		return l.releaser(state), nil
	}
}

func (l *FairLimiter) keyFor(key string) *keyState {
	state, ok := l.keys[key]
	if !ok {
		state = &keyState{key: key}
		l.keys[key] = state
	}
	return state
}

func (l *FairLimiter) eligible(state *keyState) bool {
	return l.cfg.MaxInFlightPerKey <= 0 || state.inFlight < l.cfg.MaxInFlightPerKey
}

func (l *FairLimiter) grant(state *keyState) {
	l.inFlight++
	state.inFlight++
}

func (l *FairLimiter) forgetIfIdle(state *keyState) {
	if state.inFlight == 0 && len(state.waiters) == 0 {
		delete(l.keys, state.key)
	}
}

// withdraw removes a waiter from the queue, reporting false if it was already granted.
func (l *FairLimiter) withdraw(w *queuedWaiter) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w.granted {
		return false
	}
	state := w.state
	for i, waiter := range state.waiters {
		if waiter == w {
			state.waiters = append(state.waiters[:i], state.waiters[i+1:]...)
			break
		}
	}
	l.queued--
	if len(state.waiters) == 0 {
		l.removeFromRing(state)
	}
	l.forgetIfIdle(state)
	return true
}

func (l *FairLimiter) removeFromRing(state *keyState) {
	for i, s := range l.ring {
		if s != state {
			continue
		}
		l.ring = append(l.ring[:i], l.ring[i+1:]...)
		if i < l.next {
			l.next--
		}
		break
	}
	if l.next >= len(l.ring) {
		l.next = 0
	}
}

func (l *FairLimiter) releaser(state *keyState) func() {
	var released atomic.Bool
	return func() {
		if released.Swap(true) {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.inFlight--
		state.inFlight--
		l.dispatch()
		l.forgetIfIdle(state)
	}
}

// dispatch hands free slots to eligible waiters, one key at a time.
func (l *FairLimiter) dispatch() {
	for l.inFlight < l.cfg.MaxConcurrency && l.grantNext() {
	}
}

func (l *FairLimiter) grantNext() bool {
	for range len(l.ring) {
		if l.next >= len(l.ring) {
			l.next = 0
		}
		state := l.ring[l.next]
		if !l.eligible(state) {
			l.next++
			continue
		}

		w := state.waiters[0]
		state.waiters = state.waiters[1:]
		l.queued--
		l.grant(state)
		w.granted = true
		close(w.ready)

		if len(state.waiters) == 0 {
			l.removeFromRing(state)
		} else {
			l.next++
		}
		return true
	}
	return false
}

// Stats returns the limiter's current state.
func (l *FairLimiter) Stats() Stats {
	if l == nil {
		return Stats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return Stats{InFlight: l.inFlight, Queued: l.queued, Shed: l.shed}
}
