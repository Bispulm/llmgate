// Package cache holds exact-match responses in Redis, separately for each team.
//
// It's an optimization and never an authority: a Redis failure is a miss, and nothing
// about money depends on it. A hit happens before any budget reservation, so it costs a
// team nothing.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Bispulm/llmgate/internal/chat"
)

const (
	// DefaultMaxEntryBytes bounds one stored response.
	DefaultMaxEntryBytes = 256 << 10
	// DefaultMaxFlights bounds how many distinct requests can share work at once.
	DefaultMaxFlights = 1024

	opTimeout = 100 * time.Millisecond
	// outageRetryInterval is how long a replica skips the cache after Redis fails.
	outageRetryInterval = time.Second
)

// Key identifies a request's answer. Every field is length-prefixed, so that no two
// different requests share an encoding. Anything that could change the answer is
// included, and the field name a client used for its output limit isn't.
func Key(teamID, alias, revision string, req *chat.Request) string {
	h := sha256.New()
	writeString(h, "llmgate-cache-v1")
	writeString(h, teamID)
	writeString(h, alias)
	writeString(h, revision)
	writeUint(h, uint64(len(req.Messages)))
	for _, m := range req.Messages {
		writeString(h, string(m.Role))
		writeString(h, m.Content)
	}
	writeUint(h, uint64(req.MaxOutputTokens)) //nolint:gosec // Validated as positive.
	writeFloat(h, req.Temperature)
	writeFloat(h, req.TopP)
	writeUint(h, uint64(len(req.Stop)))
	for _, s := range req.Stop {
		writeString(h, s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeUint(h hash.Hash, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, _ = h.Write(b[:])
}

func writeString(h hash.Hash, s string) {
	writeUint(h, uint64(len(s)))
	_, _ = h.Write([]byte(s))
}

// writeFloat keeps "not set" apart from every value, including zero.
func writeFloat(h hash.Hash, f *float64) {
	if f == nil {
		_, _ = h.Write([]byte{0})
		return
	}
	_, _ = h.Write([]byte{1})
	writeUint(h, math.Float64bits(*f))
}

// Entry is a stored response. Its usage is exactly what the original response reported.
// A hit costs nothing, but the counts still describe the text.
type Entry struct {
	Content                string            `json:"content"`
	Refusal                string            `json:"refusal,omitempty"`
	FinishReason           chat.FinishReason `json:"finish_reason"`
	Route                  string            `json:"route"`
	PromptTokens           int64             `json:"prompt_tokens"`
	CompletionTokens       int64             `json:"completion_tokens"`
	TotalTokens            int64             `json:"total_tokens"`
	CachedPromptTokens     *int64            `json:"cached_prompt_tokens,omitempty"`
	CacheWritePromptTokens *int64            `json:"cache_write_prompt_tokens,omitempty"`
	ReasoningTokens        *int64            `json:"reasoning_tokens,omitempty"`
}

// Options configures a Cache.
type Options struct {
	Client *redis.Client
	Prefix string
	// MaxEntryBytes bounds one stored response. Zero uses DefaultMaxEntryBytes.
	MaxEntryBytes int
	// MaxFlights bounds duplicate suppression. Zero uses DefaultMaxFlights.
	MaxFlights int
	// OnFailure hears about a Redis failure, at most once each time the cache stops using
	// Redis for a moment, so that an outage is visible without flooding logs.
	OnFailure func(error)
}

// Cache stores responses. Its size is bounded by each entry's size and TTL, and Redis's
// own memory limit: see docs/admin.md.
type Cache struct {
	client     *redis.Client
	prefix     string
	maxEntry   int
	maxFlights int
	onFailure  func(error)
	downUntil  atomic.Int64

	mu      sync.Mutex
	flights map[string]*flight
}

type flight struct {
	done   chan struct{}
	entry  Entry
	usable bool
}

// New returns a Cache.
func New(opts Options) *Cache {
	c := &Cache{client: opts.Client, prefix: opts.Prefix, maxEntry: opts.MaxEntryBytes, maxFlights: opts.MaxFlights,
		onFailure: opts.OnFailure, flights: make(map[string]*flight)}
	if c.maxEntry <= 0 {
		c.maxEntry = DefaultMaxEntryBytes
	}
	if c.maxFlights <= 0 {
		c.maxFlights = DefaultMaxFlights
	}
	return c
}

// redisKey puts the team in the key's hash tag, so a team's entries can be found, and
// deleted, together.
func (c *Cache) redisKey(teamID, key string) string {
	return c.prefix + "cache:{" + teamID + "}:" + key
}

// run bounds a call, and skips Redis for a moment after it fails, so that an outage
// adds almost nothing to a request.
func (c *Cache) run(ctx context.Context, call func(context.Context) error) error {
	if time.Now().UnixNano() < c.downUntil.Load() {
		return errors.New("redis was unreachable moments ago")
	}
	opCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	err := call(opCtx)
	if err != nil && !errors.Is(err, redis.Nil) && ctx.Err() == nil {
		c.downUntil.Store(time.Now().Add(outageRetryInterval).UnixNano())
		if c.onFailure != nil {
			c.onFailure(err)
		}
	}
	return err
}

// Get returns a stored response. Any failure is a miss.
func (c *Cache) Get(ctx context.Context, teamID, key string) (Entry, bool) {
	var raw []byte
	err := c.run(ctx, func(ctx context.Context) error {
		var err error
		raw, err = c.client.Get(ctx, c.redisKey(teamID, key)).Bytes()
		return err
	})
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	if json.Unmarshal(raw, &e) != nil {
		return Entry{}, false
	}
	return e, true
}

// Set stores a response, unless it's too large. A failure is ignored, because the
// response has already been served.
func (c *Cache) Set(ctx context.Context, teamID, key string, e Entry, ttl time.Duration) {
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > c.maxEntry || ttl <= 0 {
		return
	}
	_ = c.run(ctx, func(ctx context.Context) error {
		return c.client.Set(ctx, c.redisKey(teamID, key), raw, ttl).Err()
	})
}

// Join suppresses duplicate work within this replica. The first caller for a key leads,
// and must call finish with its result. Later callers wait for it: if the leader's result
// was usable they get it with shared true, and otherwise they do their own work. Keys
// include the team, so no team waits on another's request. finish is safe to call more
// than once, and only the first call counts.
func (c *Cache) Join(ctx context.Context, key string) (e Entry, shared bool, finish func(Entry, bool)) {
	c.mu.Lock()
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			if f.usable {
				return f.entry, true, func(Entry, bool) {}
			}
		case <-ctx.Done():
		}
		return Entry{}, false, func(Entry, bool) {}
	}
	if len(c.flights) >= c.maxFlights {
		c.mu.Unlock()
		return Entry{}, false, func(Entry, bool) {}
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	var once sync.Once
	return Entry{}, false, func(result Entry, usable bool) {
		once.Do(func() {
			f.entry, f.usable = result, usable
			c.mu.Lock()
			delete(c.flights, key)
			c.mu.Unlock()
			close(f.done)
		})
	}
}

// Flights returns how many requests are leading shared work.
func (c *Cache) Flights() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.flights)
}
