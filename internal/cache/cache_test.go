package cache_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Bispulm/llmgate/internal/cache"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/testenv"
)

func float(v float64) *float64 { return &v }

func baseRequest() *chat.Request {
	return &chat.Request{
		Model:           "fast",
		Messages:        []chat.Message{{Role: chat.RoleSystem, Content: "be brief"}, {Role: chat.RoleUser, Content: "hello"}},
		MaxOutputTokens: 100,
		MaxOutputParam:  "max_tokens",
		Temperature:     float(0.5),
		Stop:            []string{"END"},
	}
}

// Anything that could change the answer has to change the key, and nothing else may.
func TestTheKeyCoversEverythingThatChangesTheAnswer(t *testing.T) {
	base := cache.Key("team-a", "fast", "rev1", baseRequest())
	if again := cache.Key("team-a", "fast", "rev1", baseRequest()); again != base {
		t.Fatal("the same request gave different keys")
	}
	sameAnswer := baseRequest()
	sameAnswer.MaxOutputParam = "max_completion_tokens"
	if cache.Key("team-a", "fast", "rev1", sameAnswer) != base {
		t.Fatal("the name of the output limit's field changed the key")
	}

	changes := map[string]func() (string, string, string, *chat.Request){
		"team":     func() (string, string, string, *chat.Request) { return "team-b", "fast", "rev1", baseRequest() },
		"alias":    func() (string, string, string, *chat.Request) { return "team-a", "slow", "rev1", baseRequest() },
		"revision": func() (string, string, string, *chat.Request) { return "team-a", "fast", "rev2", baseRequest() },
		"message order": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Messages[0], r.Messages[1] = r.Messages[1], r.Messages[0]
			return "team-a", "fast", "rev1", r
		},
		"a role": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Messages[0].Role = chat.RoleDeveloper
			return "team-a", "fast", "rev1", r
		},
		"trailing whitespace": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Messages[1].Content = "hello "
			return "team-a", "fast", "rev1", r
		},
		"a boundary between messages": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Messages[0].Content, r.Messages[1].Content = "be brie", "fhello"
			return "team-a", "fast", "rev1", r
		},
		"an extra empty message": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Messages = append(r.Messages, chat.Message{Role: chat.RoleUser})
			return "team-a", "fast", "rev1", r
		},
		"output limit": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.MaxOutputTokens = 101
			return "team-a", "fast", "rev1", r
		},
		"temperature": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Temperature = float(0.50000001)
			return "team-a", "fast", "rev1", r
		},
		"no temperature, rather than zero": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Temperature = nil
			return "team-a", "fast", "rev1", r
		},
		"top_p": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.TopP = float(0.5)
			return "team-a", "fast", "rev1", r
		},
		"top_p instead of temperature": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Temperature, r.TopP = nil, float(0.5)
			return "team-a", "fast", "rev1", r
		},
		"stop sequences": func() (string, string, string, *chat.Request) {
			r := baseRequest()
			r.Stop = []string{"EN", "D"}
			return "team-a", "fast", "rev1", r
		},
		"team boundary": func() (string, string, string, *chat.Request) { return "team-af", "ast", "rev1", baseRequest() },
	}
	seen := map[string]string{base: "base"}
	for name, change := range changes {
		team, alias, revision, req := change()
		key := cache.Key(team, alias, revision, req)
		if other, dup := seen[key]; dup {
			t.Errorf("changing %s gave the same key as %s", name, other)
		}
		seen[key] = name
	}
}

func newCache(t *testing.T, opts cache.Options) *cache.Cache {
	t.Helper()
	if opts.Client == nil {
		opts.Client, opts.Prefix = testenv.Redis(t)
	}
	return cache.New(opts)
}

func entry(content string) cache.Entry {
	return cache.Entry{Content: content, FinishReason: chat.FinishStop, Route: "mock/alpha", PromptTokens: 5, CompletionTokens: 2}
}

func TestEntriesRoundTripAndExpire(t *testing.T) {
	client, prefix := testenv.Redis(t)
	c := newCache(t, cache.Options{Client: client, Prefix: prefix})
	ctx := context.Background()
	key := cache.Key("team-a", "fast", "rev1", baseRequest())

	if _, ok := c.Get(ctx, "team-a", key); ok {
		t.Fatal("a hit before anything was stored")
	}
	c.Set(ctx, "team-a", key, entry("hi"), 200*time.Millisecond)
	got, ok := c.Get(ctx, "team-a", key)
	if !ok || got != entry("hi") {
		t.Fatalf("got %+v, %v", got, ok)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := c.Get(ctx, "team-a", key); ok {
		t.Fatal("an entry outlived its TTL")
	}
}

// The team is part of where an entry lives, not only of its key, so even a stolen key
// can't read another team's entry.
func TestATeamOnlyReadsItsOwnEntries(t *testing.T) {
	c := newCache(t, cache.Options{})
	ctx := context.Background()
	key := cache.Key("team-a", "fast", "rev1", baseRequest())
	c.Set(ctx, "team-a", key, entry("for a"), time.Minute)
	if _, ok := c.Get(ctx, "team-b", key); ok {
		t.Fatal("another team read the entry")
	}
}

func TestOversizedEntriesAreNotStored(t *testing.T) {
	c := newCache(t, cache.Options{MaxEntryBytes: 64})
	ctx := context.Background()
	key := cache.Key("team-a", "fast", "rev1", baseRequest())
	c.Set(ctx, "team-a", key, entry(string(make([]byte, 100))), time.Minute)
	if _, ok := c.Get(ctx, "team-a", key); ok {
		t.Fatal("an oversized entry was stored")
	}
}

// A cache is only an optimization, so an outage is a miss, quickly.
func TestARedisOutageIsAQuickMiss(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: time.Second, MaxRetries: 3})
	defer func() { _ = dead.Close() }()
	c := cache.New(cache.Options{Client: dead, Prefix: "x:"})
	ctx := context.Background()
	for range 5 {
		start := time.Now()
		if _, ok := c.Get(ctx, "team-a", "k"); ok {
			t.Fatal("a hit from a dead Redis")
		}
		c.Set(ctx, "team-a", "k", entry("x"), time.Minute)
		if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
			t.Fatalf("a miss took %v", elapsed)
		}
	}
}

func TestDuplicateRequestsWaitForOneLeader(t *testing.T) {
	c := newCache(t, cache.Options{})
	ctx := context.Background()
	const waiters = 20

	_, _, finish := c.Join(ctx, "k")
	var hits atomic.Int64
	var wg sync.WaitGroup
	for range waiters {
		wg.Go(func() {
			got, shared, done := c.Join(ctx, "k")
			defer done(cache.Entry{}, false)
			if shared && got == entry("led") {
				hits.Add(1)
			}
		})
	}
	time.Sleep(50 * time.Millisecond) // Let them queue behind the leader.
	finish(entry("led"), true)
	wg.Wait()
	if hits.Load() != waiters {
		t.Fatalf("%d of %d waiters got the leader's answer", hits.Load(), waiters)
	}
}

// If the leader can't produce something cacheable, each waiter does its own work.
func TestWaitersCarryOnWhenTheLeaderFails(t *testing.T) {
	c := newCache(t, cache.Options{})
	ctx := context.Background()
	_, _, finish := c.Join(ctx, "k")
	result := make(chan bool, 1)
	go func() {
		_, shared, done := c.Join(ctx, "k")
		done(cache.Entry{}, false)
		result <- shared
	}()
	time.Sleep(20 * time.Millisecond)
	finish(cache.Entry{}, false)
	if <-result {
		t.Fatal("a waiter was given a failed leader's answer")
	}
	// The flight is over, so the next caller leads.
	_, shared, done := c.Join(ctx, "k")
	defer done(cache.Entry{}, false)
	if shared {
		t.Fatal("a new caller joined a finished flight")
	}
}

func TestAWaiterCanGiveUp(t *testing.T) {
	c := newCache(t, cache.Options{})
	_, _, finish := c.Join(context.Background(), "k")
	defer finish(cache.Entry{}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, shared, done := c.Join(ctx, "k")
	done(cache.Entry{}, false)
	if shared || time.Since(start) > time.Second {
		t.Fatalf("shared = %v after %v", shared, time.Since(start))
	}
}

// Flights are bounded, so a flood of distinct requests can't grow memory without limit.
// Past the bound, requests simply don't share work.
func TestFlightsAreBounded(t *testing.T) {
	c := newCache(t, cache.Options{MaxFlights: 2})
	ctx := context.Background()
	_, _, a := c.Join(ctx, "a")
	_, _, b := c.Join(ctx, "b")
	_, _, extra := c.Join(ctx, "c")
	if got := c.Flights(); got != 2 {
		t.Fatalf("flights = %d", got)
	}
	// "c" isn't tracked, so another "c" doesn't wait for it.
	done := make(chan bool, 1)
	go func() {
		_, shared, finish := c.Join(ctx, "c")
		finish(cache.Entry{}, false)
		done <- shared
	}()
	select {
	case shared := <-done:
		if shared {
			t.Fatal("shared an untracked flight")
		}
	case <-time.After(time.Second):
		t.Fatal("waited on a flight that isn't tracked")
	}
	for _, f := range []func(cache.Entry, bool){a, b, extra} {
		f(cache.Entry{}, false)
		f(cache.Entry{}, false) // Finishing twice is harmless.
	}
	if got := c.Flights(); got != 0 {
		t.Fatalf("flights = %d after finishing", got)
	}
}
