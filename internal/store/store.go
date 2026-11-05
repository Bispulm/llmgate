// Package store connects to Postgres, which is the authority for money and budget
// liability, and manages its schema.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

// connectTimeout bounds establishing a connection, so that an unreachable database
// fails an admission quickly rather than holding the request.
const connectTimeout = 3 * time.Second

// PoolConfig configures a connection pool.
type PoolConfig struct {
	URL      string
	MaxConns int32
}

// Open returns a connection pool. It connects lazily, so a database that's down at
// startup is reported by the first query rather than preventing startup.
func Open(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing the database URL: %w", redactParseError(err))
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	poolCfg.ConnConfig.ConnectTimeout = connectTimeout
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating the connection pool: %w", err)
	}
	return pool, nil
}

// redactParseError drops the detail of a parse failure, because pgx can include the
// connection string, and so the password, in it.
func redactParseError(error) error {
	return fmt.Errorf("the URL is malformed")
}

// ErrSchemaOutdated means migrations haven't been applied. The gateway doesn't apply them
// itself, so that a schema change is a deliberate step: run llmgate migrate.
var ErrSchemaOutdated = errors.New("store: the database schema is not current; run llmgate migrate")

// CheckCurrent reports whether every migration has been applied.
func CheckCurrent(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	provider, err := newProvider(db)
	if err != nil {
		return err
	}
	pending, err := provider.HasPending(ctx)
	if err != nil {
		return fmt.Errorf("checking the schema version: %w", err)
	}
	if pending {
		return ErrSchemaOutdated
	}
	return nil
}

// Migrate brings the schema up to date. Goose takes its own lock, so replicas starting
// together don't conflict.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	return migrate(ctx, db)
}

func newProvider(db *sql.DB) (*goose.Provider, error) {
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading migrations: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("preparing the migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("preparing migrations: %w", err)
	}
	return provider, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	provider, err := newProvider(db)
	if err != nil {
		return err
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}
