package limits

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrLimiterUnavailable means shared limits couldn't be checked.
var ErrLimiterUnavailable = errors.New("limits: the shared limiter is unavailable")

// Why a request was denied. The values are a small fixed set, so they're safe as labels.
const (
	ReasonRequests = "requests"
	ReasonTokens   = "tokens"
	ReasonInFlight = "in_flight"
)

const (
	// releaseTimeout bounds giving back an in-flight slot, on a context of its own,
	// because a request that's over may have a canceled context. If it fails, the slot
	// expires on its own.
	releaseTimeout = 2 * time.Second

	bucketTTLMillis = 120_000

	// checkTimeout bounds one check, so that an outage can't add much to a request.
	checkTimeout = 250 * time.Millisecond

	// OutageRetryInterval is how long a replica goes without trying Redis after a failure.
	// Until then every request gets its answer at once, rather than each waiting out a
	// timeout, and afterwards one request finds out whether Redis has recovered.
	OutageRetryInterval = time.Second
)

var errRecentlyUnreachable = errors.New("redis was unreachable moments ago")

// TeamLimits are a team's limits. Zero means no limit.
type TeamLimits struct {
	RequestsPerMinute int64
	TokensPerMinute   int64
	MaxInFlight       int64
}

func (l TeamLimits) none() bool {
	return l.RequestsPerMinute <= 0 && l.TokensPerMinute <= 0 && l.MaxInFlight <= 0
}

// Decision is the result of asking to admit a request.
type Decision struct {
	Allowed    bool
	Reason     string
	RetryAfter time.Duration
}

// admitScript checks and consumes all of a team's limits atomically, using Redis's clock
// so that replicas can't disagree about the time. A denial consumes nothing. Token buckets
// hold a minute's worth, and refill continuously.
var admitScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
local requests_limit, tokens_limit, max_in_flight = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
local member, hold_ms = ARGV[4], tonumber(ARGV[5])

local function level(key, limit)
  local state = redis.call('HMGET', key, 'level', 'at')
  local value, at = tonumber(state[1]), tonumber(state[2])
  if value == nil then return limit end
  return math.min(limit, value + (now - at) * limit / 60000)
end

local requests_level
if requests_limit > 0 then
  requests_level = level(KEYS[1], requests_limit)
  if requests_level < 1 then
    return {0, 'requests', math.ceil((1 - requests_level) * 60000 / requests_limit)}
  end
end

if tokens_limit > 0 then
  local tokens_level = level(KEYS[2], tokens_limit)
  if tokens_level <= 0 then
    return {0, 'tokens', math.ceil((1 - tokens_level) * 60000 / tokens_limit)}
  end
end

if max_in_flight > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now)
  if redis.call('ZCARD', KEYS[3]) >= max_in_flight then
    return {0, 'in_flight', 1000}
  end
  redis.call('ZADD', KEYS[3], now + hold_ms, member)
  redis.call('PEXPIRE', KEYS[3], hold_ms + 1000)
end

if requests_limit > 0 then
  redis.call('HSET', KEYS[1], 'level', tostring(requests_level - 1), 'at', now)
  redis.call('PEXPIRE', KEYS[1], ARGV[6])
end
return {1, '', 0}
`)

// debitScript spends tokens. The level can go negative, because tokens are only known
// after a request has used them, and a negative level delays the next one.
var debitScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
local limit, spent = tonumber(ARGV[1]), tonumber(ARGV[2])
local state = redis.call('HMGET', KEYS[1], 'level', 'at')
local value, at = tonumber(state[1]), tonumber(state[2])
local current = limit
if value ~= nil then current = math.min(limit, value + (now - at) * limit / 60000) end
local level = current - spent
redis.call('HSET', KEYS[1], 'level', tostring(level), 'at', now)
-- Debt has to outlast its repayment, or expiring would forgive it.
local ttl = tonumber(ARGV[3])
if level < 0 then ttl = math.max(ttl, math.ceil(-level * 60000 / limit) + ttl) end
redis.call('PEXPIRE', KEYS[1], ttl)
return 1
`)

// TeamLimiterOptions configures a TeamLimiter.
type TeamLimiterOptions struct {
	Client *redis.Client
	// Prefix namespaces this limiter's keys.
	Prefix string
	// FailOpen admits requests during a Redis outage, within a per-process limit at each
	// team's configured rate. The default is to refuse them. Either way, budgets don't
	// depend on Redis and stay enforced.
	FailOpen bool
	// Now replaces the clock, for tests.
	Now func() time.Time
}

// TeamLimiter enforces limits a team is promised across all replicas.
type TeamLimiter struct {
	client    *redis.Client
	prefix    string
	failOpen  bool
	local     *localLimiter
	failing   atomic.Bool
	now       func() time.Time
	downUntil atomic.Int64 // Unix nanoseconds until Redis is next tried
}

// NewTeamLimiter returns a TeamLimiter.
func NewTeamLimiter(opts TeamLimiterOptions) *TeamLimiter {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &TeamLimiter{client: opts.Client, prefix: opts.Prefix, failOpen: opts.FailOpen, local: newLocalLimiter(), now: now}
}

// run calls Redis, unless a recent failure says not to bother, and bounds how long it can
// take. A failure that was really the caller's context ending doesn't count as an outage.
func (l *TeamLimiter) run(ctx context.Context, call func(context.Context) error) error {
	if l.now().UnixNano() < l.downUntil.Load() {
		return errRecentlyUnreachable
	}
	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	err := call(checkCtx)
	if err != nil && ctx.Err() == nil {
		l.downUntil.Store(l.now().Add(OutageRetryInterval).UnixNano())
	}
	return err
}

// FailingOpen reports whether the most recent check fell back to local limits.
func (l *TeamLimiter) FailingOpen() bool { return l.failing.Load() }

func (l *TeamLimiter) keys(team string) []string {
	// The hash tag keeps a team's keys together, so that the script can run on a cluster.
	tag := "{" + team + "}"
	return []string{l.prefix + "requests:" + tag, l.prefix + "tokens:" + tag, l.prefix + "in_flight:" + tag}
}

// Admit checks a team's limits, and on success holds an in-flight slot until the returned
// function is called, or until hold passes if this replica crashes first. The function is
// safe to call more than once.
func (l *TeamLimiter) Admit(ctx context.Context, team string, cfg TeamLimits, requestID string, hold time.Duration) (Decision, func(), error) {
	if cfg.none() {
		return Decision{Allowed: true}, func() {}, nil
	}
	keys := l.keys(team)
	var result []any
	err := l.run(ctx, func(checkCtx context.Context) error {
		var err error
		result, err = admitScript.Run(checkCtx, l.client, keys,
			cfg.RequestsPerMinute, cfg.TokensPerMinute, cfg.MaxInFlight, requestID, hold.Milliseconds(), bucketTTLMillis).Slice()
		return err
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Decision{}, func() {}, fmt.Errorf("checking limits: %w", ctxErr)
		}
		if l.failOpen {
			l.failing.Store(true)
			d, release := l.local.admit(team, cfg)
			return d, release, nil
		}
		return Decision{}, func() {}, fmt.Errorf("%w: %w", ErrLimiterUnavailable, err)
	}
	l.failing.Store(false)

	allowed, _ := result[0].(int64)
	if allowed == 0 {
		reason, _ := result[1].(string)
		retryMillis, _ := result[2].(int64)
		return Decision{Reason: reason, RetryAfter: time.Duration(retryMillis) * time.Millisecond}, func() {}, nil
	}
	if cfg.MaxInFlight <= 0 {
		return Decision{Allowed: true}, func() {}, nil
	}
	var once sync.Once
	return Decision{Allowed: true}, func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			defer cancel()
			// If this fails, the slot still expires when its hold runs out.
			_ = l.client.ZRem(releaseCtx, keys[2], requestID).Err()
		})
	}, nil
}

// DebitTokens spends tokens against a team's limit once they're known.
func (l *TeamLimiter) DebitTokens(ctx context.Context, team string, cfg TeamLimits, tokens int64) error {
	if cfg.TokensPerMinute <= 0 || tokens <= 0 {
		return nil
	}
	err := l.run(ctx, func(checkCtx context.Context) error {
		return debitScript.Run(checkCtx, l.client, []string{l.keys(team)[1]}, cfg.TokensPerMinute, tokens, bucketTTLMillis).Err()
	})
	if err != nil {
		if l.failOpen {
			l.local.debit(team, cfg, tokens)
			return nil
		}
		return fmt.Errorf("%w: %w", ErrLimiterUnavailable, err)
	}
	return nil
}

// localLimiter is what a failing-open limiter falls back to. It enforces each team's rate
// per process, so the worst case during an outage is that rate times the number of
// replicas, rather than no limit at all.
type localLimiter struct {
	mu    sync.Mutex
	teams map[string]*localTeam
	now   func() time.Time
}

type localTeam struct {
	requests, tokens         float64
	requestsSeen, tokensSeen bool
	at                       time.Time
	inFlight                 int64
}

func newLocalLimiter() *localLimiter {
	return &localLimiter{teams: make(map[string]*localTeam), now: time.Now}
}

func (l *localLimiter) team(name string, cfg TeamLimits) *localTeam {
	t, ok := l.teams[name]
	now := l.now()
	if !ok {
		t = &localTeam{at: now}
		l.teams[name] = t
	}
	elapsed := now.Sub(t.at).Minutes()
	t.at = now
	if cfg.RequestsPerMinute > 0 {
		if !t.requestsSeen {
			t.requests, t.requestsSeen = float64(cfg.RequestsPerMinute), true
		}
		t.requests = min(float64(cfg.RequestsPerMinute), t.requests+elapsed*float64(cfg.RequestsPerMinute))
	}
	if cfg.TokensPerMinute > 0 {
		if !t.tokensSeen {
			t.tokens, t.tokensSeen = float64(cfg.TokensPerMinute), true
		}
		t.tokens = min(float64(cfg.TokensPerMinute), t.tokens+elapsed*float64(cfg.TokensPerMinute))
	}
	return t
}

func (l *localLimiter) admit(name string, cfg TeamLimits) (Decision, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.team(name, cfg)
	if cfg.RequestsPerMinute > 0 && t.requests < 1 {
		return Decision{Reason: ReasonRequests, RetryAfter: time.Second}, func() {}
	}
	if cfg.TokensPerMinute > 0 && t.tokens <= 0 {
		return Decision{Reason: ReasonTokens, RetryAfter: time.Second}, func() {}
	}
	if cfg.MaxInFlight > 0 && t.inFlight >= cfg.MaxInFlight {
		return Decision{Reason: ReasonInFlight, RetryAfter: time.Second}, func() {}
	}
	if cfg.RequestsPerMinute > 0 {
		t.requests--
	}
	if cfg.MaxInFlight <= 0 {
		return Decision{Allowed: true}, func() {}
	}
	t.inFlight++
	var once sync.Once
	return Decision{Allowed: true}, func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			t.inFlight--
		})
	}
}

func (l *localLimiter) debit(name string, cfg TeamLimits, tokens int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.team(name, cfg).tokens -= float64(tokens)
}
