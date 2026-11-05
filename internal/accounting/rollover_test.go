package accounting_test

import (
	"context"
	"testing"
	"time"
)

func (e *env) periodLimit(start time.Time) int64 {
	e.t.Helper()
	var limit int64
	if err := e.pool.QueryRow(context.Background(), `SELECT limit_micros FROM budget_periods WHERE team_id = $1 AND starts_at = $2`, team, start).Scan(&limit); err != nil {
		e.t.Fatal(err)
	}
	return limit
}

// A limit change made just before the month turns, by a replica whose clock is still in
// the old month, has to reach a new month's period that another replica already opened.
func TestALimitChangeReachesAPeriodAlreadyOpenedAhead(t *testing.T) {
	e := newEnv(t, 100_000)
	ctx := context.Background()
	e.clock.Set(time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC))
	e.reserve("old-month")

	ahead := e.newLedger(e.pool)
	e.clock.Set(time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC))
	if _, err := ahead.Reserve(ctx, reserveRequest("new-month", 1)); err != nil {
		t.Fatal(err)
	}

	e.clock.Set(time.Date(2026, 3, 31, 23, 59, 59, 500_000_000, time.UTC))
	if err := e.ledger.SetLimit(ctx, team, 5_000); err != nil {
		t.Fatal(err)
	}
	if got := e.periodLimit(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)); got != 5_000 {
		t.Fatalf("the new month's limit is %d", got)
	}
	if got := e.periodLimit(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); got != 5_000 {
		t.Fatalf("the old month's limit is %d", got)
	}
}

// A period opened while a limit change is in flight waits for it, rather than starting
// from the old limit.
func TestAPeriodOpenedDuringALimitChangeGetsTheNewLimit(t *testing.T) {
	e := newEnv(t, 100_000)
	ctx := context.Background()

	change, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = change.Rollback(ctx) }()
	if _, err := change.Exec(ctx, `UPDATE teams SET budget_limit_micros = 5000 WHERE id = $1`, team); err != nil {
		t.Fatal(err)
	}

	opened := make(chan error, 1)
	go func() {
		_, err := e.ledger.Reserve(ctx, reserveRequest("during-change", 1))
		opened <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := change.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
	if got := e.periodLimit(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); got != 5_000 {
		t.Fatalf("the period opened with limit %d", got)
	}
}
