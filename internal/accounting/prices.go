package accounting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrPriceListChanged means a version label already exists with different contents.
// Price lists are immutable, because old charges must stay reproducible.
var ErrPriceListChanged = errors.New("accounting: a price list with this label has different contents")

// PriceEntry is one model's prices.
type PriceEntry struct {
	Provider string
	Model    string
	Prices   Prices
}

// PriceList is a labeled set of prices, as written in configuration.
type PriceList struct {
	Label   string
	Entries []PriceEntry
}

func (l PriceList) contentHash() string {
	lines := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		lines[i] = fmt.Sprintf("%q %q %d %d %d %d", e.Provider, e.Model, e.Prices.Input, e.Prices.CachedInput, e.Prices.CacheWrite, e.Prices.Output)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// PriceBook is a stored price list, used to price new attempts. It's immutable.
type PriceBook struct {
	versionID int64
	entries   map[[2]string]Prices
}

// VersionID returns the ID pinned on attempts priced from this book.
func (b *PriceBook) VersionID() int64 { return b.versionID }

// Lookup returns a model's prices.
func (b *PriceBook) Lookup(providerName, model string) (Prices, bool) {
	p, ok := b.entries[[2]string{providerName, model}]
	return p, ok
}

// EnsurePrices stores a price list if it's new, and returns its book. Replicas starting
// together agree on one version. A label that exists with different contents is an
// error, because silently using either list would be wrong.
func EnsurePrices(ctx context.Context, pool *pgxpool.Pool, list PriceList) (*PriceBook, error) {
	hash := list.contentHash()
	book := &PriceBook{entries: make(map[[2]string]Prices, len(list.Entries))}
	for _, e := range list.Entries {
		book.entries[[2]string{e.Provider, e.Model}] = e.Prices
	}

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (label, content_hash) VALUES ($1, $2)
			ON CONFLICT (label) DO NOTHING RETURNING id`, list.Label, hash).Scan(&book.versionID)
		if errors.Is(err, pgx.ErrNoRows) {
			var existing string
			if err := tx.QueryRow(ctx, `SELECT id, content_hash FROM price_versions WHERE label = $1`, list.Label).Scan(&book.versionID, &existing); err != nil {
				return fmt.Errorf("reading the price list: %w", err)
			}
			if existing != hash {
				return ErrPriceListChanged
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("storing the price list: %w", err)
		}
		for _, e := range list.Entries {
			if _, err := tx.Exec(ctx, `
				INSERT INTO prices (price_version_id, provider, model, input_pico, cached_input_pico, cache_write_pico, output_pico)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				book.versionID, e.Provider, e.Model, e.Prices.Input, e.Prices.CachedInput, e.Prices.CacheWrite, e.Prices.Output); err != nil {
				return fmt.Errorf("storing a price: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrPriceListChanged) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return book, nil
}
