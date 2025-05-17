package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Bispulm/llmgate/internal/store"
	"github.com/Bispulm/llmgate/internal/testenv"
)

func TestMigrationsApplyAndAreIdempotent(t *testing.T) {
	pool := testenv.Postgres(t, 2)
	ctx := context.Background()

	// A second run, as when replicas start together, changes nothing.
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"teams", "api_keys", "price_versions", "prices", "budget_periods", "requests", "attempts", "ledger_entries"} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

// These constraints are the last line of defense for money, and they hold even
// if application code is wrong.
func TestSchemaConstraints(t *testing.T) {
	pool := testenv.Postgres(t, 2)
	ctx := context.Background()

	setup := []string{
		`INSERT INTO teams (id, name, budget_limit_micros) VALUES ('t1', 'one', 1000)`,
		`INSERT INTO api_keys (id, team_id, name, secret_hash) VALUES ('k1', 't1', 'k', '\x00')`,
		`INSERT INTO price_versions (label, content_hash) VALUES ('v1', 'h')`,
		`INSERT INTO budget_periods (team_id, starts_at, ends_at, limit_micros) VALUES ('t1', '2026-01-01', '2026-02-01', 1000)`,
		`INSERT INTO requests (id, team_id, key_id, alias, stream) VALUES ('r1', 't1', 'k1', 'a', false)`,
		`INSERT INTO attempts (id, request_id, number, team_id, budget_period_id, price_version_id, provider, model,
		   input_bound_tokens, output_bound_tokens, reserved_micros, state, lease_expires_at)
		 VALUES ('a1', 'r1', 1, 't1', 1, 1, 'p', 'm', 1, 1, 1, 'reserved', now())`,
		`INSERT INTO ledger_entries (attempt_id, kind, amount_micros) VALUES ('a1', 'charge', 1)`,
	}
	for _, stmt := range setup {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	tests := []struct {
		name, stmt string
		code       string // Postgres error code
	}{
		{"a second charge for one attempt", `INSERT INTO ledger_entries (attempt_id, kind, amount_micros) VALUES ('a1', 'charge', 1)`, "23505"},
		{"a second attempt with the same number", `INSERT INTO attempts (id, request_id, number, team_id, budget_period_id, price_version_id, provider, model, input_bound_tokens, output_bound_tokens, reserved_micros, state, lease_expires_at) VALUES ('a2', 'r1', 1, 't1', 1, 1, 'p', 'm', 1, 1, 1, 'reserved', now())`, "23505"},
		{"an unknown attempt state", `UPDATE attempts SET state = 'lost' WHERE id = 'a1'`, "23514"},
		{"an unknown ledger entry kind", `INSERT INTO ledger_entries (attempt_id, kind, amount_micros) VALUES ('a1', 'refund', 1)`, "23514"},
		{"an unknown charge kind", `UPDATE attempts SET charge_kind = 'free' WHERE id = 'a1'`, "23514"},
		{"a negative charge", `UPDATE attempts SET charge_micros = -1 WHERE id = 'a1'`, "23514"},
		{"negative reserved liability", `UPDATE budget_periods SET reserved_micros = -1`, "23514"},
		{"negative settled spend", `UPDATE budget_periods SET settled_micros = -1`, "23514"},
		{"a negative limit", `UPDATE budget_periods SET limit_micros = -1`, "23514"},
		{"a period that ends before it starts", `INSERT INTO budget_periods (team_id, starts_at, ends_at, limit_micros) VALUES ('t1', '2026-03-01', '2026-02-01', 1)`, "23514"},
		{"two periods with one start", `INSERT INTO budget_periods (team_id, starts_at, ends_at, limit_micros) VALUES ('t1', '2026-01-01', '2026-03-01', 1)`, "23505"},
		{"a duplicate team name", `INSERT INTO teams (id, name, budget_limit_micros) VALUES ('t2', 'one', 1)`, "23505"},
		{"a key for no team", `INSERT INTO api_keys (id, team_id, name, secret_hash) VALUES ('k2', 'nope', 'k', '\x00')`, "23503"},
		{"a negative price", `INSERT INTO prices (price_version_id, provider, model, input_pico, cached_input_pico, cache_write_pico, output_pico) VALUES (1, 'p', 'm', -1, 0, 0, 0)`, "23514"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tt.stmt)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("error = %v, want a constraint violation", err)
			}
			if pgErr.Code != tt.code {
				t.Fatalf("code = %s (%s), want %s", pgErr.Code, pgErr.Message, tt.code)
			}
		})
	}
}

func TestOpenDoesNotLeakTheURLOnError(t *testing.T) {
	const password = "super-secret-password"
	_, err := store.Open(context.Background(), store.PoolConfig{URL: "postgres://user:" + password + "@host:notaport/db"})
	if err == nil {
		t.Fatal("Open accepted a malformed URL")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestSeparatePoolsShareASchema(t *testing.T) {
	url := testenv.PostgresURL(t)
	ctx := context.Background()
	first, second := testenv.PoolFor(t, url, 1), testenv.PoolFor(t, url, 1)

	if _, err := first.Exec(ctx, `INSERT INTO teams (id, name, budget_limit_micros) VALUES ('t', 'n', 1)`); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := second.QueryRow(ctx, `SELECT name FROM teams WHERE id = 't'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "n" {
		t.Fatalf("name = %q", name)
	}
}
