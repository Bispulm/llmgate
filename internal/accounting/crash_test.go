package accounting_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/testenv"
)

// A settlement that dies partway, as when its replica crashes, has to leave nothing
// behind: not the state change, not the ledger entry, and not half a period update. The
// transaction is held inside its last statement, and its connection is then killed.
func TestACrashDuringSettlementLeavesNothingHalfDone(t *testing.T) {
	e := newEnv(t, 100_000)
	r := e.dispatched("crash-settle")
	ctx := context.Background()

	// Settlement updates the period last, so holding that row stops it there, after it has
	// already changed the attempt and written its ledger entry.
	blocker, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT id FROM budget_periods WHERE team_id = $1 FOR UPDATE`, team); err != nil {
		t.Fatal(err)
	}

	name := "settle_" + strings.ToLower(rand.Text())
	victim := e.newLedger(testenv.PoolFor(t, e.url+"&application_name="+name, 2))
	result := make(chan error, 1)
	go func() {
		_, err := victim.Finish(ctx, finished(r.AttemptID, reported(40, 10)))
		result <- err
	}()

	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := e.pool.QueryRow(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock' AND query LIKE '%UPDATE budget_periods%'`, name).Scan(&pid)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settlement never reached the period update: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := e.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, accounting.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	state, charge, _ := e.attemptState(r.AttemptID)
	var entries int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE attempt_id = $1`, r.AttemptID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if state != accounting.StateDispatched || charge != nil || entries != 0 {
		t.Fatalf("after the crash: state %s, charge %v, %d ledger entries", state, charge, entries)
	}
	if got := e.period(); got.settled != 0 || got.reserved != r.ReservedMicros {
		t.Fatalf("period = %+v", got)
	}
	e.checkBooks()

	// A retry, by the owner or anyone else, settles it once.
	for range 2 {
		if _, err := e.ledger.Finish(ctx, finished(r.AttemptID, reported(40, 10))); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.period(); got.settled != 60 || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
	e.checkBooks()
}
