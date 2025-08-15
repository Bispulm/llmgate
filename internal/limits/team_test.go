package limits_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/testenv"
)

func newTeamLimiter(t *testing.T) (*limits.TeamLimiter, *redis.Client, string) {
	t.Helper()
	client, prefix := testenv.Redis(t)
	return limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: prefix}), client, prefix
}

func admit(t *testing.T, l *limits.TeamLimiter, team string, cfg limits.TeamLimits, id string) (limits.Decision, func()) {
	t.Helper()
	d, release, err := l.Admit(context.Background(), team, cfg, id, time.Minute)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return d, release
}

func TestNoLimitsAdmitEverything(t *testing.T) {
	l, _, _ := newTeamLimiter(t)
	for i := range 50 {
		d, release := admit(t, l, "t", limits.TeamLimits{}, fmt.Sprint(i))
		if !d.Allowed {
			t.Fatalf("request %d was denied: %+v", i, d)
		}
		release()
	}
}

// Limits a team is promised have to hold across replicas, so this goes through separate
// clients concurrently.
func TestRequestLimitHoldsAcrossReplicas(t *testing.T) {
	client, prefix := testenv.Redis(t)
	second := redis.NewClient(client.Options())
	defer func() { _ = second.Close() }()
	replicas := []*limits.TeamLimiter{
		limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: prefix}),
		limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: second, Prefix: prefix}),
	}

	const limit = 60
	var allowed, denied atomic.Int64
	var wg sync.WaitGroup
	for i := range 300 {
		wg.Go(func() {
			d, release, err := replicas[i%2].Admit(context.Background(), "t", limits.TeamLimits{RequestsPerMinute: limit}, fmt.Sprint(i), time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			if d.Allowed {
				allowed.Add(1)
			} else {
				denied.Add(1)
			}
		})
	}
	wg.Wait()

	// A small amount refills while the test runs, at one request per second.
	if got := allowed.Load(); got < limit || got > limit+5 {
		t.Fatalf("allowed %d of 300 with a limit of %d", got, limit)
	}
	if allowed.Load()+denied.Load() != 300 {
		t.Fatalf("allowed %d, denied %d", allowed.Load(), denied.Load())
	}
}

func TestADenialSaysWhyAndWhen(t *testing.T) {
	l, _, _ := newTeamLimiter(t)
	cfg := limits.TeamLimits{RequestsPerMinute: 1}
	if d, release := admit(t, l, "t", cfg, "a"); !d.Allowed {
		t.Fatalf("first request denied: %+v", d)
	} else {
		release()
	}
	d, release := admit(t, l, "t", cfg, "b")
	defer release()
	if d.Allowed {
		t.Fatal("over the limit, but allowed")
	}
	if d.Reason != limits.ReasonRequests {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("RetryAfter = %v", d.RetryAfter)
	}
}

func TestTeamsDoNotAffectEachOther(t *testing.T) {
	l, _, _ := newTeamLimiter(t)
	cfg := limits.TeamLimits{RequestsPerMinute: 1, MaxInFlight: 1}
	d, release := admit(t, l, "busy", cfg, "a")
	defer release()
	if !d.Allowed {
		t.Fatalf("denied: %+v", d)
	}
	if d, _ := admit(t, l, "busy", cfg, "b"); d.Allowed {
		t.Fatal("the busy team was allowed past its limit")
	}
	other, releaseOther := admit(t, l, "quiet", cfg, "c")
	defer releaseOther()
	if !other.Allowed {
		t.Fatalf("another team was denied: %+v", other)
	}
}

func TestInFlightLimitHoldsAcrossReplicasAndIsReleased(t *testing.T) {
	client, prefix := testenv.Redis(t)
	first := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: prefix})
	second := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: prefix})
	cfg := limits.TeamLimits{MaxInFlight: 2}

	d1, release1 := admit(t, first, "t", cfg, "a")
	d2, release2 := admit(t, second, "t", cfg, "b")
	defer release2()
	if !d1.Allowed || !d2.Allowed {
		t.Fatalf("decisions = %+v, %+v", d1, d2)
	}
	d3, _ := admit(t, first, "t", cfg, "c")
	if d3.Allowed || d3.Reason != limits.ReasonInFlight {
		t.Fatalf("over the limit: %+v", d3)
	}

	release1()
	release1() // Releasing twice must not free another request's slot.
	d4, release4 := admit(t, second, "t", cfg, "d")
	defer release4()
	if !d4.Allowed {
		t.Fatalf("after a release: %+v", d4)
	}
	d5, _ := admit(t, first, "t", cfg, "e")
	if d5.Allowed {
		t.Fatalf("a double release freed a slot: %+v", d5)
	}
}

// A replica can crash holding slots. They have to come back on their own.
func TestAbandonedInFlightSlotsExpire(t *testing.T) {
	l, _, _ := newTeamLimiter(t)
	cfg := limits.TeamLimits{MaxInFlight: 1}
	d, _, err := l.Admit(context.Background(), "t", cfg, "crashed", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Fatalf("denied: %+v", d)
	}
	if d, _ := admit(t, l, "t", cfg, "next"); d.Allowed {
		t.Fatal("the slot was free while still held")
	}
	time.Sleep(100 * time.Millisecond)
	if d, _ := admit(t, l, "t", cfg, "later"); !d.Allowed {
		t.Fatalf("an abandoned slot never expired: %+v", d)
	}
}

func TestADenialConsumesNothing(t *testing.T) {
	l, _, _ := newTeamLimiter(t)
	// Denied for in-flight, so its request token must not be spent.
	cfg := limits.TeamLimits{RequestsPerMinute: 2, MaxInFlight: 1}
	first, release := admit(t, l, "t", cfg, "a")
	if !first.Allowed {
		t.Fatalf("denied: %+v", first)
	}
	for range 5 {
		if d, _ := admit(t, l, "t", cfg, "b"); d.Allowed || d.Reason != limits.ReasonInFlight {
			t.Fatalf("decision = %+v", d)
		}
	}
	release()
	d, releaseNext := admit(t, l, "t", cfg, "c")
	defer releaseNext()
	if !d.Allowed {
		t.Fatalf("the denials used up the request limit: %+v", d)
	}
}

func TestTokenLimitAppliesAfterSpending(t *testing.T) {
	l, _, _ := newTeamLimiter(t)
	cfg := limits.TeamLimits{TokensPerMinute: 1000}
	ctx := context.Background()

	d, release := admit(t, l, "t", cfg, "a")
	release()
	if !d.Allowed {
		t.Fatalf("denied: %+v", d)
	}
	// Tokens are only known afterwards, so spending can overshoot once.
	if err := l.DebitTokens(ctx, "t", cfg, 5000); err != nil {
		t.Fatal(err)
	}
	d, _ = admit(t, l, "t", cfg, "b")
	if d.Allowed || d.Reason != limits.ReasonTokens {
		t.Fatalf("decision = %+v", d)
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v", d.RetryAfter)
	}
	if err := l.DebitTokens(ctx, "unlimited", limits.TeamLimits{}, 5000); err != nil {
		t.Fatal(err)
	}
}

func deadClient() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
}

func TestARedisOutageFailsClosedByDefault(t *testing.T) {
	client := deadClient()
	defer func() { _ = client.Close() }()
	l := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: "x:"})

	_, _, err := l.Admit(context.Background(), "t", limits.TeamLimits{RequestsPerMinute: 10}, "a", time.Minute)
	if !errors.Is(err, limits.ErrLimiterUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

// A dead dependency must not slow every request. Without a bound, each check waited on
// the client's own dial retries, adding hundreds of milliseconds to the request path.
func TestAnOutageAddsLittleLatency(t *testing.T) {
	for name, failOpen := range map[string]bool{"failing closed": false, "failing open": true} {
		t.Run(name, func(t *testing.T) {
			client := deadClient()
			defer func() { _ = client.Close() }()
			l := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: "x:", FailOpen: failOpen})
			cfg := limits.TeamLimits{RequestsPerMinute: 1000}

			// The first check discovers the outage, within a bound.
			start := time.Now()
			_, release, _ := l.Admit(context.Background(), "t", cfg, "first", time.Minute)
			release()
			if got := time.Since(start); got > time.Second {
				t.Fatalf("the first check took %v", got)
			}

			// Later ones know already, and don't wait at all.
			start = time.Now()
			for i := range 100 {
				_, release, _ := l.Admit(context.Background(), "t", cfg, fmt.Sprint(i), time.Minute)
				release()
			}
			if got := time.Since(start); got > 100*time.Millisecond {
				t.Fatalf("100 checks during an outage took %v", got)
			}
		})
	}
}

func TestRecoveryIsNoticedAfterAnOutage(t *testing.T) {
	real, prefix := testenv.Redis(t)
	var down atomic.Bool
	down.Store(true)
	var dialer net.Dialer
	opts := *real.Options()
	opts.MaxRetries = -1
	opts.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if down.Load() {
			return nil, errors.New("redis is down")
		}
		return dialer.DialContext(ctx, network, addr)
	}
	client := redis.NewClient(&opts)
	defer func() { _ = client.Close() }()

	now := time.Unix(1_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	l := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: prefix, Now: clock})
	cfg := limits.TeamLimits{RequestsPerMinute: 1000}

	if _, _, err := l.Admit(context.Background(), "t", cfg, "a", time.Minute); !errors.Is(err, limits.ErrLimiterUnavailable) {
		t.Fatalf("error = %v", err)
	}

	// It's back, but this replica hasn't looked yet.
	down.Store(false)
	if _, _, err := l.Admit(context.Background(), "t", cfg, "b", time.Minute); !errors.Is(err, limits.ErrLimiterUnavailable) {
		t.Fatalf("error = %v", err)
	}

	// Once the retry interval passes, one request finds out.
	mu.Lock()
	now = now.Add(limits.OutageRetryInterval)
	mu.Unlock()
	d, release, err := l.Admit(context.Background(), "t", cfg, "c", time.Minute)
	if err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	defer release()
	if !d.Allowed {
		t.Fatalf("decision = %+v", d)
	}
}

func TestATeamWithNoLimitsDoesNotNeedRedis(t *testing.T) {
	client := deadClient()
	defer func() { _ = client.Close() }()
	l := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: "x:"})
	d, release, err := l.Admit(context.Background(), "t", limits.TeamLimits{}, "a", time.Minute)
	if err != nil || !d.Allowed {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
	release()
}

func TestAnOutageCanFailOpenWithinABound(t *testing.T) {
	client := deadClient()
	defer func() { _ = client.Close() }()
	l := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: "x:", FailOpen: true})

	t.Run("the request rate stays bounded in this process", func(t *testing.T) {
		cfg := limits.TeamLimits{RequestsPerMinute: 10}
		allowed := 0
		for i := range 100 {
			d, release, err := l.Admit(context.Background(), "rate", cfg, fmt.Sprint(i), time.Minute)
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			release()
			if d.Allowed {
				allowed++
			}
		}
		if allowed < 10 || allowed > 12 {
			t.Fatalf("allowed %d of 100 with a limit of 10", allowed)
		}
		if !l.FailingOpen() {
			t.Fatal("the limiter doesn't report that it's failing open")
		}
	})
	t.Run("in-flight stays bounded in this process", func(t *testing.T) {
		cfg := limits.TeamLimits{MaxInFlight: 2}
		first, release1, err := l.Admit(context.Background(), "flight", cfg, "a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		second, release2, err := l.Admit(context.Background(), "flight", cfg, "b", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer release2()
		third, _, err := l.Admit(context.Background(), "flight", cfg, "c", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !first.Allowed || !second.Allowed || third.Allowed {
			t.Fatalf("decisions = %+v %+v %+v", first, second, third)
		}
		release1()
		fourth, release4, err := l.Admit(context.Background(), "flight", cfg, "d", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer release4()
		if !fourth.Allowed {
			t.Fatalf("after a release: %+v", fourth)
		}
	})
	t.Run("token debits don't error", func(t *testing.T) {
		if err := l.DebitTokens(context.Background(), "tokens", limits.TeamLimits{TokensPerMinute: 10}, 100); err != nil {
			t.Fatal(err)
		}
		d, release, err := l.Admit(context.Background(), "tokens", limits.TeamLimits{TokensPerMinute: 10}, "a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if d.Allowed {
			t.Fatal("spending past the token limit during an outage wasn't counted")
		}
	})
}

func TestACanceledContextIsNotAnOutage(t *testing.T) {
	l, _, _ := newTeamLimiter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := l.Admit(ctx, "t", limits.TeamLimits{RequestsPerMinute: 10}, "a", time.Minute)
	if err == nil {
		t.Fatal("a canceled request was admitted")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

// Token debt has to last until it's repaid. If its key expired first, a team that
// overspent would start again with a full minute's allowance.
func TestTokenDebtOutlastsItsKeyExpiry(t *testing.T) {
	l, client, prefix := newTeamLimiter(t)
	cfg := limits.TeamLimits{TokensPerMinute: 1000}
	ctx := context.Background()
	if err := l.DebitTokens(ctx, "t", cfg, 10_000); err != nil {
		t.Fatal(err)
	}
	// 9,000 tokens of debt at 1,000 a minute takes nine minutes to repay.
	ttl, err := client.PTTL(ctx, prefix+"tokens:{t}").Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl < 9*time.Minute {
		t.Fatalf("the debt expires after %v", ttl)
	}
	d, _ := admit(t, l, "t", cfg, "after")
	if d.Allowed || d.RetryAfter < 8*time.Minute {
		t.Fatalf("decision = %+v", d)
	}
}
