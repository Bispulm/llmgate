package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pgUniqueViolation = "23505"

var (
	// ErrTeamExists means a team with that ID already exists.
	ErrTeamExists = errors.New("auth: team already exists")
	// ErrInvalidTeam means a team's ID or limits aren't acceptable.
	ErrInvalidTeam = errors.New("auth: invalid team")
)

// teamID keeps IDs plain, because they end up in Redis keys, log fields, and metric
// labels, where braces, spaces, or unbounded length would cause trouble.
var teamID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Team is a tenant. Its budget is in micro-USD, and changes through the ledger, so that
// the current period changes with it.
type Team struct {
	ID                string     `json:"id"`
	BudgetLimitMicros int64      `json:"budget_limit_micros"`
	Limits            TeamLimits `json:"limits"`
	CreatedAt         time.Time  `json:"created_at"`
}

// TeamStore manages teams. A change to a team's limits reaches requests once the key
// cache has expired, the same bound as revoking a key.
type TeamStore struct {
	pool *pgxpool.Pool
}

// NewTeamStore returns a TeamStore.
func NewTeamStore(pool *pgxpool.Pool) *TeamStore { return &TeamStore{pool: pool} }

func (l TeamLimits) valid() bool {
	return l.RequestsPerMinute >= 0 && l.TokensPerMinute >= 0 && l.MaxInFlight >= 0
}

// Create stores a new team.
func (s *TeamStore) Create(ctx context.Context, t Team) (Team, error) {
	if !teamID.MatchString(t.ID) || t.BudgetLimitMicros < 0 || !t.Limits.valid() {
		return Team{}, ErrInvalidTeam
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO teams (id, name, budget_limit_micros, requests_per_minute, tokens_per_minute, max_in_flight)
		VALUES ($1, $1, $2, $3, $4, $5) RETURNING created_at`,
		t.ID, t.BudgetLimitMicros, t.Limits.RequestsPerMinute, t.Limits.TokensPerMinute, t.Limits.MaxInFlight).Scan(&t.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return Team{}, ErrTeamExists
		}
		return Team{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return t, nil
}

const teamColumns = `id, budget_limit_micros, requests_per_minute, tokens_per_minute, max_in_flight, created_at`

func scanTeam(row pgx.Row) (Team, error) {
	var t Team
	err := row.Scan(&t.ID, &t.BudgetLimitMicros, &t.Limits.RequestsPerMinute, &t.Limits.TokensPerMinute, &t.Limits.MaxInFlight, &t.CreatedAt)
	return t, err
}

// Get returns a team.
func (s *TeamStore) Get(ctx context.Context, id string) (Team, error) {
	t, err := scanTeam(s.pool.QueryRow(ctx, `SELECT `+teamColumns+` FROM teams WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, ErrUnknownTeam
	}
	if err != nil {
		return Team{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return t, nil
}

// List returns every team, in ID order.
func (s *TeamStore) List(ctx context.Context) ([]Team, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+teamColumns+` FROM teams ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	teams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Team, error) { return scanTeam(row) })
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return teams, nil
}

// SetLimits replaces a team's limits that aren't about money.
func (s *TeamStore) SetLimits(ctx context.Context, id string, l TeamLimits) error {
	if !l.valid() {
		return ErrInvalidTeam
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE teams SET requests_per_minute = $2, tokens_per_minute = $3, max_in_flight = $4 WHERE id = $1`,
		id, l.RequestsPerMinute, l.TokensPerMinute, l.MaxInFlight)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUnknownTeam
	}
	return nil
}
