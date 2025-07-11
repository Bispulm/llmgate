package accounting

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Usage is a team's spending and activity in the current period.
type Usage struct {
	TeamID      string    `json:"team_id"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	Budget      Budget    `json:"budget"`
	Requests    int64     `json:"requests"`
	// Attempts counts attempts by state.
	Attempts map[string]int64 `json:"attempts"`
	Routes   []RouteUsage     `json:"routes"`
}

// RouteUsage is one route's share of a period. Tokens only count attempts whose usage
// was reported, and Open counts attempts whose cost isn't settled yet.
type RouteUsage struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Attempts         int64  `json:"attempts"`
	Open             int64  `json:"open"`
	ChargeMicros     int64  `json:"charge_micros"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// AttemptRecord is one attempt as stored, without any request or response content.
type AttemptRecord struct {
	ID               string    `json:"id"`
	RequestID        string    `json:"request_id"`
	Number           int       `json:"number"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	State            string    `json:"state"`
	Outcome          string    `json:"outcome,omitempty"`
	ErrorKind        string    `json:"error_kind,omitempty"`
	ReservedMicros   int64     `json:"reserved_micros"`
	ChargeMicros     *int64    `json:"charge_micros,omitempty"`
	ChargeKind       string    `json:"charge_kind,omitempty"`
	PromptTokens     *int64    `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64    `json:"completion_tokens,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// Usage returns a team's position and activity in the current period.
func (l *Ledger) Usage(ctx context.Context, teamID string) (Usage, error) {
	budget, err := l.Budget(ctx, teamID)
	if err != nil {
		return Usage{}, err
	}
	start, end := periodBounds(l.now())
	out := Usage{TeamID: teamID, PeriodStart: start, PeriodEnd: end, Budget: budget, Attempts: map[string]int64{}, Routes: []RouteUsage{}}

	err = pgx.BeginTxFunc(ctx, l.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		const inPeriod = `a.budget_period_id = (SELECT id FROM budget_periods WHERE team_id = $1 AND starts_at = $2)`
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT a.request_id) FROM attempts a WHERE `+inPeriod, teamID, start).Scan(&out.Requests); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT a.state, count(*) FROM attempts a WHERE `+inPeriod+` GROUP BY a.state`, teamID, start)
		if err != nil {
			return err
		}
		var state string
		var count int64
		if _, err := pgx.ForEachRow(rows, []any{&state, &count}, func() error {
			out.Attempts[state] = count
			return nil
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT a.provider, a.model, count(*),
			  count(*) FILTER (WHERE a.state IN ('reserved', 'dispatched', 'uncertain')),
			  COALESCE(sum(a.charge_micros), 0),
			  COALESCE(sum(a.prompt_tokens) FILTER (WHERE a.usage_state IN ('reported', 'inconsistent')), 0),
			  COALESCE(sum(a.completion_tokens) FILTER (WHERE a.usage_state IN ('reported', 'inconsistent')), 0)
			FROM attempts a WHERE `+inPeriod+`
			GROUP BY a.provider, a.model ORDER BY a.provider, a.model`, teamID, start)
		if err != nil {
			return err
		}
		out.Routes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RouteUsage, error) {
			var r RouteUsage
			err := row.Scan(&r.Provider, &r.Model, &r.Attempts, &r.Open, &r.ChargeMicros, &r.PromptTokens, &r.CompletionTokens)
			return r, err
		})
		return err
	})
	if err != nil {
		return Usage{}, unavailable(err)
	}
	return out, nil
}

// Attempts returns a team's most recent attempts, newest first.
func (l *Ledger) Attempts(ctx context.Context, teamID string, limit int) ([]AttemptRecord, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT id, request_id, number, provider, model, state, COALESCE(outcome, ''), COALESCE(error_kind, ''),
		  reserved_micros, charge_micros, COALESCE(charge_kind, ''), prompt_tokens, completion_tokens, created_at
		FROM attempts WHERE team_id = $1 ORDER BY created_at DESC, request_id DESC, number DESC LIMIT $2`, teamID, limit)
	if err != nil {
		return nil, unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AttemptRecord, error) {
		var a AttemptRecord
		err := row.Scan(&a.ID, &a.RequestID, &a.Number, &a.Provider, &a.Model, &a.State, &a.Outcome, &a.ErrorKind,
			&a.ReservedMicros, &a.ChargeMicros, &a.ChargeKind, &a.PromptTokens, &a.CompletionTokens, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// RunReconciler runs reconciliation passes until ctx ends. A pass that fills its batch is
// followed straight away by another, so a backlog after a crash clears quickly.
func (l *Ledger) RunReconciler(ctx context.Context, interval time.Duration, batch int, report func(ReconcileResult, error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for {
			result, err := l.Reconcile(ctx, batch)
			if report != nil && (err != nil || result != (ReconcileResult{})) {
				report(result, err)
			}
			if err != nil || (result.Released < batch && result.MarkedUncertain < batch && result.ChargedConservatively < batch) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
