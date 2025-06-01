package accounting

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
)

// Attempt states. See docs/adr/0002-postgres-is-the-budget-authority.md.
const (
	// StateReserved holds liability, and proves nothing was sent, because dispatch
	// intent commits before sending.
	StateReserved = "reserved"
	// StateDispatched may have been sent, so may have cost money.
	StateDispatched = "dispatched"
	// StateUncertain may have cost money, and nobody knows how much yet.
	StateUncertain = "uncertain"
	// StateReconciled is final: its charge is settled.
	StateReconciled = "reconciled"
	// StateReleased is final: it was never sent, and costs nothing.
	StateReleased = "released"
)

// How a charge was decided.
const (
	// ChargeReported comes from the provider's usage report.
	ChargeReported = "reported"
	// ChargeRejected is an explicit refusal by the provider, which charges nothing.
	ChargeRejected = "rejected"
	// ChargeConservative is the full reservation, because the real cost is unknown.
	ChargeConservative = "conservative"
)

const (
	entryCharge         = "charge"
	entryLateAdjustment = "late_adjustment"

	outcomeLeaseExpired = "lease_expired"

	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

var (
	// ErrBudgetExhausted means an attempt's reservation doesn't fit within the budget.
	ErrBudgetExhausted = errors.New("accounting: budget exhausted")
	// ErrNoPrice means a route has no configured price. An attempt can't be bounded
	// without one, so it's rejected rather than treated as free.
	ErrNoPrice = errors.New("accounting: no price for this route")
	// ErrNotReserved means an attempt can't be sent. It may have been released after its
	// lease ran out, in which case sending it would spend released money.
	ErrNotReserved = errors.New("accounting: attempt is not reserved")
	// ErrUnavailable means the ledger couldn't be reached. Admission fails closed on it.
	ErrUnavailable = errors.New("accounting: ledger unavailable")
	// ErrUnknownAttempt means no such attempt exists.
	ErrUnknownAttempt = errors.New("accounting: unknown attempt")
	// ErrUnknownTeam means no such team exists.
	ErrUnknownTeam = errors.New("accounting: unknown team")
)

// RejectedCharge says how to treat a provider's explicit refusal, with no usage.
type RejectedCharge int

const (
	// RejectedChargeNone charges nothing. Providers don't normally bill for a request
	// they refused, but this is a policy choice rather than something they guarantee.
	RejectedChargeNone RejectedCharge = iota
	// RejectedChargeConservative treats a refusal like any other unknown cost.
	RejectedChargeConservative
)

// LedgerOptions configures a Ledger.
type LedgerOptions struct {
	Pool   *pgxpool.Pool
	Prices *PriceBook
	// Now replaces the clock, for tests.
	Now func() time.Time
	// UncertainGrace is how long to wait for late usage before charging conservatively.
	UncertainGrace time.Duration
	// RejectedAttemptCharge says how to treat a provider's explicit refusal.
	RejectedAttemptCharge RejectedCharge
}

// Ledger is the authority for money and outstanding liability. Everything that changes
// either happens in one Postgres transaction, so replicas can't disagree.
//
// Lock order is always attempt, then period, one attempt per transaction, so that
// concurrent settlement and reconciliation can't deadlock.
type Ledger struct {
	pool           *pgxpool.Pool
	prices         *PriceBook
	now            func() time.Time
	uncertainGrace time.Duration
	rejected       RejectedCharge

	pinnedMu sync.RWMutex
	pinned   map[pinnedKey]Prices // old price lists, which never change
}

type pinnedKey struct {
	version         int64
	provider, model string
}

// NewLedger returns a Ledger.
func NewLedger(opts LedgerOptions) *Ledger {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Ledger{
		pool:           opts.Pool,
		prices:         opts.Prices,
		now:            now,
		uncertainGrace: opts.UncertainGrace,
		rejected:       opts.RejectedAttemptCharge,
		pinned:         make(map[pinnedKey]Prices),
	}
}

func unavailable(err error) error {
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// ReserveRequest asks to hold liability for one attempt.
type ReserveRequest struct {
	RequestID string
	TeamID    string
	KeyID     string
	Alias     string
	Stream    bool
	// Number is this attempt's position within the request, from 1.
	Number   int
	Provider string
	Model    string
	// InputBound and OutputBound are the most tokens this attempt could use.
	InputBound  int64
	OutputBound int64
	// Lease is how long before a reconciler may decide this was never sent.
	Lease time.Duration
}

// Reservation is held liability for one attempt.
type Reservation struct {
	AttemptID      string
	PeriodID       int64
	ReservedMicros int64
}

// periodBounds returns the calendar month containing t, in UTC.
func periodBounds(t time.Time) (start, end time.Time) {
	t = t.UTC()
	start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// Reserve admits an attempt if settled spend, outstanding liability, and its own
// reservation all fit within the limit. Locking the period row makes the check and the
// reservation atomic across every replica.
func (l *Ledger) Reserve(ctx context.Context, r ReserveRequest) (*Reservation, error) {
	prices, ok := l.prices.Lookup(r.Provider, r.Model)
	if !ok {
		return nil, ErrNoPrice
	}
	amount, err := prices.Reservation(r.InputBound, r.OutputBound)
	if err != nil {
		return nil, err
	}

	now := l.now()
	start, end := periodBounds(now)
	out := &Reservation{AttemptID: "att_" + rand.Text(), ReservedMicros: amount}

	err = pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO requests (id, team_id, key_id, alias, stream, created_at) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO NOTHING`, r.RequestID, r.TeamID, r.KeyID, r.Alias, r.Stream, now); err != nil {
			return err
		}
		// A period is created on first use, from the team's limit at that moment.
		if _, err := tx.Exec(ctx, `
			INSERT INTO budget_periods (team_id, starts_at, ends_at, limit_micros)
			SELECT id, $2, $3, budget_limit_micros FROM teams WHERE id = $1
			ON CONFLICT (team_id, starts_at) DO NOTHING`, r.TeamID, start, end); err != nil {
			return err
		}

		var limit, settled, reserved int64
		err := tx.QueryRow(ctx, `
			SELECT id, limit_micros, settled_micros, reserved_micros FROM budget_periods
			WHERE team_id = $1 AND starts_at = $2 FOR UPDATE`, r.TeamID, start).Scan(&out.PeriodID, &limit, &settled, &reserved)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownTeam
		}
		if err != nil {
			return err
		}

		held, err := addChecked(settled, reserved)
		if err != nil {
			return err
		}
		total, err := addChecked(held, amount)
		if err != nil {
			return err
		}
		if total > limit {
			return ErrBudgetExhausted
		}

		if _, err := tx.Exec(ctx, `UPDATE budget_periods SET reserved_micros = reserved_micros + $2 WHERE id = $1`, out.PeriodID, amount); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO attempts (id, request_id, number, team_id, budget_period_id, price_version_id, provider, model,
			  input_bound_tokens, output_bound_tokens, reserved_micros, state, lease_expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			out.AttemptID, r.RequestID, r.Number, r.TeamID, out.PeriodID, l.prices.VersionID(), r.Provider, r.Model,
			r.InputBound, r.OutputBound, amount, StateReserved, now.Add(r.Lease), now)
		return err
	})
	if err != nil {
		return nil, classifyReserveError(err)
	}
	return out, nil
}

func classifyReserveError(err error) error {
	switch {
	case errors.Is(err, ErrBudgetExhausted), errors.Is(err, ErrUnknownTeam), errors.Is(err, ErrAmountOutOfRange):
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgForeignKeyViolation:
			return ErrUnknownTeam
		case pgUniqueViolation:
			return fmt.Errorf("accounting: attempt already exists: %w", err)
		}
	}
	return unavailable(err)
}

// MarkDispatched commits the intent to send an attempt, which must happen before
// sending. After this the attempt is assumed to have cost money until shown otherwise.
//
// ErrNotReserved means it must not be sent: a reconciler may have released it after its
// lease ran out, and its money may already be spent elsewhere.
func (l *Ledger) MarkDispatched(ctx context.Context, attemptID string, lease time.Duration) error {
	now := l.now()
	tag, err := l.pool.Exec(ctx, `
		UPDATE attempts SET state = $2, dispatched_at = $3, lease_expires_at = $4
		WHERE id = $1 AND state = $5`, attemptID, StateDispatched, now, now.Add(lease), StateReserved)
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotReserved
	}
	return nil
}

// FinishRequest reports how an attempt ended.
type FinishRequest struct {
	AttemptID      string
	Outcome        Outcome
	ErrorKind      provider.Kind
	Delivery       provider.Delivery
	UpstreamStatus int
	UpstreamCode   string
	Usage          *chat.Usage
}

// Settlement is what Finish decided.
type Settlement struct {
	ChargeMicros int64
	Kind         string
	// Uncertain means the cost isn't known yet, and the liability stays held.
	Uncertain bool
	// AlreadySettled means an earlier call settled this attempt, and nothing changed.
	AlreadySettled bool
	// OverReservation means the charge exceeded what was reserved, so the budget can be
	// exceeded by the difference. That's the documented bounded risk, made visible.
	OverReservation bool
}

type disposition int

const (
	dispositionReported disposition = iota
	dispositionReleased
	dispositionRejected
	dispositionUncertain
)

// disposition applies the charge policy from ADR 0002. Anything that may have cost money
// and has no trustworthy report is uncertain, never zero.
func (l *Ledger) disposition(f FinishRequest) disposition {
	switch {
	case f.Usage != nil && (f.Usage.State == chat.UsageReported || f.Usage.State == chat.UsageInconsistent):
		return dispositionReported
	case f.Usage != nil:
		return dispositionUncertain
	case f.Delivery == provider.DeliveryNotSent:
		return dispositionReleased
	case f.UpstreamStatus != 0 && l.rejected == RejectedChargeNone:
		return dispositionRejected
	default:
		return dispositionUncertain
	}
}

type attemptRow struct {
	state           string
	reservedMicros  int64
	periodID        int64
	priceVersion    int64
	provider, model string
	chargeMicros    *int64
	chargeKind      *string
}

// lockAttempt reads an attempt for update. With skipLocked it returns false for an
// attempt another transaction holds, which someone else is already handling.
func lockAttempt(ctx context.Context, tx pgx.Tx, id string, skipLocked bool) (attemptRow, bool, error) {
	query := `SELECT state, reserved_micros, budget_period_id, price_version_id, provider, model, charge_micros, charge_kind
		FROM attempts WHERE id = $1 FOR UPDATE`
	if skipLocked {
		query += " SKIP LOCKED"
	}
	var a attemptRow
	err := tx.QueryRow(ctx, query, id).Scan(&a.state, &a.reservedMicros, &a.periodID, &a.priceVersion, &a.provider, &a.model, &a.chargeMicros, &a.chargeKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	return a, true, nil
}

// Finish records how an attempt ended, and settles it if the cost is known. It's
// idempotent: once an attempt is final, repeating this changes nothing.
func (l *Ledger) Finish(ctx context.Context, f FinishRequest) (Settlement, error) {
	var out Settlement
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		a, found, err := lockAttempt(ctx, tx, f.AttemptID, false)
		if err != nil {
			return err
		}
		if !found {
			return ErrUnknownAttempt
		}
		if a.state == StateReconciled || a.state == StateReleased {
			out = Settlement{AlreadySettled: true}
			if a.chargeMicros != nil {
				out.ChargeMicros = *a.chargeMicros
			}
			if a.chargeKind != nil {
				out.Kind = *a.chargeKind
			}
			return nil
		}

		decision := l.disposition(f)
		var charge int64
		if decision == dispositionReported {
			prices, err := l.pinnedPrices(ctx, tx, a)
			if err != nil {
				return err
			}
			charge, err = prices.Charge(f.Usage)
			if err != nil {
				// A report that can't support a charge is an unknown cost.
				decision = dispositionUncertain
			}
		}

		now := l.now()
		switch decision {
		case dispositionReported:
			out = Settlement{ChargeMicros: charge, Kind: ChargeReported, OverReservation: charge > a.reservedMicros}
			return settle(ctx, tx, f.AttemptID, a, f, now, charge, ChargeReported, entryCharge)
		case dispositionRejected:
			out = Settlement{Kind: ChargeRejected}
			return settle(ctx, tx, f.AttemptID, a, f, now, 0, ChargeRejected, entryCharge)
		case dispositionReleased:
			return release(ctx, tx, f.AttemptID, a, &f, now)
		default:
			out = Settlement{Uncertain: true}
			return markUncertain(ctx, tx, f.AttemptID, f, now, now.Add(l.uncertainGrace))
		}
	})
	if err != nil {
		if errors.Is(err, ErrUnknownAttempt) {
			return Settlement{}, err
		}
		return Settlement{}, unavailable(err)
	}
	return out, nil
}

// pinnedPrices returns the prices an attempt was admitted under. Old price lists never
// change, so they can be kept once read.
func (l *Ledger) pinnedPrices(ctx context.Context, tx pgx.Tx, a attemptRow) (Prices, error) {
	if a.priceVersion == l.prices.VersionID() {
		if p, ok := l.prices.Lookup(a.provider, a.model); ok {
			return p, nil
		}
	}
	key := pinnedKey{a.priceVersion, a.provider, a.model}
	l.pinnedMu.RLock()
	p, ok := l.pinned[key]
	l.pinnedMu.RUnlock()
	if ok {
		return p, nil
	}
	err := tx.QueryRow(ctx, `
		SELECT input_pico, cached_input_pico, cache_write_pico, output_pico FROM prices
		WHERE price_version_id = $1 AND provider = $2 AND model = $3`, a.priceVersion, a.provider, a.model).
		Scan(&p.Input, &p.CachedInput, &p.CacheWrite, &p.Output)
	if err != nil {
		return Prices{}, fmt.Errorf("reading pinned prices: %w", err)
	}
	l.pinnedMu.Lock()
	l.pinned[key] = p
	l.pinnedMu.Unlock()
	return p, nil
}

// usageColumns flattens a usage report for storage. Nothing here holds content.
func usageColumns(u *chat.Usage) (state *string, prompt, completion, cached, write, reasoning *int64, raw any) {
	if u == nil {
		return nil, nil, nil, nil, nil, nil, nil
	}
	s := string(u.State)
	state = &s
	cached, write, reasoning = u.CachedPromptTokens, u.CacheWritePromptTokens, u.ReasoningTokens
	if u.State != chat.UsageMalformed {
		prompt, completion = &u.PromptTokens, &u.CompletionTokens
	}
	if json.Valid(u.Raw) {
		raw = string(u.Raw)
	}
	return state, prompt, completion, cached, write, reasoning, raw
}

func nullableInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// settle finishes an attempt with a known charge, in the caller's transaction. The state
// transition and the ledger's unique key each make a repeat harmless.
func settle(ctx context.Context, tx pgx.Tx, id string, a attemptRow, f FinishRequest, now time.Time, charge int64, kind, entry string) error {
	usageState, prompt, completion, cached, write, reasoning, raw := usageColumns(f.Usage)
	if _, err := tx.Exec(ctx, `
		UPDATE attempts SET state = $2, finished_at = $3, settled_at = $3, charge_micros = $4, charge_kind = $5, over_reservation = $6,
		  outcome = $7, error_kind = $8, delivery = $9, upstream_status = $10, upstream_code = $11,
		  usage_state = $12, prompt_tokens = $13, completion_tokens = $14, cached_tokens = $15, cache_write_tokens = $16, reasoning_tokens = $17, raw_usage = $18
		WHERE id = $1`,
		id, StateReconciled, now, charge, kind, charge > a.reservedMicros,
		string(f.Outcome), nullableString(string(f.ErrorKind)), nullableString(string(f.Delivery)), nullableInt(f.UpstreamStatus), nullableString(f.UpstreamCode),
		usageState, prompt, completion, cached, write, reasoning, raw); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (attempt_id, kind, amount_micros, created_at) VALUES ($1, $2, $3, $4)`, id, entry, charge, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE budget_periods SET settled_micros = settled_micros + $2, reserved_micros = reserved_micros - $3 WHERE id = $1`,
		a.periodID, charge, a.reservedMicros)
	return err
}

func release(ctx context.Context, tx pgx.Tx, id string, a attemptRow, f *FinishRequest, now time.Time) error {
	outcome, errorKind, delivery := "", "", ""
	if f != nil {
		outcome, errorKind, delivery = string(f.Outcome), string(f.ErrorKind), string(f.Delivery)
	} else {
		outcome = outcomeLeaseExpired
	}
	if _, err := tx.Exec(ctx, `
		UPDATE attempts SET state = $2, finished_at = $3, settled_at = $3, outcome = $4, error_kind = $5, delivery = $6 WHERE id = $1`,
		id, StateReleased, now, outcome, nullableString(errorKind), nullableString(delivery)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE budget_periods SET reserved_micros = reserved_micros - $2 WHERE id = $1`, a.periodID, a.reservedMicros)
	return err
}

func markUncertain(ctx context.Context, tx pgx.Tx, id string, f FinishRequest, now, graceUntil time.Time) error {
	usageState, prompt, completion, cached, write, reasoning, raw := usageColumns(f.Usage)
	_, err := tx.Exec(ctx, `
		UPDATE attempts SET state = $2, finished_at = $3, lease_expires_at = $4,
		  outcome = $5, error_kind = $6, delivery = $7, upstream_status = $8, upstream_code = $9,
		  usage_state = $10, prompt_tokens = $11, completion_tokens = $12, cached_tokens = $13, cache_write_tokens = $14, reasoning_tokens = $15, raw_usage = $16
		WHERE id = $1`,
		id, StateUncertain, now, graceUntil,
		string(f.Outcome), nullableString(string(f.ErrorKind)), nullableString(string(f.Delivery)), nullableInt(f.UpstreamStatus), nullableString(f.UpstreamCode),
		usageState, prompt, completion, cached, write, reasoning, raw)
	return err
}

// ReconcileResult counts what one pass did.
type ReconcileResult struct {
	Released              int
	MarkedUncertain       int
	ChargedConservatively int
}

// Reconcile resolves attempts whose owner has gone quiet. Any replica can run it, and
// any number at once, since each attempt is handled in its own transaction and skipped if
// another replica is on it.
//
// A lease running out only moves who may decide. It never shows that a provider stopped
// billing, so a dispatched attempt is never released: only an attempt still in state
// reserved, which proves it was never sent, can be.
func (l *Ledger) Reconcile(ctx context.Context, batch int) (ReconcileResult, error) {
	var result ReconcileResult
	now := l.now()

	released, err := l.reconcileEach(ctx, StateReserved, now, batch, func(tx pgx.Tx, id string, a attemptRow) error {
		return release(ctx, tx, id, a, nil, now)
	})
	if err != nil {
		return result, err
	}
	result.Released = released

	// A dispatched attempt whose lease ran out has an owner that crashed or lost contact.
	tag, err := l.pool.Exec(ctx, `
		UPDATE attempts SET state = $1, lease_expires_at = $2, outcome = COALESCE(outcome, $3)
		WHERE id IN (
		  SELECT id FROM attempts WHERE state = $4 AND lease_expires_at < $5
		  ORDER BY lease_expires_at LIMIT $6 FOR UPDATE SKIP LOCKED
		)`, StateUncertain, now.Add(l.uncertainGrace), outcomeLeaseExpired, StateDispatched, now, batch)
	if err != nil {
		return result, unavailable(err)
	}
	result.MarkedUncertain = int(tag.RowsAffected())

	charged, err := l.reconcileEach(ctx, StateUncertain, now, batch, func(tx pgx.Tx, id string, a attemptRow) error {
		// The reserved amount is the most this attempt could have cost under the pricing
		// model, so charging it can't undercharge. Late usage can still correct it.
		return settleConservatively(ctx, tx, id, a, now)
	})
	if err != nil {
		return result, err
	}
	result.ChargedConservatively = charged
	return result, nil
}

func settleConservatively(ctx context.Context, tx pgx.Tx, id string, a attemptRow, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE attempts SET state = $2, settled_at = $3, charge_micros = $4, charge_kind = $5 WHERE id = $1`,
		id, StateReconciled, now, a.reservedMicros, ChargeConservative); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (attempt_id, kind, amount_micros, created_at) VALUES ($1, $2, $3, $4)`, id, entryCharge, a.reservedMicros, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE budget_periods SET settled_micros = settled_micros + $2, reserved_micros = reserved_micros - $2 WHERE id = $1`,
		a.periodID, a.reservedMicros)
	return err
}

// reconcileEach applies action to expired attempts in a state, one transaction each, and
// rechecks the state under the lock, since another replica may have got there first.
func (l *Ledger) reconcileEach(ctx context.Context, state string, now time.Time, batch int, action func(pgx.Tx, string, attemptRow) error) (int, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id FROM attempts WHERE state = $1 AND lease_expires_at < $2 ORDER BY lease_expires_at LIMIT $3`, state, now, batch)
	if err != nil {
		return 0, unavailable(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, unavailable(err)
	}

	done := 0
	for _, id := range ids {
		acted := false
		err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
			a, found, err := lockAttempt(ctx, tx, id, true)
			if err != nil || !found || a.state != state {
				return err
			}
			acted = true
			return action(tx, id, a)
		})
		if err != nil {
			return done, unavailable(err)
		}
		if acted {
			done++
		}
	}
	return done, nil
}

// ApplyLateUsage applies a usage report that arrived after an attempt ended. It reports
// whether anything changed. A conservative charge is corrected exactly once, by the
// difference, which may be negative. A charge already based on a report is left alone.
func (l *Ledger) ApplyLateUsage(ctx context.Context, attemptID string, usage *chat.Usage) (bool, error) {
	if usage == nil || (usage.State != chat.UsageReported && usage.State != chat.UsageInconsistent) {
		return false, nil
	}
	applied := false
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		a, found, err := lockAttempt(ctx, tx, attemptID, false)
		if err != nil {
			return err
		}
		if !found {
			return ErrUnknownAttempt
		}
		prices, err := l.pinnedPrices(ctx, tx, a)
		if err != nil {
			return err
		}
		charge, err := prices.Charge(usage)
		if err != nil {
			return nil //nolint:nilerr // A report that can't support a charge changes nothing.
		}
		now := l.now()

		switch {
		case a.state == StateUncertain || a.state == StateDispatched:
			applied = true
			f := FinishRequest{AttemptID: attemptID, Usage: usage}
			return settleLate(ctx, tx, attemptID, a, f, now, charge)
		case a.state == StateReconciled && a.chargeKind != nil && *a.chargeKind == ChargeConservative && a.chargeMicros != nil:
			applied = true
			return adjustCharge(ctx, tx, attemptID, a, usage, now, charge)
		default:
			return nil
		}
	})
	if err != nil {
		if errors.Is(err, ErrUnknownAttempt) {
			return false, err
		}
		return false, unavailable(err)
	}
	return applied, nil
}

// settleLate settles an attempt that's still open, keeping what its end recorded.
func settleLate(ctx context.Context, tx pgx.Tx, id string, a attemptRow, f FinishRequest, now time.Time, charge int64) error {
	usageState, prompt, completion, cached, write, reasoning, raw := usageColumns(f.Usage)
	if _, err := tx.Exec(ctx, `
		UPDATE attempts SET state = $2, finished_at = COALESCE(finished_at, $3), settled_at = $3, charge_micros = $4, charge_kind = $5, over_reservation = $6,
		  usage_state = $7, prompt_tokens = $8, completion_tokens = $9, cached_tokens = $10, cache_write_tokens = $11, reasoning_tokens = $12, raw_usage = $13
		WHERE id = $1`,
		id, StateReconciled, now, charge, ChargeReported, charge > a.reservedMicros,
		usageState, prompt, completion, cached, write, reasoning, raw); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (attempt_id, kind, amount_micros, created_at) VALUES ($1, $2, $3, $4)`, id, entryCharge, charge, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE budget_periods SET settled_micros = settled_micros + $2, reserved_micros = reserved_micros - $3 WHERE id = $1`,
		a.periodID, charge, a.reservedMicros)
	return err
}

// adjustCharge corrects a conservative charge. The attempt's charge kind changes in the
// same transaction, and the ledger's unique key backs that up, so it happens only once.
func adjustCharge(ctx context.Context, tx pgx.Tx, id string, a attemptRow, usage *chat.Usage, now time.Time, charge int64) error {
	difference := charge - *a.chargeMicros
	usageState, prompt, completion, cached, write, reasoning, raw := usageColumns(usage)
	if _, err := tx.Exec(ctx, `
		UPDATE attempts SET charge_micros = $2, charge_kind = $3, over_reservation = $4,
		  usage_state = $5, prompt_tokens = $6, completion_tokens = $7, cached_tokens = $8, cache_write_tokens = $9, reasoning_tokens = $10, raw_usage = $11
		WHERE id = $1`,
		id, charge, ChargeReported, charge > a.reservedMicros,
		usageState, prompt, completion, cached, write, reasoning, raw); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (attempt_id, kind, amount_micros, created_at) VALUES ($1, $2, $3, $4)`, id, entryLateAdjustment, difference, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE budget_periods SET settled_micros = settled_micros + $2 WHERE id = $1`, a.periodID, difference)
	return err
}

// SetLimit changes a team's budget, including for the current period. Lowering it below
// existing obligations is allowed: it blocks new work and doesn't touch work in flight.
func (l *Ledger) SetLimit(ctx context.Context, teamID string, limitMicros int64) error {
	start, end := periodBounds(l.now())
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE teams SET budget_limit_micros = $2 WHERE id = $1`, teamID, limitMicros)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrUnknownTeam
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO budget_periods (team_id, starts_at, ends_at, limit_micros) VALUES ($1, $2, $3, $4)
			ON CONFLICT (team_id, starts_at) DO UPDATE SET limit_micros = EXCLUDED.limit_micros`, teamID, start, end, limitMicros)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrUnknownTeam) {
			return err
		}
		return unavailable(err)
	}
	return nil
}

// Budget is a team's position in the current period.
type Budget struct {
	LimitMicros     int64 `json:"limit_micros"`
	SettledMicros   int64 `json:"settled_micros"`
	ReservedMicros  int64 `json:"reserved_micros"`
	AvailableMicros int64 `json:"available_micros"`
}

// Budget returns a team's position in the current period.
func (l *Ledger) Budget(ctx context.Context, teamID string) (Budget, error) {
	start, _ := periodBounds(l.now())
	var b Budget
	err := l.pool.QueryRow(ctx, `
		SELECT COALESCE(p.limit_micros, t.budget_limit_micros), COALESCE(p.settled_micros, 0), COALESCE(p.reserved_micros, 0)
		FROM teams t LEFT JOIN budget_periods p ON p.team_id = t.id AND p.starts_at = $2
		WHERE t.id = $1`, teamID, start).Scan(&b.LimitMicros, &b.SettledMicros, &b.ReservedMicros)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrUnknownTeam
	}
	if err != nil {
		return b, unavailable(err)
	}
	b.AvailableMicros = b.LimitMicros - b.SettledMicros - b.ReservedMicros
	return b, nil
}
