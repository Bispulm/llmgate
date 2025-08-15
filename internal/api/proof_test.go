package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/store"
)

// The central claim: however many requests arrive at however many replicas, settled spend
// plus outstanding liability never exceeds the limit, every admitted attempt is accounted
// for exactly once, and nothing reaches a provider without a reservation.
func TestABudgetHoldsUnderConcurrentRequestsAcrossReplicas(t *testing.T) {
	if testing.Short() {
		t.Skip("a load test")
	}
	const (
		replicas = 4
		requests = 1200
		model    = "load;tokens=3;latency=20ms"
	)
	e := newTenantEnv(t, model)
	req, err := chat.DecodeRequest([]byte(chatBody(false, "")))
	if err != nil {
		t.Fatal(err)
	}
	prices := accounting.Prices{Input: 1_000_000, CachedInput: 100_000, CacheWrite: 1_000_000, Output: 2_000_000}
	reservation, err := prices.Reservation(accounting.InputBound(req), 10)
	if err != nil {
		t.Fatal(err)
	}
	// Room for about 40 reservations at once, against 1,200 requests.
	limit := 40*reservation + reservation/2
	key := e.addTeam("a", limit)

	type replica struct {
		url    string
		client *http.Client
	}
	gateways := make([]replica, replicas)
	for i := range gateways {
		// Admissions for one team take turns, so on a busy machine the queue can outlast the
		// default admission timeout and fail closed. This test is about correctness, so it
		// gives the queue time to drain.
		gateways[i].url, gateways[i].client = e.instance(instanceOptions{logger: slog.New(slog.DiscardHandler), adjust: func(l *Limits) {
			l.AdmissionTimeout = 60 * time.Second
			l.RequestTimeout = 90 * time.Second
		}}, model)
	}

	// Watch the period row while requests run. Each read sees one committed state.
	var samples, violations atomic.Int64
	watchCtx, stopWatching := context.WithCancel(context.Background())
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for watchCtx.Err() == nil {
			var lim, settled, reserved int64
			err := e.pool.QueryRow(watchCtx, `SELECT limit_micros, settled_micros, reserved_micros FROM budget_periods WHERE team_id = 'a'`).Scan(&lim, &settled, &reserved)
			if err == nil {
				samples.Add(1)
				if settled+reserved > lim || reserved < 0 {
					violations.Add(1)
				}
			}
		}
	}()

	var ok, refused, other atomic.Int64
	var unexpected sync.Map
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() {
			g := gateways[i%replicas]
			<-start
			r, err := http.NewRequest(http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(chatBody(i%2 == 0, "")))
			if err != nil {
				other.Add(1)
				return
			}
			r.Header.Set("Authorization", "Bearer "+key)
			resp, err := g.client.Do(r)
			if err != nil {
				other.Add(1)
				unexpected.Store(err.Error(), true)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK && (i%2 == 1 || strings.HasSuffix(string(body), "data: [DONE]\n\n")):
				ok.Add(1)
			case resp.StatusCode == http.StatusTooManyRequests && strings.Contains(string(body), codeBudgetExhausted):
				refused.Add(1)
			default:
				other.Add(1)
				unexpected.Store(fmt.Sprintf("%d %s", resp.StatusCode, body), true)
			}
		})
	}
	close(start)
	wg.Wait()

	// Settlement finishes as each handler returns, just after its client is answered.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var open int64
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM attempts WHERE state IN ('reserved', 'dispatched', 'uncertain')`).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d attempts never settled", open)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopWatching()
	<-watched

	unexpected.Range(func(k, _ any) bool { t.Errorf("unexpected result: %v", k); return true })
	t.Logf("%d served, %d refused for budget, %d other, %d samples of the period", ok.Load(), refused.Load(), other.Load(), samples.Load())
	if other.Load() != 0 {
		t.Fatalf("%d requests ended some other way", other.Load())
	}
	if ok.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("served %d and refused %d: the budget never came under contention", ok.Load(), refused.Load())
	}
	if violations.Load() != 0 || samples.Load() < 10 {
		t.Fatalf("%d of %d samples broke the invariant", violations.Load(), samples.Load())
	}

	var attempts, reported, wrongCharges, entries, entrySum, chargeSum int64
	err = e.pool.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE charge_kind = 'reported'),
		  count(*) FILTER (WHERE charge_micros <> prompt_tokens + 2 * completion_tokens),
		  COALESCE(sum(charge_micros), 0)
		FROM attempts WHERE team_id = 'a'`).Scan(&attempts, &reported, &wrongCharges, &chargeSum)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(sum(amount_micros), 0) FROM ledger_entries`).Scan(&entries, &entrySum); err != nil {
		t.Fatal(err)
	}
	// Every served request is one attempt, charged once from its report. A refused request
	// left nothing behind, and never reached the provider.
	if attempts != ok.Load() || reported != ok.Load() || entries != ok.Load() || wrongCharges != 0 {
		t.Fatalf("%d served, but %d attempts, %d reported, %d ledger entries, %d charged wrongly", ok.Load(), attempts, reported, entries, wrongCharges)
	}
	if got := e.mock.Stats()["load"].Requests; got != ok.Load() {
		t.Fatalf("the provider saw %d requests, and %d were admitted", got, ok.Load())
	}
	period := e.period("a")
	if period.reserved != 0 || period.settled != chargeSum || period.settled != entrySum || period.settled > limit {
		t.Fatalf("period = %+v, charges = %d, ledger = %d, limit = %d", period, chargeSum, entrySum, limit)
	}
}

// A database that can't be reached at all, rather than one that misbehaves, still means
// nothing is sent.
func TestAnUnreachableDatabaseFailsClosed(t *testing.T) {
	const model = "alpha;tokens=1"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	dead, err := store.Open(context.Background(), store.PoolConfig{URL: "postgres://llmgate:x@127.0.0.1:1/llmgate?sslmode=disable", MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	ledger := accounting.NewLedger(accounting.LedgerOptions{Pool: dead, Prices: e.book})
	url, client := e.instance(instanceOptions{ledger: ledger}, model)

	start := time.Now()
	resp, body := call(t, client, url, key, chatBody(true, ""))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, codeAccountingUnavailable) {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed > defaultAdmissionTimeout {
		t.Fatalf("took %v", elapsed)
	}
	if got := e.mock.Stats()["alpha"].Requests; got != 0 {
		t.Fatalf("the upstream saw %d requests", got)
	}
}

// Many clients that stop reading mustn't leave goroutines, upstream connections, or
// buffered output behind.
func TestManyStalledStreamsReleaseEverything(t *testing.T) {
	if testing.Short() {
		t.Skip("a load test")
	}
	const (
		streams = 100
		tokens  = 200000
	)
	g := newGateway(t, fmt.Sprintf("stuck;tokens=%d;pad=4096", tokens), func(o *Options) {
		o.Limits.ClientWriteTimeout = 300 * time.Millisecond
		o.Routes = routingWithLimit(o.Routes, tokens)
		o.Recorder = accounting.NewLogRecorder(slog.New(slog.DiscardHandler))
	})
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	baseline := runtime.NumGoroutine()

	body := chatBody(true, "")
	conns := make([]net.Conn, streams)
	for i := range conns {
		conn, err := net.Dial("tcp", strings.TrimPrefix(g.url, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn
		if err := conn.(*net.TCPConn).SetReadBuffer(4096); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
			testAPIKey, len(body), body); err != nil {
			t.Fatal(err)
		}
	}

	// None of them reads. Every upstream has to be canceled once writes time out.
	deadline := time.Now().Add(20 * time.Second)
	for g.mock.Stats()["stuck"].Canceled < streams {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d upstreams canceled", g.mock.Stats()["stuck"].Canceled, streams)
		}
		time.Sleep(20 * time.Millisecond)
	}
	runtime.GC()
	var during runtime.MemStats
	runtime.ReadMemStats(&during)
	const perStream = 1 << 20
	if grown := int64(during.HeapInuse) - int64(before.HeapInuse); grown > streams*perStream {
		t.Fatalf("the heap grew by %d bytes for %d stalled streams", grown, streams)
	}
	if written := g.mock.Stats()["stuck"].BytesWritten; written > streams*(128<<20) {
		t.Fatalf("the upstream wrote %d bytes", written)
	}

	for _, conn := range conns {
		_ = conn.Close()
	}
	deadline = time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > baseline+5 {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines, from %d before", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A database that stops answering, rather than refusing connections, mustn't hold a
// request that needs its key looked up. This is a lock on the keys table, as a migration
// stuck behind a long transaction would take.
func TestAHungKeyLookupIsRefusedQuickly(t *testing.T) {
	const model = "alpha;tokens=1"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	const bound = 300 * time.Millisecond
	url, client := e.instance(instanceOptions{adjust: func(l *Limits) { l.AdmissionTimeout = bound }}, model)

	ctx := context.Background()
	lock, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, `LOCK TABLE api_keys IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(chatBody(false, "")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("no answer after %v: %v", time.Since(start), err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), codeAuthUnavailable) {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed > bound+2*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}
