package accounting_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/testenv"
)

// clock is a settable time shared by a test's ledgers.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

const (
	team     = "team-a"
	key      = "key-a"
	provName = "prov"
	model    = "model"

	// Prices: $1 per MTok input, $2 output, so a token is 1 or 2 micro-USD.
	inputPico  = 1_000_000
	outputPico = 2_000_000
)

type env struct {
	t      *testing.T
	url    string
	pool   *pgxpool.Pool
	clock  *clock
	ledger *accounting.Ledger
	book   *accounting.PriceBook
}

func testPriceList(label string, output int64) accounting.PriceList {
	return accounting.PriceList{Label: label, Entries: []accounting.PriceEntry{{
		Provider: provName, Model: model,
		Prices: accounting.Prices{Input: inputPico, CachedInput: 100_000, CacheWrite: inputPico, Output: output},
	}}}
}

func newEnv(t *testing.T, limitMicros int64) *env {
	t.Helper()
	url := testenv.PostgresURL(t)
	pool := testenv.PoolFor(t, url, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO teams (id, name, budget_limit_micros) VALUES ($1, $1, $2)`, team, limitMicros); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO api_keys (id, team_id, name, secret_hash) VALUES ($1, $2, 'k', '\x00')`, key, team); err != nil {
		t.Fatal(err)
	}
	book, err := accounting.EnsurePrices(ctx, pool, testPriceList("v1", outputPico))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, url: url, pool: pool, clock: newClock(), book: book}
	e.ledger = e.newLedger(pool)
	return e
}

func (e *env) newLedger(pool *pgxpool.Pool) *accounting.Ledger {
	return accounting.NewLedger(accounting.LedgerOptions{
		Pool:           pool,
		Prices:         e.book,
		Now:            e.clock.Now,
		UncertainGrace: time.Minute,
	})
}

func reserveRequest(requestID string, number int) accounting.ReserveRequest {
	return accounting.ReserveRequest{
		RequestID:   requestID,
		TeamID:      team,
		KeyID:       key,
		Alias:       "alias",
		Stream:      false,
		Number:      number,
		Provider:    provName,
		Model:       model,
		InputBound:  100,
		OutputBound: 50,
		Lease:       time.Minute,
	}
}

// reservedFor is the reservation for reserveRequest: 100*1 + 50*2 micro-USD.
const reservedFor = 200

func (e *env) reserve(requestID string) *accounting.Reservation {
	e.t.Helper()
	r, err := e.ledger.Reserve(context.Background(), reserveRequest(requestID, 1))
	if err != nil {
		e.t.Fatalf("reserve %s: %v", requestID, err)
	}
	return r
}

func (e *env) dispatched(requestID string) *accounting.Reservation {
	e.t.Helper()
	r := e.reserve(requestID)
	if err := e.ledger.MarkDispatched(context.Background(), r.AttemptID, time.Minute); err != nil {
		e.t.Fatal(err)
	}
	return r
}

type periodState struct {
	limit, settled, reserved int64
}

func (e *env) period() periodState {
	e.t.Helper()
	var p periodState
	err := e.pool.QueryRow(context.Background(), `
		SELECT limit_micros, settled_micros, reserved_micros FROM budget_periods
		WHERE team_id = $1 ORDER BY starts_at DESC LIMIT 1`, team).Scan(&p.limit, &p.settled, &p.reserved)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// checkBooks verifies that the period counters agree with the attempts and the ledger,
// which are what they summarize. It's run after every scenario.
func (e *env) checkBooks() {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT p.id, p.settled_micros, p.reserved_micros,
		  COALESCE((SELECT sum(l.amount_micros) FROM ledger_entries l JOIN attempts a ON a.id = l.attempt_id WHERE a.budget_period_id = p.id), 0),
		  COALESCE((SELECT sum(a.reserved_micros) FROM attempts a WHERE a.budget_period_id = p.id AND a.state IN ('reserved', 'dispatched', 'uncertain')), 0)
		FROM budget_periods p`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, settled, reserved, ledgerSum, openSum int64
		if err := rows.Scan(&id, &settled, &reserved, &ledgerSum, &openSum); err != nil {
			e.t.Fatal(err)
		}
		if settled != ledgerSum {
			e.t.Errorf("period %d: settled %d, but the ledger sums to %d", id, settled, ledgerSum)
		}
		if reserved != openSum {
			e.t.Errorf("period %d: reserved %d, but open attempts sum to %d", id, reserved, openSum)
		}
	}
}

func (e *env) attemptState(id string) (state string, charge *int64, kind *string) {
	e.t.Helper()
	err := e.pool.QueryRow(context.Background(), `SELECT state, charge_micros, charge_kind FROM attempts WHERE id = $1`, id).Scan(&state, &charge, &kind)
	if err != nil {
		e.t.Fatal(err)
	}
	return state, charge, kind
}

func reported(prompt, completion int64) *chat.Usage {
	return &chat.Usage{
		State:            chat.UsageReported,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		Raw:              json.RawMessage(fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d}`, prompt, completion)),
	}
}

func finished(attemptID string, usage *chat.Usage) accounting.FinishRequest {
	return accounting.FinishRequest{
		AttemptID: attemptID,
		Outcome:   accounting.OutcomeCompleted,
		Delivery:  provider.DeliveryConfirmed,
		Usage:     usage,
	}
}

func TestReserveHoldsLiability(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.reserve("r1")
	if r.ReservedMicros != reservedFor {
		t.Fatalf("reserved = %d, want %d", r.ReservedMicros, reservedFor)
	}
	if got := e.period(); got != (periodState{limit: 1000, settled: 0, reserved: reservedFor}) {
		t.Fatalf("period = %+v", got)
	}
	if state, _, _ := e.attemptState(r.AttemptID); state != accounting.StateReserved {
		t.Fatalf("state = %q", state)
	}
	e.checkBooks()
}

func TestReserveRecordsHowItWasComputed(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.reserve("r1")
	var inputBound, outputBound, priceVersion int64
	var providerName, modelName string
	err := e.pool.QueryRow(context.Background(), `
		SELECT input_bound_tokens, output_bound_tokens, price_version_id, provider, model FROM attempts WHERE id = $1`,
		r.AttemptID).Scan(&inputBound, &outputBound, &priceVersion, &providerName, &modelName)
	if err != nil {
		t.Fatal(err)
	}
	if inputBound != 100 || outputBound != 50 || priceVersion != e.book.VersionID() || providerName != provName || modelName != model {
		t.Fatalf("attempt = %d %d %d %s %s", inputBound, outputBound, priceVersion, providerName, modelName)
	}
}

func TestReserveEnforcesTheLimitExactly(t *testing.T) {
	e := newEnv(t, 2*reservedFor)
	e.reserve("r1")
	e.reserve("r2") // Lands exactly on the limit, which is allowed.

	_, err := e.ledger.Reserve(context.Background(), reserveRequest("r3", 1))
	if !errors.Is(err, accounting.ErrBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	if got := e.period().reserved; got != 2*reservedFor {
		t.Fatalf("a refused reservation changed liability: %d", got)
	}
	e.checkBooks()
}

func TestSettledSpendAndLiabilityBothCount(t *testing.T) {
	e := newEnv(t, 300)
	first := e.dispatched("r1")
	if _, err := e.ledger.Finish(context.Background(), finished(first.AttemptID, reported(50, 25))); err != nil {
		t.Fatal(err)
	}
	// 100 settled and 200 available.
	e.reserve("r2")
	if _, err := e.ledger.Reserve(context.Background(), reserveRequest("r3", 1)); !errors.Is(err, accounting.ErrBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	e.checkBooks()
}

func TestReserveRejectsWhatItCantPrice(t *testing.T) {
	e := newEnv(t, 1000)
	r := reserveRequest("r1", 1)
	r.Model = "unpriced"
	if _, err := e.ledger.Reserve(context.Background(), r); !errors.Is(err, accounting.ErrNoPrice) {
		t.Fatalf("error = %v", err)
	}
	r = reserveRequest("r2", 1)
	r.InputBound = 1 << 62
	r.OutputBound = 1 << 62
	if _, err := e.ledger.Reserve(context.Background(), r); !errors.Is(err, accounting.ErrAmountOutOfRange) {
		t.Fatalf("error = %v", err)
	}
	e.checkBooks()
}

func TestEachAttemptHoldsItsOwnReservation(t *testing.T) {
	e := newEnv(t, 1000)
	first, err := e.ledger.Reserve(context.Background(), reserveRequest("r1", 1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.ledger.Reserve(context.Background(), reserveRequest("r1", 2))
	if err != nil {
		t.Fatal(err)
	}
	if first.AttemptID == second.AttemptID {
		t.Fatal("attempts share an ID")
	}
	if got := e.period().reserved; got != 2*reservedFor {
		t.Fatalf("reserved = %d", got)
	}
	var requests int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM requests WHERE id = 'r1'`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
	if _, err := e.ledger.Reserve(context.Background(), reserveRequest("r1", 2)); err == nil {
		t.Fatal("the same attempt number was reserved twice")
	}
	e.checkBooks()
}

// This is the invariant. Atomicity comes from the database row lock, so it runs through
// separate connection pools, as separate gateway replicas would, on a real server.
func TestConcurrentAdmissionAcrossReplicasIsExact(t *testing.T) {
	const (
		replicas = 6
		clients  = 400
		allowed  = 25
	)
	e := newEnv(t, allowed*reservedFor)
	ledgers := make([]*accounting.Ledger, replicas)
	for i := range ledgers {
		ledgers[i] = e.newLedger(testenv.PoolFor(t, e.url, 6))
	}

	var admitted, refused atomic.Int64
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			_, err := ledgers[i%replicas].Reserve(context.Background(), reserveRequest(fmt.Sprintf("r%d", i), 1))
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.Is(err, accounting.ErrBudgetExhausted):
				refused.Add(1)
			default:
				t.Errorf("request %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	if got := admitted.Load(); got != allowed {
		t.Fatalf("admitted %d, want exactly %d", got, allowed)
	}
	if got := refused.Load(); got != clients-allowed {
		t.Fatalf("refused %d", got)
	}
	got := e.period()
	if got.settled+got.reserved > got.limit {
		t.Fatalf("the invariant broke: %+v", got)
	}
	e.checkBooks()
}

func TestSettlementChargesActualUsage(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	settlement, err := e.ledger.Finish(context.Background(), finished(r.AttemptID, reported(40, 10)))
	if err != nil {
		t.Fatal(err)
	}
	const charge = 40*1 + 10*2
	if settlement.ChargeMicros != charge || settlement.Kind != accounting.ChargeReported {
		t.Fatalf("settlement = %+v", settlement)
	}
	if got := e.period(); got.settled != charge || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
	state, gotCharge, kind := e.attemptState(r.AttemptID)
	if state != accounting.StateReconciled || *gotCharge != charge || *kind != accounting.ChargeReported {
		t.Fatalf("attempt = %q %v %v", state, gotCharge, kind)
	}

	// What the provider said is kept alongside the normalized counts.
	var raw []byte
	var prompt, completion int64
	var usageState string
	err = e.pool.QueryRow(context.Background(), `SELECT raw_usage, prompt_tokens, completion_tokens, usage_state FROM attempts WHERE id = $1`, r.AttemptID).Scan(&raw, &prompt, &completion, &usageState)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != 40 || completion != 10 || usageState != string(chat.UsageReported) || !json.Valid(raw) {
		t.Fatalf("usage = %d %d %s %s", prompt, completion, usageState, raw)
	}
	e.checkBooks()
}

func TestSettlementIsIdempotent(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	first, err := e.ledger.Finish(context.Background(), finished(r.AttemptID, reported(40, 10)))
	if err != nil {
		t.Fatal(err)
	}
	// A retry, even with different numbers, changes nothing.
	again, err := e.ledger.Finish(context.Background(), finished(r.AttemptID, reported(400, 100)))
	if err != nil {
		t.Fatal(err)
	}
	if again.ChargeMicros != first.ChargeMicros || !again.AlreadySettled {
		t.Fatalf("second settlement = %+v", again)
	}
	if got := e.period(); got.settled != first.ChargeMicros || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
	e.checkBooks()
}

func TestConcurrentSettlementChargesOnce(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	replicas := []*accounting.Ledger{e.ledger, e.newLedger(testenv.PoolFor(t, e.url, 4)), e.newLedger(testenv.PoolFor(t, e.url, 4))}
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			if _, err := replicas[i%len(replicas)].Finish(context.Background(), finished(r.AttemptID, reported(40, 10))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := e.period(); got.settled != 60 || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
	e.checkBooks()
}

func TestChargingAboveTheReservationIsAppliedAndFlagged(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	settlement, err := e.ledger.Finish(context.Background(), finished(r.AttemptID, reported(500, 100)))
	if err != nil {
		t.Fatal(err)
	}
	if settlement.ChargeMicros != 700 || !settlement.OverReservation {
		t.Fatalf("settlement = %+v", settlement)
	}
	var flagged bool
	if err := e.pool.QueryRow(context.Background(), `SELECT over_reservation FROM attempts WHERE id = $1`, r.AttemptID).Scan(&flagged); err != nil {
		t.Fatal(err)
	}
	if !flagged {
		t.Fatal("not flagged")
	}
	e.checkBooks()
}

func TestAttemptsThatWereNeverSentAreReleased(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	settlement, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: r.AttemptID,
		Outcome:   accounting.OutcomeFailed,
		ErrorKind: provider.KindConnection,
		Delivery:  provider.DeliveryNotSent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.ChargeMicros != 0 {
		t.Fatalf("settlement = %+v", settlement)
	}
	if got := e.period(); got.settled != 0 || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
	if state, _, _ := e.attemptState(r.AttemptID); state != accounting.StateReleased {
		t.Fatalf("state = %q", state)
	}
	e.checkBooks()
}

// These all may have cost money. None can be treated as zero, and none can be released.
func TestAttemptsThatMayHaveCostMoneyKeepTheirLiability(t *testing.T) {
	cases := map[string]accounting.FinishRequest{
		"canceled before a response": {Outcome: accounting.OutcomeClientCanceled, ErrorKind: provider.KindCanceled, Delivery: provider.DeliveryUncertain},
		"timed out":                  {Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindTimeout, Delivery: provider.DeliveryUncertain},
		"dropped connection":         {Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindConnection, Delivery: provider.DeliveryUncertain},
		"interrupted stream":         {Outcome: accounting.OutcomeInterrupted, ErrorKind: provider.KindInterrupted, Delivery: provider.DeliveryConfirmed},
		"completed with no usage":    {Outcome: accounting.OutcomeCompleted, Delivery: provider.DeliveryConfirmed},
		"partial usage": {Outcome: accounting.OutcomeInterrupted, ErrorKind: provider.KindInterrupted, Delivery: provider.DeliveryConfirmed,
			Usage: &chat.Usage{State: chat.UsagePartial, PromptTokens: 10, CompletionTokens: 1}},
		"malformed usage": {Outcome: accounting.OutcomeCompleted, Delivery: provider.DeliveryConfirmed,
			Usage: &chat.Usage{State: chat.UsageMalformed, Raw: json.RawMessage(`{"nonsense":true}`)}},
	}
	for name, finish := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 1000)
			r := e.dispatched("r1")
			finish.AttemptID = r.AttemptID
			settlement, err := e.ledger.Finish(context.Background(), finish)
			if err != nil {
				t.Fatal(err)
			}
			if !settlement.Uncertain {
				t.Fatalf("settlement = %+v", settlement)
			}
			if state, charge, _ := e.attemptState(r.AttemptID); state != accounting.StateUncertain || charge != nil {
				t.Fatalf("attempt = %q %v", state, charge)
			}
			if got := e.period(); got.reserved != reservedFor || got.settled != 0 {
				t.Fatalf("period = %+v", got)
			}
			e.checkBooks()
		})
	}
}

func TestPartialUsageIsKeptForReconciliation(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	_, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: r.AttemptID, Outcome: accounting.OutcomeInterrupted, Delivery: provider.DeliveryConfirmed,
		Usage: &chat.Usage{State: chat.UsagePartial, PromptTokens: 10, CompletionTokens: 1, Raw: json.RawMessage(`{"input_tokens":10}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var prompt int64
	var usageState string
	if err := e.pool.QueryRow(context.Background(), `SELECT prompt_tokens, usage_state FROM attempts WHERE id = $1`, r.AttemptID).Scan(&prompt, &usageState); err != nil {
		t.Fatal(err)
	}
	if prompt != 10 || usageState != string(chat.UsagePartial) {
		t.Fatalf("usage = %d %q", prompt, usageState)
	}
}

func TestAProviderRefusalChargesNothingByDefault(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	settlement, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: r.AttemptID, Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindRateLimited,
		Delivery: provider.DeliveryConfirmed, UpstreamStatus: 429, UpstreamCode: "rate_limit_exceeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.ChargeMicros != 0 || settlement.Kind != accounting.ChargeRejected {
		t.Fatalf("settlement = %+v", settlement)
	}
	state, charge, kind := e.attemptState(r.AttemptID)
	if state != accounting.StateReconciled || *charge != 0 || *kind != accounting.ChargeRejected {
		t.Fatalf("attempt = %q %v %v", state, charge, kind)
	}
	if got := e.period(); got.reserved != 0 || got.settled != 0 {
		t.Fatalf("period = %+v", got)
	}
	e.checkBooks()
}

func TestAProviderRefusalCanBeChargedConservatively(t *testing.T) {
	e := newEnv(t, 1000)
	strict := accounting.NewLedger(accounting.LedgerOptions{
		Pool: e.pool, Prices: e.book, Now: e.clock.Now, UncertainGrace: time.Minute,
		RejectedAttemptCharge: accounting.RejectedChargeConservative,
	})
	r := e.dispatched("r1")
	settlement, err := strict.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: r.AttemptID, Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindServer,
		Delivery: provider.DeliveryConfirmed, UpstreamStatus: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !settlement.Uncertain {
		t.Fatalf("settlement = %+v", settlement)
	}
	e.checkBooks()
}

func TestAFailedAttemptAndItsFallbackAreBothAccounted(t *testing.T) {
	e := newEnv(t, 1000)
	first, err := e.ledger.Reserve(context.Background(), reserveRequest("r1", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ledger.MarkDispatched(context.Background(), first.AttemptID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: first.AttemptID, Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindTimeout, Delivery: provider.DeliveryUncertain,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := e.ledger.Reserve(context.Background(), reserveRequest("r1", 2))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ledger.MarkDispatched(context.Background(), second.AttemptID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ledger.Finish(context.Background(), finished(second.AttemptID, reported(40, 10))); err != nil {
		t.Fatal(err)
	}
	// The first attempt's liability is still held, and the second's charge is settled.
	if got := e.period(); got.settled != 60 || got.reserved != reservedFor {
		t.Fatalf("period = %+v", got)
	}
	e.checkBooks()
}

func TestDispatchIsConditional(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.reserve("r1")
	if err := e.ledger.MarkDispatched(context.Background(), r.AttemptID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := e.attemptState(r.AttemptID); state != accounting.StateDispatched {
		t.Fatalf("state = %q", state)
	}
	if err := e.ledger.MarkDispatched(context.Background(), r.AttemptID, time.Minute); !errors.Is(err, accounting.ErrNotReserved) {
		t.Fatalf("second dispatch: %v", err)
	}
	if err := e.ledger.MarkDispatched(context.Background(), "no-such-attempt", time.Minute); !errors.Is(err, accounting.ErrNotReserved) {
		t.Fatalf("unknown attempt: %v", err)
	}
}

func TestReconcileReleasesAttemptsThatWereNeverDispatched(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.reserve("r1")

	// Still within its lease: the gateway may be about to send it.
	result, err := e.ledger.Reconcile(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result != (accounting.ReconcileResult{}) {
		t.Fatalf("result = %+v", result)
	}
	if state, _, _ := e.attemptState(r.AttemptID); state != accounting.StateReserved {
		t.Fatalf("state = %q", state)
	}

	e.clock.Advance(2 * time.Minute)
	result, err = e.ledger.Reconcile(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Released != 1 {
		t.Fatalf("result = %+v", result)
	}
	if state, _, _ := e.attemptState(r.AttemptID); state != accounting.StateReleased {
		t.Fatalf("state = %q", state)
	}
	if got := e.period(); got.reserved != 0 || got.settled != 0 {
		t.Fatalf("period = %+v", got)
	}

	// A gateway that was only slow now knows it must not send.
	if err := e.ledger.MarkDispatched(context.Background(), r.AttemptID, time.Minute); !errors.Is(err, accounting.ErrNotReserved) {
		t.Fatalf("dispatch after release: %v", err)
	}
	e.checkBooks()
}

// A lease expiring moves who may reconcile an attempt. It says nothing about whether
// the provider billed, so it must never release money.
func TestALeaseExpiringNeverReleasesADispatchedAttempt(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")

	e.clock.Advance(2 * time.Minute)
	result, err := e.ledger.Reconcile(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.MarkedUncertain != 1 || result.Released != 0 {
		t.Fatalf("result = %+v", result)
	}
	if state, _, _ := e.attemptState(r.AttemptID); state != accounting.StateUncertain {
		t.Fatalf("state = %q", state)
	}
	if got := e.period(); got.reserved != reservedFor || got.settled != 0 {
		t.Fatalf("period = %+v: the liability must remain", got)
	}
	e.checkBooks()
}

func TestReconcileChargesUncertainAttemptsConservativelyAfterAGrace(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	if _, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: r.AttemptID, Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindTimeout, Delivery: provider.DeliveryUncertain,
	}); err != nil {
		t.Fatal(err)
	}

	// Within the grace, late usage could still arrive.
	result, err := e.ledger.Reconcile(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.ChargedConservatively != 0 {
		t.Fatalf("result = %+v", result)
	}

	e.clock.Advance(2 * time.Minute)
	result, err = e.ledger.Reconcile(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.ChargedConservatively != 1 {
		t.Fatalf("result = %+v", result)
	}
	state, charge, kind := e.attemptState(r.AttemptID)
	if state != accounting.StateReconciled || *charge != reservedFor || *kind != accounting.ChargeConservative {
		t.Fatalf("attempt = %q %v %v", state, charge, kind)
	}
	if got := e.period(); got.settled != reservedFor || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
	e.checkBooks()
}

func TestConcurrentReconcilersChargeOnce(t *testing.T) {
	e := newEnv(t, 100*reservedFor)
	const attempts = 30
	for i := range attempts {
		e.dispatched(fmt.Sprintf("r%d", i))
	}
	e.clock.Advance(2 * time.Minute)

	replicas := []*accounting.Ledger{e.ledger, e.newLedger(testenv.PoolFor(t, e.url, 4)), e.newLedger(testenv.PoolFor(t, e.url, 4))}
	run := func() {
		var wg sync.WaitGroup
		for _, l := range replicas {
			for range 3 {
				wg.Go(func() {
					if _, err := l.Reconcile(context.Background(), 100); err != nil {
						t.Error(err)
					}
				})
			}
		}
		wg.Wait()
	}
	run() // dispatched -> uncertain
	e.clock.Advance(2 * time.Minute)
	run() // uncertain -> charged

	got := e.period()
	if got.settled != attempts*reservedFor || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
	e.checkBooks()
}

func TestSettlingAfterAttemptWasMarkedUncertainStillUsesRealUsage(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	e.clock.Advance(2 * time.Minute)
	if _, err := e.ledger.Reconcile(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	// A slow gateway finishes after its lease ran out.
	settlement, err := e.ledger.Finish(context.Background(), finished(r.AttemptID, reported(40, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if settlement.ChargeMicros != 60 || settlement.Kind != accounting.ChargeReported {
		t.Fatalf("settlement = %+v", settlement)
	}
	e.checkBooks()
}

func TestLateUsageAdjustsAConservativeChargeOnce(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	if _, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: r.AttemptID, Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindTimeout, Delivery: provider.DeliveryUncertain,
	}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Minute)
	if _, err := e.ledger.Reconcile(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if got := e.period().settled; got != reservedFor {
		t.Fatalf("settled = %d", got)
	}

	applied, err := e.ledger.ApplyLateUsage(context.Background(), r.AttemptID, reported(40, 10))
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("late usage wasn't applied")
	}
	if got := e.period().settled; got != 60 {
		t.Fatalf("settled = %d, want the real charge", got)
	}
	state, charge, kind := e.attemptState(r.AttemptID)
	if state != accounting.StateReconciled || *charge != 60 || *kind != accounting.ChargeReported {
		t.Fatalf("attempt = %q %v %v", state, charge, kind)
	}

	// Duplicates, even with other numbers, change nothing.
	for range 3 {
		applied, err := e.ledger.ApplyLateUsage(context.Background(), r.AttemptID, reported(1, 1))
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			t.Fatal("a duplicate was applied")
		}
	}
	if got := e.period().settled; got != 60 {
		t.Fatalf("settled = %d", got)
	}
	e.checkBooks()
}

func TestLateUsageCanRaiseACharge(t *testing.T) {
	e := newEnv(t, 10_000)
	r := e.dispatched("r1")
	if _, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: r.AttemptID, Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindTimeout, Delivery: provider.DeliveryUncertain,
	}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Minute)
	if _, err := e.ledger.Reconcile(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ledger.ApplyLateUsage(context.Background(), r.AttemptID, reported(500, 100)); err != nil {
		t.Fatal(err)
	}
	if got := e.period().settled; got != 700 {
		t.Fatalf("settled = %d", got)
	}
	e.checkBooks()
}

func TestLateUsageBeforeReconciliationSettlesNormally(t *testing.T) {
	e := newEnv(t, 1000)
	r := e.dispatched("r1")
	if _, err := e.ledger.Finish(context.Background(), accounting.FinishRequest{
		AttemptID: r.AttemptID, Outcome: accounting.OutcomeFailed, ErrorKind: provider.KindTimeout, Delivery: provider.DeliveryUncertain,
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := e.ledger.ApplyLateUsage(context.Background(), r.AttemptID, reported(40, 10))
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("late usage wasn't applied")
	}
	if got := e.period(); got.settled != 60 || got.reserved != 0 {
		t.Fatalf("period = %+v", got)
	}
	e.clock.Advance(2 * time.Minute)
	result, err := e.ledger.Reconcile(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.ChargedConservatively != 0 {
		t.Fatalf("a settled attempt was charged again: %+v", result)
	}
	e.checkBooks()
}

func TestLateUsageIgnoresAttemptsItCantApplyTo(t *testing.T) {
	e := newEnv(t, 1000)
	settled := e.dispatched("r1")
	if _, err := e.ledger.Finish(context.Background(), finished(settled.AttemptID, reported(40, 10))); err != nil {
		t.Fatal(err)
	}
	if applied, err := e.ledger.ApplyLateUsage(context.Background(), settled.AttemptID, reported(1, 1)); err != nil || applied {
		t.Fatalf("an exact charge was adjusted: %v, %v", applied, err)
	}
	if applied, err := e.ledger.ApplyLateUsage(context.Background(), settled.AttemptID, &chat.Usage{State: chat.UsagePartial}); err != nil || applied {
		t.Fatalf("untrusted usage was applied: %v, %v", applied, err)
	}
	if _, err := e.ledger.ApplyLateUsage(context.Background(), "no-such-attempt", reported(1, 1)); !errors.Is(err, accounting.ErrUnknownAttempt) {
		t.Fatalf("error = %v", err)
	}
	e.checkBooks()
}

func TestRolloverKeepsOldLiabilitiesInTheirOwnPeriod(t *testing.T) {
	e := newEnv(t, 1000)
	e.clock.Set(time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC))
	march := e.dispatched("r1")

	e.clock.Set(time.Date(2026, 4, 1, 0, 1, 0, 0, time.UTC))
	april := e.reserve("r2")
	if march.PeriodID == april.PeriodID {
		t.Fatal("a new month used the old period")
	}
	if got := e.period(); got.reserved != reservedFor || got.settled != 0 {
		t.Fatalf("April = %+v", got)
	}

	// March's attempt settles against March, after April has begun.
	if _, err := e.ledger.Finish(context.Background(), finished(march.AttemptID, reported(40, 10))); err != nil {
		t.Fatal(err)
	}
	var settled, reserved int64
	if err := e.pool.QueryRow(context.Background(), `SELECT settled_micros, reserved_micros FROM budget_periods WHERE id = $1`, march.PeriodID).Scan(&settled, &reserved); err != nil {
		t.Fatal(err)
	}
	if settled != 60 || reserved != 0 {
		t.Fatalf("March = %d settled, %d reserved", settled, reserved)
	}
	if got := e.period(); got.settled != 0 {
		t.Fatalf("March's charge landed in April: %+v", got)
	}
	e.checkBooks()
}

func TestANewPeriodStartsFromTheTeamsCurrentLimit(t *testing.T) {
	e := newEnv(t, 1000)
	e.reserve("r1")
	if _, err := e.pool.Exec(context.Background(), `UPDATE teams SET budget_limit_micros = 5000 WHERE id = $1`, team); err != nil {
		t.Fatal(err)
	}
	if got := e.period().limit; got != 1000 {
		t.Fatalf("an existing period's limit changed on its own: %d", got)
	}
	e.clock.Advance(31 * 24 * time.Hour)
	e.reserve("r2")
	if got := e.period().limit; got != 5000 {
		t.Fatalf("limit = %d", got)
	}
}

func TestLoweringALimitBlocksNewWorkButKeepsObligations(t *testing.T) {
	e := newEnv(t, 1000)
	first := e.dispatched("r1")
	e.reserve("r2")

	if err := e.ledger.SetLimit(context.Background(), team, 100); err != nil {
		t.Fatal(err)
	}
	got := e.period()
	if got.limit != 100 || got.reserved != 2*reservedFor {
		t.Fatalf("period = %+v: obligations must not change", got)
	}
	if _, err := e.ledger.Reserve(context.Background(), reserveRequest("r3", 1)); !errors.Is(err, accounting.ErrBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	// Work already admitted still settles.
	if _, err := e.ledger.Finish(context.Background(), finished(first.AttemptID, reported(40, 10))); err != nil {
		t.Fatal(err)
	}
	// Raising it again lets new work in.
	if err := e.ledger.SetLimit(context.Background(), team, 10_000); err != nil {
		t.Fatal(err)
	}
	e.reserve("r3")
	e.checkBooks()
}

func TestAChargeUsesThePriceVersionPinnedAtAdmission(t *testing.T) {
	e := newEnv(t, 100_000)
	r := e.dispatched("r1")

	newer, err := accounting.EnsurePrices(context.Background(), e.pool, testPriceList("v2", 100*outputPico))
	if err != nil {
		t.Fatal(err)
	}
	if newer.VersionID() == e.book.VersionID() {
		t.Fatal("a new version has the same ID")
	}
	replica := accounting.NewLedger(accounting.LedgerOptions{Pool: e.pool, Prices: newer, Now: e.clock.Now, UncertainGrace: time.Minute})

	settlement, err := replica.Finish(context.Background(), finished(r.AttemptID, reported(40, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if settlement.ChargeMicros != 60 {
		t.Fatalf("charge = %d: it should use the prices from admission", settlement.ChargeMicros)
	}

	// Later admissions use the new version.
	later, err := replica.Reserve(context.Background(), reserveRequest("r2", 1))
	if err != nil {
		t.Fatal(err)
	}
	if later.ReservedMicros <= reservedFor {
		t.Fatalf("reserved = %d", later.ReservedMicros)
	}
	e.checkBooks()
}

func TestPriceListsAreImmutable(t *testing.T) {
	e := newEnv(t, 1000)
	ctx := context.Background()

	same, err := accounting.EnsurePrices(ctx, e.pool, testPriceList("v1", outputPico))
	if err != nil {
		t.Fatal(err)
	}
	if same.VersionID() != e.book.VersionID() {
		t.Fatalf("the same list got a new ID: %d and %d", same.VersionID(), e.book.VersionID())
	}
	if _, err := accounting.EnsurePrices(ctx, e.pool, testPriceList("v1", 3*outputPico)); !errors.Is(err, accounting.ErrPriceListChanged) {
		t.Fatalf("error = %v", err)
	}
}

func TestPriceListsSurviveConcurrentStartup(t *testing.T) {
	e := newEnv(t, 1000)
	var wg sync.WaitGroup
	ids := make([]int64, 8)
	for i := range ids {
		wg.Go(func() {
			book, err := accounting.EnsurePrices(context.Background(), e.pool, testPriceList("shared", outputPico))
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = book.VersionID()
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("replicas got different versions: %v", ids)
		}
	}
}

func TestDatabaseOutageFailsClosed(t *testing.T) {
	e := newEnv(t, 1000)
	down := testenv.PoolFor(t, e.url, 2)
	r := e.reserve("r1")
	broken := e.newLedger(down)
	down.Close()

	if _, err := broken.Reserve(context.Background(), reserveRequest("r2", 1)); !errors.Is(err, accounting.ErrUnavailable) {
		t.Fatalf("reserve: %v", err)
	}
	if err := broken.MarkDispatched(context.Background(), r.AttemptID, time.Minute); !errors.Is(err, accounting.ErrUnavailable) {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := broken.Finish(context.Background(), finished(r.AttemptID, reported(1, 1))); !errors.Is(err, accounting.ErrUnavailable) {
		t.Fatalf("finish: %v", err)
	}
	e.checkBooks()
}

func TestACanceledContextDoesNotLeaveAPartialReservation(t *testing.T) {
	e := newEnv(t, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.ledger.Reserve(ctx, reserveRequest("r1", 1)); err == nil {
		t.Fatal("a canceled reservation succeeded")
	}
	var attempts int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d", attempts)
	}
	e.checkBooks()
}

func TestSpendReporting(t *testing.T) {
	e := newEnv(t, 1000)
	settled := e.dispatched("r1")
	if _, err := e.ledger.Finish(context.Background(), finished(settled.AttemptID, reported(40, 10))); err != nil {
		t.Fatal(err)
	}
	e.reserve("r2")

	got, err := e.ledger.Budget(context.Background(), team)
	if err != nil {
		t.Fatal(err)
	}
	if got.LimitMicros != 1000 || got.SettledMicros != 60 || got.ReservedMicros != reservedFor {
		t.Fatalf("budget = %+v", got)
	}
	if got.AvailableMicros != 1000-60-reservedFor {
		t.Fatalf("available = %d", got.AvailableMicros)
	}
}
