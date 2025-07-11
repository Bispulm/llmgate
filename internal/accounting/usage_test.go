package accounting_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
)

func TestUsageSummarizesTheCurrentPeriod(t *testing.T) {
	e := newEnv(t, 100_000)
	ctx := context.Background()
	ok := e.dispatched("r1")
	if _, err := e.ledger.Finish(ctx, finished(ok.AttemptID, reported(40, 10))); err != nil {
		t.Fatal(err)
	}
	open := e.dispatched("r2")
	if _, err := e.ledger.Finish(ctx, finished(open.AttemptID, nil)); err != nil {
		t.Fatal(err)
	}

	usage, err := e.ledger.Usage(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Requests != 2 || usage.Attempts[accounting.StateReconciled] != 1 || usage.Attempts[accounting.StateUncertain] != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Budget.SettledMicros != 60 || usage.Budget.ReservedMicros != open.ReservedMicros {
		t.Fatalf("budget = %+v", usage.Budget)
	}
	if len(usage.Routes) != 1 {
		t.Fatalf("routes = %+v", usage.Routes)
	}
	route := usage.Routes[0]
	if route.Provider != provName || route.Model != model || route.Attempts != 2 || route.ChargeMicros != 60 ||
		route.PromptTokens != 40 || route.CompletionTokens != 10 || route.Open != 1 {
		t.Fatalf("route = %+v", route)
	}
	if !usage.PeriodStart.Before(usage.PeriodEnd) {
		t.Fatalf("period = %v to %v", usage.PeriodStart, usage.PeriodEnd)
	}

	attempts, err := e.ledger.Attempts(ctx, team, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].ID != open.AttemptID || attempts[1].ID != ok.AttemptID {
		t.Fatalf("attempts = %+v", attempts)
	}
	if attempts[1].ChargeMicros == nil || *attempts[1].ChargeMicros != 60 || attempts[1].ChargeKind != accounting.ChargeReported {
		t.Fatalf("attempt = %+v", attempts[1])
	}
}

func TestUsageBelongsToOneTeam(t *testing.T) {
	e := newEnv(t, 100_000)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO teams (id, name, budget_limit_micros) VALUES ('other', 'other', 100)`); err != nil {
		t.Fatal(err)
	}
	r := e.dispatched("mine")
	if _, err := e.ledger.Finish(ctx, finished(r.AttemptID, reported(1, 1))); err != nil {
		t.Fatal(err)
	}
	usage, err := e.ledger.Usage(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Requests != 0 || len(usage.Routes) != 0 || usage.Budget.SettledMicros != 0 || usage.Budget.LimitMicros != 100 {
		t.Fatalf("usage = %+v", usage)
	}
	attempts, err := e.ledger.Attempts(ctx, "other", 10)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("attempts = %+v, %v", attempts, err)
	}
	if _, err := e.ledger.Usage(ctx, "nobody"); err == nil {
		t.Fatal("no error for an unknown team")
	}
}

func TestParseUSDIsExact(t *testing.T) {
	for in, want := range map[string]int64{"5": 5_000_000, "0.000001": 1, "12.34": 12_340_000} {
		got, err := accounting.ParseUSD(in)
		if err != nil || got != want {
			t.Errorf("%s = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "-1", "1e3", "0.0000001", "1,000"} {
		if _, err := accounting.ParseUSD(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestTheReconcilerRunsUntilStopped(t *testing.T) {
	e := newEnv(t, 100_000)
	r := e.reserve("abandoned")
	e.clock.Advance(2 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	reports := make(chan accounting.ReconcileResult, 10)
	go func() {
		defer close(done)
		e.ledger.RunReconciler(ctx, 10*time.Millisecond, 100, func(result accounting.ReconcileResult, err error) {
			if err == nil {
				reports <- result
			}
		})
	}()
	select {
	case got := <-reports:
		if got.Released != 1 {
			t.Fatalf("result = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reconciler never ran")
	}
	if state, _, _ := e.attemptState(r.AttemptID); state != accounting.StateReleased {
		t.Fatalf("state = %s", state)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconciler didn't stop")
	}
}

// A backlog bigger than a batch clears in one go, rather than one batch per interval.
func TestTheReconcilerClearsABacklog(t *testing.T) {
	e := newEnv(t, 100_000)
	for i := range 5 {
		e.reserve(fmt.Sprintf("backlog-%d", i))
	}
	e.clock.Advance(2 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.ledger.RunReconciler(ctx, time.Hour, 2, func(result accounting.ReconcileResult, _ error) {
			released += result.Released
			if released >= 5 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("released %d before the next interval", released)
	}
	if got := e.period(); got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
}
