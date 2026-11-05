package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/testenv"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

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

const cacheTTL = 30 * time.Second

func newStore(t *testing.T) (*auth.KeyStore, *pgxpool.Pool, *clock) {
	t.Helper()
	pool := testenv.Postgres(t, 4)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO teams (id, name, budget_limit_micros, requests_per_minute, tokens_per_minute, max_in_flight)
		VALUES ('team-a', 'a', 1000, 60, 6000, 5), ('team-b', 'b', 1000, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	c := &clock{now: time.Unix(1_000_000, 0)}
	return auth.NewKeyStore(auth.KeyStoreOptions{Pool: pool, CacheTTL: cacheTTL, Now: c.Now}), pool, c
}

func TestGeneratedKeysAreUniqueAndHardToGuess(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		key, err := auth.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[key.Raw] || seen[key.ID] {
			t.Fatalf("a repeated key: %s", key.ID)
		}
		seen[key.Raw], seen[key.ID] = true, true
		if !strings.HasPrefix(key.Raw, "lgk_") {
			t.Fatalf("key = %q", key.Raw)
		}
		if !strings.Contains(key.Raw, key.ID) {
			t.Fatalf("the raw key %q doesn't contain its lookup ID %q", key.Raw, key.ID)
		}
		// 256 bits of secret, however it's encoded.
		if secret := strings.TrimPrefix(key.Raw, key.ID+"_"); len(secret) < 43 {
			t.Fatalf("secret %q is too short", secret)
		}
		if strings.Contains(string(key.Hash), key.Raw) {
			t.Fatal("the hash contains the key")
		}
	}
}

func TestAKeyAuthenticatesToItsTeam(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()
	created, err := store.Create(ctx, "team-a", "production")
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Authenticate(ctx, created.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.TeamID != "team-a" || got.KeyID != created.ID {
		t.Fatalf("principal = %+v", got)
	}
	if got.Limits.RequestsPerMinute != 60 || got.Limits.TokensPerMinute != 6000 || got.Limits.MaxInFlight != 5 {
		t.Fatalf("limits = %+v", got.Limits)
	}
}

func TestOnlyAHashIsStored(t *testing.T) {
	store, pool, _ := newStore(t)
	ctx := context.Background()
	created, err := store.Create(ctx, "team-a", "k")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(created.Raw, created.ID+"_")

	var all string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(k)::text FROM api_keys k WHERE id = $1`, created.ID).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all, secret) || strings.Contains(all, created.Raw) {
		t.Fatalf("the stored row holds the secret: %s", all)
	}
}

func TestBadCredentialsAreRejected(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()
	created, err := store.Create(ctx, "team-a", "k")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(created.Raw, created.ID+"_")
	wrong := created.ID + "_" + strings.Repeat("A", len(secret))
	other, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{
		"empty":                       "",
		"no prefix":                   "abc",
		"just the prefix":             "lgk_",
		"no secret":                   created.ID,
		"an empty secret":             created.ID + "_",
		"the wrong secret":            wrong,
		"a key that was never stored": other.Raw,
		"another key's secret":        other.ID + "_" + secret,
		"trailing text":               created.Raw + "x",
		"too long":                    "lgk_" + strings.Repeat("a", 1<<16),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Authenticate(ctx, token); !errors.Is(err, auth.ErrInvalidCredential) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	t.Run("the right secret still works after failures", func(t *testing.T) {
		if _, err := store.Authenticate(ctx, created.Raw); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTheWrongSecretIsRejectedEvenWhenTheKeyIsCached(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()
	created, err := store.Create(ctx, "team-a", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, created.Raw); err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(created.Raw, created.ID+"_")
	if _, err := store.Authenticate(ctx, created.ID+"_"+strings.Repeat("A", len(secret))); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("error = %v", err)
	}
}

// This is the documented revocation bound: a revoked key works for at most the cache TTL.
func TestRevocationTakesEffectWithinTheCacheTTL(t *testing.T) {
	store, pool, c := newStore(t)
	ctx := context.Background()
	created, err := store.Create(ctx, "team-a", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, created.Raw); err != nil {
		t.Fatal(err)
	}

	// Another replica revokes it, so this replica's cache knows nothing about it.
	other := auth.NewKeyStore(auth.KeyStoreOptions{Pool: pool, CacheTTL: cacheTTL, Now: c.Now})
	if err := other.Revoke(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	c.Advance(cacheTTL - time.Second)
	if _, err := store.Authenticate(ctx, created.Raw); err != nil {
		t.Fatalf("within the bound, the cached key may still work: %v", err)
	}
	c.Advance(time.Second)
	if _, err := store.Authenticate(ctx, created.Raw); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("at the bound: error = %v", err)
	}
	// The replica that revoked it stops at once.
	if _, err := other.Authenticate(ctx, created.Raw); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("on the revoking replica: error = %v", err)
	}
}

func TestRevokingIsIdempotentAndUnknownKeysAreReported(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()
	created, err := store.Create(ctx, "team-a", "k")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.Revoke(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Revoke(ctx, "lgk_nope"); !errors.Is(err, auth.ErrUnknownKey) {
		t.Fatalf("error = %v", err)
	}
}

func TestCreatingAKeyForAnUnknownTeamFails(t *testing.T) {
	store, _, _ := newStore(t)
	if _, err := store.Create(context.Background(), "no-such-team", "k"); !errors.Is(err, auth.ErrUnknownTeam) {
		t.Fatalf("error = %v", err)
	}
}

func TestListingKeysHoldsNoSecrets(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()
	first, err := store.Create(ctx, "team-a", "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "team-b", "other team"); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "team-a", "two"); err != nil {
		t.Fatal(err)
	}

	got, err := store.List(ctx, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("keys = %+v: only this team's keys belong here", got)
	}
	for _, k := range got {
		if k.TeamID != "team-a" {
			t.Fatalf("another team's key: %+v", k)
		}
	}
	revoked := 0
	for _, k := range got {
		if k.RevokedAt != nil {
			revoked++
		}
	}
	if revoked != 1 {
		t.Fatalf("keys = %+v", got)
	}
}

func TestTheCacheAvoidsTheDatabaseButFailsClosedWhenItExpires(t *testing.T) {
	url := testenv.PostgresURL(t)
	pool := testenv.PoolFor(t, url, 2)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO teams (id, name, budget_limit_micros) VALUES ('t', 'n', 1)`); err != nil {
		t.Fatal(err)
	}
	c := &clock{now: time.Unix(1_000_000, 0)}
	writer := auth.NewKeyStore(auth.KeyStoreOptions{Pool: pool, CacheTTL: cacheTTL, Now: c.Now})
	created, err := writer.Create(ctx, "t", "k")
	if err != nil {
		t.Fatal(err)
	}

	down := testenv.PoolFor(t, url, 1)
	store := auth.NewKeyStore(auth.KeyStoreOptions{Pool: down, CacheTTL: cacheTTL, Now: c.Now})
	if _, err := store.Authenticate(ctx, created.Raw); err != nil {
		t.Fatal(err)
	}
	down.Close()

	// Within the TTL, a known key doesn't need the database.
	if _, err := store.Authenticate(ctx, created.Raw); err != nil {
		t.Fatalf("a cached key failed during an outage: %v", err)
	}

	// Past it, a key can't be checked. Trusting it would break the revocation bound.
	c.Advance(cacheTTL)
	if _, err := store.Authenticate(ctx, created.Raw); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("error = %v", err)
	}
	// And an unknown key isn't reported as invalid, since that can't be known either.
	other, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, other.Raw); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestUnknownKeysDoNotEachReachTheDatabase(t *testing.T) {
	store, pool, c := newStore(t)
	ctx := context.Background()
	unknown, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, unknown.Raw); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("error = %v", err)
	}
	before := pool.Stat().AcquireCount()
	for range 50 {
		if _, err := store.Authenticate(ctx, unknown.Raw); !errors.Is(err, auth.ErrInvalidCredential) {
			t.Fatalf("error = %v", err)
		}
	}
	if got := pool.Stat().AcquireCount() - before; got != 0 {
		t.Fatalf("repeated failures made %d database calls", got)
	}

	// A remembered miss expires, so a key created later isn't locked out for long.
	c.Advance(cacheTTL)
	if _, err := store.Authenticate(ctx, unknown.Raw); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("error = %v", err)
	}
	if got := pool.Stat().AcquireCount() - before; got == 0 {
		t.Fatal("a remembered miss never expired")
	}
}

func TestTheCacheIsBounded(t *testing.T) {
	url := testenv.PostgresURL(t)
	pool := testenv.PoolFor(t, url, 2)
	c := &clock{now: time.Unix(1_000_000, 0)}
	store := auth.NewKeyStore(auth.KeyStoreOptions{Pool: pool, CacheTTL: cacheTTL, Now: c.Now, MaxCacheEntries: 10})
	ctx := context.Background()
	for range 200 {
		unknown, err := auth.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Authenticate(ctx, unknown.Raw); !errors.Is(err, auth.ErrInvalidCredential) {
			t.Fatalf("error = %v", err)
		}
		if got := store.CacheSize(); got > 10 {
			t.Fatalf("cache size = %d", got)
		}
	}
}

func TestErrorsDoNotCarryTheKey(t *testing.T) {
	store, _, _ := newStore(t)
	created, err := store.Create(context.Background(), "team-a", "k")
	if err != nil {
		t.Fatal(err)
	}
	wrong := created.Raw + "x"
	_, err = store.Authenticate(context.Background(), wrong)
	if err == nil || strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), created.Raw) {
		t.Fatalf("error = %v", err)
	}
}

func TestConcurrentAuthenticationIsSafe(t *testing.T) {
	store, _, c := newStore(t)
	ctx := context.Background()
	created, err := store.Create(ctx, "team-a", "k")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 50 {
				if _, err := store.Authenticate(ctx, created.Raw); err != nil {
					t.Error(err)
				}
				c.Advance(time.Second)
			}
		})
	}
	wg.Wait()
}

// Anyone can send well-formed keys that don't exist. Remembering those mustn't push out
// real keys, or a flood of made-up ones would send every request to the database.
func TestMadeUpKeysDontEvictRealOnes(t *testing.T) {
	pool := testenv.Postgres(t, 4)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO teams (id, name, budget_limit_micros) VALUES ('team-a', 'a', 1000)`); err != nil {
		t.Fatal(err)
	}
	store := auth.NewKeyStore(auth.KeyStoreOptions{Pool: pool, CacheTTL: time.Hour, MaxCacheEntries: 4})
	key, err := store.Create(ctx, "team-a", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, key.Raw); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		fake, err := auth.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Authenticate(ctx, fake.Raw); !errors.Is(err, auth.ErrInvalidCredential) {
			t.Fatalf("err = %v", err)
		}
	}
	// With the database gone, only a cached key still works.
	pool.Close()
	if _, err := store.Authenticate(ctx, key.Raw); err != nil {
		t.Fatalf("a real key was pushed out of the cache: %v", err)
	}
}
