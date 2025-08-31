package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/admin"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/cache"
	"github.com/Bispulm/llmgate/internal/config"
	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/metrics"
	"github.com/Bispulm/llmgate/internal/routing"
	"github.com/Bispulm/llmgate/internal/store"
)

const reconcileBatch = 200

// tenantRuntime is everything tenant mode adds: Postgres for money and keys, Redis for
// team limits, the admin API, and the reconciler.
type tenantRuntime struct {
	cfg   *config.Config
	pool  *pgxpool.Pool
	redis *redis.Client
	// cacheRedis is the cache's own Redis, when it has one.
	cacheRedis *redis.Client
	cache      *cache.Cache
	keys       *auth.KeyStore
	ledger     *accounting.Ledger
	limiter    *limits.TeamLimiter
	admin      http.Handler
}

func startTenant(ctx context.Context, cfg *config.Config, secrets *config.Secrets, routes *routing.Table, logger *slog.Logger) (*tenantRuntime, error) {
	pool, err := store.Open(ctx, store.PoolConfig{URL: secrets.DatabaseURL, MaxConns: cfg.Database.MaxConns})
	if err != nil {
		return nil, err
	}
	t := &tenantRuntime{cfg: cfg, pool: pool}
	if err := t.start(ctx, secrets, routes, logger); err != nil {
		t.close()
		return nil, err
	}
	return t, nil
}

func (t *tenantRuntime) start(ctx context.Context, secrets *config.Secrets, routes *routing.Table, logger *slog.Logger) error {
	if err := store.CheckCurrent(ctx, t.pool); err != nil {
		return err
	}
	list, err := t.cfg.PriceList()
	if err != nil {
		return err
	}
	book, err := accounting.EnsurePrices(ctx, t.pool, list)
	if err != nil {
		return fmt.Errorf("storing price list %q: %w", list.Label, err)
	}
	if t.redis, err = newRedis(secrets.RedisURL); err != nil {
		return err
	}
	cacheClient := t.redis
	if secrets.CacheRedisURL != secrets.RedisURL {
		if t.cacheRedis, err = newRedis(secrets.CacheRedisURL); err != nil {
			return err
		}
		cacheClient = t.cacheRedis
	}
	t.cache = cache.New(cache.Options{Client: cacheClient, Prefix: t.cfg.Redis.KeyPrefix, MaxEntryBytes: t.cfg.Cache.MaxEntryBytes})

	rejected := accounting.RejectedChargeNone
	if t.cfg.Accounting.RejectedAttemptCharge == config.RejectedChargeConservative {
		rejected = accounting.RejectedChargeConservative
	}
	t.ledger = accounting.NewLedger(accounting.LedgerOptions{
		Pool:                  t.pool,
		Prices:                book,
		UncertainGrace:        t.cfg.Accounting.UncertainGrace,
		RejectedAttemptCharge: rejected,
	})
	t.keys = auth.NewKeyStore(auth.KeyStoreOptions{Pool: t.pool, CacheTTL: t.cfg.Auth.KeyCacheTTL})
	t.limiter = limits.NewTeamLimiter(limits.TeamLimiterOptions{
		Client:   t.redis,
		Prefix:   t.cfg.Redis.KeyPrefix,
		FailOpen: t.cfg.RateLimits.OnRedisFailure == config.RedisFailureOpen,
	})
	// The admin API shares the key store, so revoking a key takes effect on this replica
	// at once, and on others within the key cache's TTL.
	t.admin = admin.New(admin.Options{
		Token:  secrets.AdminToken,
		Logger: logger,
		Teams:  auth.NewTeamStore(t.pool),
		Keys:   t.keys,
		Ledger: t.ledger,
		Routes: routes,
	})
	return nil
}

func (t *tenantRuntime) runReconciler(ctx context.Context, logger *slog.Logger, m *metrics.Metrics) {
	t.ledger.RunReconciler(ctx, t.cfg.Accounting.ReconcileInterval, reconcileBatch, func(result accounting.ReconcileResult, err error) {
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("a reconciliation pass failed", "error", err.Error())
				m.DependencyError("postgres", "reconcile")
			}
			return
		}
		m.Reconciled(result)
		logger.Info("reconciled attempts", "released", result.Released, "marked_uncertain", result.MarkedUncertain,
			"charged_conservatively", result.ChargedConservatively)
	})
}

func newRedis(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		// The parse error can contain the URL, and so its password.
		return nil, errors.New("parsing a Redis URL: the URL is malformed")
	}
	return redis.NewClient(opts), nil
}

func (t *tenantRuntime) close() {
	for _, c := range []*redis.Client{t.redis, t.cacheRedis} {
		if c != nil {
			_ = c.Close()
		}
	}
	t.pool.Close()
}

func migrateCommand(p process) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations, which the gateway never applies by itself",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if !cfg.Tenant() {
				return errors.New("development mode has no database to migrate")
			}
			url := p.getenv(cfg.Database.URLEnv)
			if url == "" {
				return fmt.Errorf("environment variable %s is not set", cfg.Database.URLEnv)
			}
			pool, err := store.Open(cmd.Context(), store.PoolConfig{URL: url, MaxConns: 2})
			if err != nil {
				return err
			}
			defer pool.Close()
			if err := store.Migrate(cmd.Context(), pool); err != nil {
				return err
			}
			_, err = fmt.Fprintln(p.stdout, "migrations applied")
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the configuration file")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
