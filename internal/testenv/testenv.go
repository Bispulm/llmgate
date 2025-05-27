// Package testenv connects integration tests to real Postgres and Redis.
//
// Tests skip when the servers aren't configured, so that `go test ./...` needs nothing
// but Go. Setting LLMGATE_REQUIRE_INTEGRATION=1 turns those skips into failures, which
// CI does, so that a misconfiguration can't silently stop proving database behavior.
package testenv

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Bispulm/llmgate/internal/store"
)

// Environment variables naming the servers.
const (
	PostgresURLEnv = "LLMGATE_TEST_POSTGRES_URL"
	RedisURLEnv    = "LLMGATE_TEST_REDIS_URL"
	RequireEnv     = "LLMGATE_REQUIRE_INTEGRATION"
)

func skipOrFail(t *testing.T, variable string) {
	t.Helper()
	if os.Getenv(RequireEnv) != "" {
		t.Fatalf("%s is not set, but %s requires integration tests to run", variable, RequireEnv)
	}
	t.Skipf("%s is not set; run `make test-integration` to include this test", variable)
}

// PostgresURL returns the URL of a fresh schema with migrations applied, which is
// dropped when the test ends. Each schema is isolated, so tests can run in parallel
// and connect from several pools, as separate gateway replicas do.
func PostgresURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv(PostgresURLEnv)
	if base == "" {
		skipOrFail(t, PostgresURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := store.Open(ctx, store.PoolConfig{URL: base, MaxConns: 2})
	if err != nil {
		t.Fatalf("connecting to Postgres: %v", err)
	}
	defer admin.Close()

	schema := "test_" + strings.ToLower(rand.Text())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("creating a schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pool, err := store.Open(cleanupCtx, store.PoolConfig{URL: base, MaxConns: 1})
		if err != nil {
			t.Errorf("connecting to drop a schema: %v", err)
			return
		}
		defer pool.Close()
		if _, err := pool.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("dropping a schema: %v", err)
		}
	})

	schemaURL, err := withSearchPath(base, schema)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, store.PoolConfig{URL: schemaURL, MaxConns: 2})
	if err != nil {
		t.Fatalf("connecting to the schema: %v", err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return schemaURL
}

func withSearchPath(base, schema string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parsing the test database URL: %w", err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// Postgres returns a pool on a fresh schema, closed when the test ends.
func Postgres(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	return PoolFor(t, PostgresURL(t), maxConns)
}

// PoolFor returns a pool for a schema URL, closed when the test ends. Separate pools
// on one URL behave like separate gateway replicas.
func PoolFor(t *testing.T, schemaURL string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Open(context.Background(), store.PoolConfig{URL: schemaURL, MaxConns: maxConns})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Redis returns a client whose keys are isolated by a unique prefix, also returned,
// with those keys removed when the test ends.
func Redis(t *testing.T) (*redis.Client, string) {
	t.Helper()
	raw := os.Getenv(RedisURLEnv)
	if raw == "" {
		skipOrFail(t, RedisURLEnv)
	}
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("parsing %s: %v", RedisURLEnv, err)
	}
	client := redis.NewClient(opts)
	prefix := "test:" + strings.ToLower(rand.Text()) + ":"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		iter := client.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			client.Del(ctx, iter.Val())
		}
		_ = client.Close()
	})
	return client, prefix
}
