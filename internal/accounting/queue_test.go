package accounting_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/testenv"
)

// A team's admissions all wait for one row lock. Waiting must not take every pooled
// connection with it, or attempts that were already admitted couldn't record their
// dispatch or settle, and would fail too.
func TestAdmissionsWaitingOnATeamLeaveConnectionsForOthers(t *testing.T) {
	e := newEnv(t, 1_000_000)
	ctx := context.Background()
	admitted := e.reserve("admitted-before")

	ledger := e.newLedger(testenv.PoolFor(t, e.url, 3))

	// Hold the team's period row, as a long admission elsewhere would.
	blocker, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT id FROM budget_periods WHERE team_id = $1 FOR UPDATE`, team); err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() { _, _ = ledger.Reserve(waitCtx, reserveRequest(fmt.Sprintf("queued-%d", i), 1)) })
	}
	time.Sleep(200 * time.Millisecond) // Let them queue.

	start := time.Now()
	dispatchCtx, cancelDispatch := context.WithTimeout(ctx, 2*time.Second)
	defer cancelDispatch()
	if err := ledger.MarkDispatched(dispatchCtx, admitted.AttemptID, time.Minute); err != nil {
		t.Fatalf("an admitted attempt couldn't dispatch while others waited: %v after %v", err, time.Since(start))
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

// A queued admission gives up when its context does.
func TestAQueuedAdmissionCanGiveUp(t *testing.T) {
	e := newEnv(t, 1_000_000)
	ctx := context.Background()
	e.reserve("creates-the-period")
	blocker, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT id FROM budget_periods WHERE team_id = $1 FOR UPDATE`, team); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = e.ledger.Reserve(context.Background(), reserveRequest("first", 1)) }()
	time.Sleep(100 * time.Millisecond)

	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := e.ledger.Reserve(short, reserveRequest("second", 1)); err == nil {
		t.Fatal("admitted while the row was held")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("gave up after %v", elapsed)
	}
}

// A refused reservation leaves nothing behind: no attempt, and no request either.
func TestARefusedReservationLeavesNothing(t *testing.T) {
	e := newEnv(t, reservedFor)
	e.reserve("opens-the-period-and-fits")
	if _, err := e.ledger.Reserve(context.Background(), reserveRequest("refused", 1)); err == nil {
		t.Fatal("admitted past the limit")
	}
	var requests, attempts int
	if err := e.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM requests WHERE id = 'refused'), (SELECT count(*) FROM attempts WHERE request_id = 'refused')`).Scan(&requests, &attempts); err != nil {
		t.Fatal(err)
	}
	if requests != 0 || attempts != 0 {
		t.Fatalf("a refusal left %d requests and %d attempts", requests, attempts)
	}
	if got := e.period(); got.reserved != reservedFor {
		t.Fatalf("period = %+v", got)
	}
}
