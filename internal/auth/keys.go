package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	keyPrefix    = "lgk_"
	keyIDBytes   = 8
	secretBytes  = 32
	maxKeyLength = 256

	pgForeignKeyViolation = "23503"

	defaultMaxCacheEntries = 100_000
)

var (
	// ErrUnavailable means a key couldn't be checked. It's not the same as a bad key:
	// the client's credential may be fine, so it must not read as a 401.
	ErrUnavailable = errors.New("auth: the key store is unavailable")
	// ErrUnknownKey means no such key exists.
	ErrUnknownKey = errors.New("auth: unknown key")
	// ErrUnknownTeam means no such team exists.
	ErrUnknownTeam = errors.New("auth: unknown team")
)

// GeneratedKey is a new key. Raw is shown once, and never stored.
type GeneratedKey struct {
	ID   string
	Raw  string
	Hash []byte
}

// GenerateKey creates a key from cryptographic randomness. The secret is 256 bits, so a
// fast hash is the right choice: slow password hashes exist to protect guessable
// passwords, and would only add latency to every request here.
func GenerateKey() (GeneratedKey, error) {
	idBytes := make([]byte, keyIDBytes)
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return GeneratedKey{}, fmt.Errorf("generating a key: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return GeneratedKey{}, fmt.Errorf("generating a key: %w", err)
	}
	id := keyPrefix + hex.EncodeToString(idBytes)
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	return GeneratedKey{ID: id, Raw: id + "_" + encoded, Hash: hashSecret(encoded)}, nil
}

func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// splitKey separates a key into its lookup ID and secret. It rejects anything that
// isn't shaped like a key without touching the database.
func splitKey(token string) (id, secret string, ok bool) {
	if len(token) > maxKeyLength || !strings.HasPrefix(token, keyPrefix) {
		return "", "", false
	}
	idPart, secret, ok := strings.Cut(strings.TrimPrefix(token, keyPrefix), "_")
	if !ok || idPart == "" || secret == "" {
		return "", "", false
	}
	return keyPrefix + idPart, secret, true
}

// KeyStoreOptions configures a KeyStore.
type KeyStoreOptions struct {
	Pool *pgxpool.Pool
	// CacheTTL is the documented bound on revocation: a revoked key can keep working on
	// another replica for at most this long.
	CacheTTL time.Duration
	// Now replaces the clock, for tests.
	Now func() time.Time
	// MaxCacheEntries bounds real keys and unknown IDs, separately. Zero uses a default.
	MaxCacheEntries int
}

type cacheEntry struct {
	expires   time.Time
	known     bool // false remembers that no active key has this ID
	hash      []byte
	principal Principal
}

// KeyStore authenticates virtual keys against Postgres, through a short-lived cache so
// that the database isn't queried on every request.
type KeyStore struct {
	pool       *pgxpool.Pool
	ttl        time.Duration
	now        func() time.Time
	maxEntries int

	mu    sync.RWMutex
	cache map[string]cacheEntry
	// misses remembers IDs with no active key. It's kept apart, because anyone can make
	// up IDs, and filling it must never push out a real key.
	misses map[string]cacheEntry
}

var _ Authenticator = (*KeyStore)(nil)

// NewKeyStore returns a KeyStore.
func NewKeyStore(opts KeyStoreOptions) *KeyStore {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxEntries := opts.MaxCacheEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxCacheEntries
	}
	return &KeyStore{pool: opts.Pool, ttl: opts.CacheTTL, now: now, maxEntries: maxEntries, cache: make(map[string]cacheEntry), misses: make(map[string]cacheEntry)}
}

// Authenticate implements Authenticator. The team comes only from the key that was
// presented, never from anything else in a request.
func (s *KeyStore) Authenticate(ctx context.Context, token string) (Principal, error) {
	id, secret, ok := splitKey(token)
	if !ok {
		return Principal{}, ErrInvalidCredential
	}
	now := s.now()

	s.mu.RLock()
	entry, cached := s.cache[id]
	if !cached {
		entry, cached = s.misses[id]
	}
	s.mu.RUnlock()
	if !cached || !now.Before(entry.expires) {
		var err error
		if entry, err = s.load(ctx, id, now); err != nil {
			return Principal{}, err
		}
	}
	if !entry.known {
		return Principal{}, ErrInvalidCredential
	}
	if subtle.ConstantTimeCompare(hashSecret(secret), entry.hash) != 1 {
		return Principal{}, ErrInvalidCredential
	}
	return entry.principal, nil
}

func (s *KeyStore) load(ctx context.Context, id string, now time.Time) (cacheEntry, error) {
	entry := cacheEntry{expires: now.Add(s.ttl)}
	err := s.pool.QueryRow(ctx, `
		SELECT k.secret_hash, k.team_id, t.requests_per_minute, t.tokens_per_minute, t.max_in_flight
		FROM api_keys k JOIN teams t ON t.id = k.team_id
		WHERE k.id = $1 AND k.revoked_at IS NULL`, id).Scan(
		&entry.hash, &entry.principal.TeamID,
		&entry.principal.Limits.RequestsPerMinute, &entry.principal.Limits.TokensPerMinute, &entry.principal.Limits.MaxInFlight)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		entry.known = false
	case err != nil:
		return entry, fmt.Errorf("%w: %w", ErrUnavailable, err)
	default:
		entry.known = true
		entry.principal.KeyID = id
	}
	s.remember(id, entry)
	return entry, nil
}

func (s *KeyStore) remember(id string, entry cacheEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := &s.cache
	if !entry.known {
		target = &s.misses
		delete(s.cache, id)
	} else {
		delete(s.misses, id)
	}
	if _, exists := (*target)[id]; !exists && len(*target) >= s.maxEntries {
		// Each map has to stay bounded. Dropping one whole map is crude but safe: entries
		// are only an optimization, and reload on demand.
		*target = make(map[string]cacheEntry)
	}
	(*target)[id] = entry
}

// CacheSize returns how many entries the cache holds.
func (s *KeyStore) CacheSize() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.cache) + len(s.misses)
}

// Create stores a new key for a team. The raw key is only in the result, and can't be
// recovered later.
func (s *KeyStore) Create(ctx context.Context, teamID, name string) (GeneratedKey, error) {
	key, err := GenerateKey()
	if err != nil {
		return GeneratedKey{}, err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO api_keys (id, team_id, name, secret_hash, created_at) VALUES ($1, $2, $3, $4, $5)`,
		key.ID, teamID, name, key.Hash, s.now())
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
			return GeneratedKey{}, ErrUnknownTeam
		}
		return GeneratedKey{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return key, nil
}

// Revoke disables a key. It's immediate on this replica, and takes at most the cache TTL
// on others. Revoking twice is harmless.
func (s *KeyStore) Revoke(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1`, id, s.now())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUnknownKey
	}
	s.mu.Lock()
	delete(s.cache, id)
	delete(s.misses, id)
	s.mu.Unlock()
	return nil
}

// KeyInfo describes a key, without anything that could be used as one.
type KeyInfo struct {
	ID        string     `json:"id"`
	TeamID    string     `json:"team_id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// List returns a team's keys.
func (s *KeyStore) List(ctx context.Context, teamID string) ([]KeyInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, team_id, name, created_at, revoked_at FROM api_keys WHERE team_id = $1 ORDER BY created_at, id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (KeyInfo, error) {
		var k KeyInfo
		err := row.Scan(&k.ID, &k.TeamID, &k.Name, &k.CreatedAt, &k.RevokedAt)
		return k, err
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return keys, nil
}
