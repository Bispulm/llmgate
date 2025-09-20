package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/cache"
	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/testenv"
)

func newTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	client, prefix := testenv.Redis(t)
	return cache.New(cache.Options{Client: client, Prefix: prefix})
}

// ask sends a request, opting in to the cache when allow is set.
func ask(t *testing.T, client *http.Client, url, key, body string, allow bool) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if allow {
		req.Header.Set(headerCache, "allow")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

func TestACachedAnswerCostsNothing(t *testing.T) {
	const model = "alpha;tokens=3"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{cache: newTestCache(t), cacheTTL: time.Minute}, model)

	first, firstBody := ask(t, client, url, key, chatBody(false, ""), true)
	if first.StatusCode != http.StatusOK || first.Header.Get(headerCache) != cacheMiss {
		t.Fatalf("first: %d %q: %s", first.StatusCode, first.Header.Get(headerCache), firstBody)
	}
	e.waitForStates("a", map[string]int{accounting.StateReconciled: 1})
	settled := e.period("a").settled

	second, secondBody := ask(t, client, url, key, chatBody(false, ""), true)
	if second.StatusCode != http.StatusOK || second.Header.Get(headerCache) != cacheHit {
		t.Fatalf("second: %d %q: %s", second.StatusCode, second.Header.Get(headerCache), secondBody)
	}
	if !strings.Contains(secondBody, "[alpha]") || second.Header.Get(headerAttempts) != "0" || second.Header.Get("X-Request-Id") == first.Header.Get("X-Request-Id") {
		t.Fatalf("hit = %v: %s", second.Header, secondBody)
	}
	if got := e.mock.Stats()["alpha"].Requests; got != 1 {
		t.Fatalf("the provider saw %d requests", got)
	}
	if got := e.period("a"); got.settled != settled || got.reserved != 0 {
		t.Fatalf("a hit changed the books: %+v, was settled %d", got, settled)
	}
	if got := e.attemptStates("a"); got[accounting.StateReconciled] != 1 || len(got) != 1 {
		t.Fatalf("attempts = %v", got)
	}
}

func TestOnlyEligibleRequestsUseTheCache(t *testing.T) {
	const model = "alpha;tokens=3"
	t.Run("a client that didn't ask", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		url, client := e.instance(instanceOptions{cache: newTestCache(t), cacheTTL: time.Minute}, model)
		for range 2 {
			resp, _ := ask(t, client, url, key, chatBody(false, ""), false)
			if resp.Header.Get(headerCache) != "" {
				t.Fatalf("cache header = %q", resp.Header.Get(headerCache))
			}
		}
		if got := e.mock.Stats()["alpha"].Requests; got != 2 {
			t.Fatalf("the provider saw %d requests", got)
		}
	})
	t.Run("a stream", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		url, client := e.instance(instanceOptions{cache: newTestCache(t), cacheTTL: time.Minute}, model)
		for range 2 {
			resp, _ := ask(t, client, url, key, chatBody(true, ""), true)
			if resp.Header.Get(headerCache) != cacheBypass {
				t.Fatalf("cache header = %q", resp.Header.Get(headerCache))
			}
		}
		if got := e.mock.Stats()["alpha"].Requests; got != 2 {
			t.Fatalf("the provider saw %d requests", got)
		}
	})
	t.Run("an alias without caching", func(t *testing.T) {
		e := newTenantEnv(t, model)
		key := e.addTeam("a", 100_000)
		url, client := e.instance(instanceOptions{cache: newTestCache(t)}, model)
		for range 2 {
			resp, _ := ask(t, client, url, key, chatBody(false, ""), true)
			if resp.Header.Get(headerCache) != cacheBypass {
				t.Fatalf("cache header = %q", resp.Header.Get(headerCache))
			}
		}
		if got := e.mock.Stats()["alpha"].Requests; got != 2 {
			t.Fatalf("the provider saw %d requests", got)
		}
	})
}

func TestTeamsDontShareCachedAnswers(t *testing.T) {
	const model = "alpha;tokens=3"
	e := newTenantEnv(t, model)
	a := e.addTeam("a", 100_000)
	b := e.addTeam("b", 100_000)
	url, client := e.instance(instanceOptions{cache: newTestCache(t), cacheTTL: time.Minute}, model)

	ask(t, client, url, a, chatBody(false, ""), true)
	resp, _ := ask(t, client, url, b, chatBody(false, ""), true)
	if resp.Header.Get(headerCache) != cacheMiss {
		t.Fatalf("team b got %q from team a's answer", resp.Header.Get(headerCache))
	}
	if got := e.mock.Stats()["alpha"].Requests; got != 2 {
		t.Fatalf("the provider saw %d requests", got)
	}
}

// A hit spends nothing, so a team out of budget can still read what it already paid for.
// New work is still refused.
func TestAnExhaustedTeamCanStillReadItsCache(t *testing.T) {
	const model = "alpha;tokens=3"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{cache: newTestCache(t), cacheTTL: time.Minute}, model)
	ask(t, client, url, key, chatBody(false, ""), true)
	e.waitForStates("a", map[string]int{accounting.StateReconciled: 1})
	if err := e.ledger.SetLimit(context.Background(), "a", 0); err != nil {
		t.Fatal(err)
	}

	resp, body := ask(t, client, url, key, chatBody(false, ""), true)
	if resp.StatusCode != http.StatusOK || resp.Header.Get(headerCache) != cacheHit {
		t.Fatalf("status = %d %q: %s", resp.StatusCode, resp.Header.Get(headerCache), body)
	}
	resp, body = ask(t, client, url, key, chatBody(false, `,"max_tokens":9`), true)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, codeBudgetExhausted) {
		t.Fatalf("new work: status = %d: %s", resp.StatusCode, body)
	}
}

// A hit is still a request, so it counts against a request rate limit. It used no tokens.
func TestAHitStillCountsAsARequest(t *testing.T) {
	const model = "alpha;tokens=3"
	e := newTenantEnv(t, model)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO teams (id, name, budget_limit_micros, requests_per_minute) VALUES ('a', 'a', 100000, 2)`); err != nil {
		t.Fatal(err)
	}
	key, err := e.keys.Create(context.Background(), "a", "k")
	if err != nil {
		t.Fatal(err)
	}
	client, prefix := testenv.Redis(t)
	limiter := limits.NewTeamLimiter(limits.TeamLimiterOptions{Client: client, Prefix: prefix})
	url, httpClient := e.instance(instanceOptions{cache: cache.New(cache.Options{Client: client, Prefix: prefix}), cacheTTL: time.Minute, limiter: limiter}, model)

	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if resp, body := ask(t, httpClient, url, key.Raw, chatBody(false, ""), true); resp.StatusCode != want {
			t.Fatalf("request %d: status = %d: %s", i+1, resp.StatusCode, body)
		}
	}
}

func TestOnlyGoodAnswersAreCached(t *testing.T) {
	for name, model := range map[string]string{
		"a failure":       "bad;fail=status;status=500",
		"a refusal":       "refused;finish=content_filter;tokens=1",
		"no usage report": "quiet;usage=none;tokens=1",
	} {
		t.Run(name, func(t *testing.T) {
			e := newTenantEnv(t, model)
			key := e.addTeam("a", 100_000)
			url, client := e.instance(instanceOptions{cache: newTestCache(t), cacheTTL: time.Minute}, model)
			for range 2 {
				resp, _ := ask(t, client, url, key, chatBody(false, ""), true)
				if resp.Header.Get(headerCache) == cacheHit {
					t.Fatal("served from the cache")
				}
			}
			label := strings.SplitN(model, ";", 2)[0]
			if got := e.mock.Stats()[label].Requests; got != 2 {
				t.Fatalf("the provider saw %d requests", got)
			}
		})
	}
}

// Identical requests arriving together make one upstream call, and the others share it.
func TestDuplicateRequestsShareOneUpstreamCall(t *testing.T) {
	const model = "slowish;tokens=3;latency=300ms"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	url, client := e.instance(instanceOptions{cache: newTestCache(t), cacheTTL: time.Minute}, model)

	const n = 10
	results := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			resp, body := ask(t, client, url, key, chatBody(false, ""), true)
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "[slowish]") {
				results <- "bad"
				return
			}
			results <- resp.Header.Get(headerCache)
		})
	}
	wg.Wait()
	close(results)
	counts := map[string]int{}
	for r := range results {
		counts[r]++
	}
	if counts[cacheMiss] != 1 || counts[cacheHit] != n-1 {
		t.Fatalf("results = %v", counts)
	}
	if got := e.mock.Stats()["slowish"].Requests; got != 1 {
		t.Fatalf("the provider saw %d requests", got)
	}
}

func TestACacheOutageIsAMiss(t *testing.T) {
	const model = "alpha;tokens=3"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	defer func() { _ = dead.Close() }()
	url, client := e.instance(instanceOptions{cache: cache.New(cache.Options{Client: dead}), cacheTTL: time.Minute}, model)
	for range 3 {
		start := time.Now()
		resp, body := ask(t, client, url, key, chatBody(false, ""), true)
		if resp.StatusCode != http.StatusOK || resp.Header.Get(headerCache) != cacheMiss {
			t.Fatalf("status = %d %q: %s", resp.StatusCode, resp.Header.Get(headerCache), body)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("a request took %v with the cache down", elapsed)
		}
	}
}
