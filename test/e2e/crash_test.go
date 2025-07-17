// Package e2e runs the real gateway binary as a separate process, so that it can be
// killed the way a crash would kill it.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/mock"
	"github.com/Bispulm/llmgate/internal/testenv"
)

const (
	model      = "slow;tokens=1000;interval=20ms"
	adminToken = "an-e2e-admin-token-123"
)

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
)

// gatewayBinary builds the gateway once for every test in this package.
func gatewayBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "llmgate-e2e")
		if err != nil {
			buildErr = err
			return
		}
		binary = filepath.Join(dir, "llmgate")
		out, err := exec.Command("go", "build", "-o", binary, "github.com/Bispulm/llmgate/cmd/llmgate").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("building the gateway: %w: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binary != "" {
		_ = os.RemoveAll(filepath.Dir(binary))
	}
	os.Exit(code)
}

type world struct {
	t        *testing.T
	dbURL    string
	pool     *pgxpool.Pool
	mock     *mock.Server
	config   string
	key      string
	redisURL string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	redisURL := os.Getenv(testenv.RedisURLEnv)
	if redisURL == "" {
		testenv.Redis(t) // Skips, or fails when integration tests are required.
	}
	w := &world{t: t, dbURL: testenv.PostgresURL(t), mock: mock.New(), redisURL: redisURL}
	w.pool = testenv.PoolFor(t, w.dbURL, 4)
	upstream := httptest.NewServer(w.mock)
	t.Cleanup(upstream.Close)

	ctx := context.Background()
	if _, err := auth.NewTeamStore(w.pool).Create(ctx, auth.Team{ID: "acme", BudgetLimitMicros: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewKeyStore(auth.KeyStoreOptions{Pool: w.pool}).Create(ctx, "acme", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	w.key = key.Raw

	w.config = filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf(`
server:
  listen: 127.0.0.1:0
  shutdown_timeout: 1s
auth:
  mode: tenant
database:
  url_env: E2E_DATABASE_URL
redis:
  url_env: E2E_REDIS_URL
admin:
  listen: 127.0.0.1:0
  token_env: E2E_ADMIN_TOKEN
pricing:
  version: e2e-v1
  prices:
    - provider: mock
      model: %q
      input_usd_per_mtok: "1"
      cached_input_usd_per_mtok: "0.1"
      cache_write_usd_per_mtok: "1.25"
      output_usd_per_mtok: "2"
providers:
  - name: mock
    type: openai-compatible
    base_url: %s/v1
    api_key_env: E2E_PROVIDER_KEY
models:
  - alias: slow
    max_output_tokens: 1000
    routes:
      - provider: mock
        model: %q
`, model, upstream.URL, model)
	if err := os.WriteFile(w.config, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return w
}

type process struct {
	cmd  *exec.Cmd
	addr string
	done chan error
}

func (w *world) start() *process {
	w.t.Helper()
	cmd := exec.Command(gatewayBinary(w.t), "serve", "--config", w.config)
	cmd.Env = append(os.Environ(),
		"E2E_DATABASE_URL="+w.dbURL, "E2E_REDIS_URL="+w.redisURL, "E2E_ADMIN_TOKEN="+adminToken, "E2E_PROVIDER_KEY=provider")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		w.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	p := &process{cmd: cmd, done: make(chan error, 1)}
	addrs := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			var line struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Msg == "gateway listening" {
				addrs <- line.Addr
			}
		}
		p.done <- cmd.Wait()
	}()
	w.t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})
	select {
	case p.addr = <-addrs:
	case err := <-p.done:
		w.t.Fatalf("the gateway exited at startup: %v", err)
	case <-time.After(20 * time.Second):
		w.t.Fatal("the gateway didn't start")
	}
	return p
}

// startStream begins a streaming request and returns once content has arrived, so the
// attempt is certainly dispatched and the provider is billing.
func (w *world) startStream(p *process) io.Closer {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+p.addr+"/v1/chat/completions",
		strings.NewReader(`{"model":"slow","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		w.t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			w.t.Fatalf("the stream ended before any content: %v", err)
		}
		if strings.Contains(line, `"content"`) {
			return resp.Body
		}
	}
}

type attemptRow struct {
	state    string
	reserved int64
	charge   *int64
	kind     *string
}

func (w *world) onlyAttempt() attemptRow {
	w.t.Helper()
	var a attemptRow
	err := w.pool.QueryRow(context.Background(), `SELECT state, reserved_micros, charge_micros, charge_kind FROM attempts`).Scan(&a.state, &a.reserved, &a.charge, &a.kind)
	if err != nil {
		w.t.Fatal(err)
	}
	return a
}

func (w *world) period() (settled, reserved int64) {
	w.t.Helper()
	if err := w.pool.QueryRow(context.Background(), `SELECT settled_micros, reserved_micros FROM budget_periods`).Scan(&settled, &reserved); err != nil {
		w.t.Fatal(err)
	}
	return settled, reserved
}

// reconcileAt runs a reconciliation pass as if the clock read now+ahead, which is how a
// test stands in for waiting out a lease and a grace period.
func (w *world) reconcileAt(ahead time.Duration) accounting.ReconcileResult {
	w.t.Helper()
	book, err := accounting.EnsurePrices(context.Background(), w.pool, accounting.PriceList{Label: "e2e-v1", Entries: []accounting.PriceEntry{{
		Provider: "mock", Model: model, Prices: accounting.Prices{Input: 1_000_000, CachedInput: 100_000, CacheWrite: 1_250_000, Output: 2_000_000},
	}}})
	if err != nil {
		w.t.Fatal(err)
	}
	ledger := accounting.NewLedger(accounting.LedgerOptions{
		Pool: w.pool, Prices: book, UncertainGrace: 10 * time.Minute,
		Now: func() time.Time { return time.Now().Add(ahead) },
	})
	result, err := ledger.Reconcile(context.Background(), 100)
	if err != nil {
		w.t.Fatal(err)
	}
	return result
}

// A replica killed while a provider is mid-stream can't settle anything. What it left
// behind has to say that money may have been spent, and nothing may release it: only
// after the lease and the grace period does a reconciler charge the most it could cost.
func TestAKilledGatewayLeavesItsLiabilityForTheReconciler(t *testing.T) {
	w := newWorld(t)
	p := w.start()
	body := w.startStream(p)
	defer func() { _ = body.Close() }()

	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := <-p.done; err == nil {
		t.Fatal("a killed process reported a clean exit")
	}

	a := w.onlyAttempt()
	settled, reserved := w.period()
	if a.state != accounting.StateDispatched || reserved != a.reserved || settled != 0 {
		t.Fatalf("after the crash: attempt %+v, settled %d, reserved %d", a, settled, reserved)
	}
	// The provider's side ended with the process.
	deadline := time.Now().Add(10 * time.Second)
	for w.mock.Stats()["slow"].Canceled != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the upstream request never ended: %+v", w.mock.Stats()["slow"])
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The lease hasn't run out, so nothing changes yet.
	if got := w.reconcileAt(0); got != (accounting.ReconcileResult{}) {
		t.Fatalf("reconciled before the lease ran out: %+v", got)
	}
	// Once it has, the attempt is uncertain, and its liability is still held.
	if got := w.reconcileAt(2 * time.Hour); got.MarkedUncertain != 1 || got.Released != 0 {
		t.Fatalf("result = %+v", got)
	}
	if _, reserved := w.period(); reserved != a.reserved {
		t.Fatalf("the liability was released: reserved %d", reserved)
	}
	// After the grace period it's charged everything it reserved.
	if got := w.reconcileAt(3 * time.Hour); got.ChargedConservatively != 1 {
		t.Fatalf("result = %+v", got)
	}
	final := w.onlyAttempt()
	settled, reserved = w.period()
	if final.state != accounting.StateReconciled || final.kind == nil || *final.kind != accounting.ChargeConservative ||
		*final.charge != a.reserved || settled != a.reserved || reserved != 0 {
		t.Fatalf("attempt %+v, settled %d, reserved %d", final, settled, reserved)
	}

	// A new replica starts cleanly on the same database and serves the team.
	again := w.start()
	stream := w.startStream(again)
	_ = stream.Close()
	if err := again.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-again.done; err != nil {
		t.Fatalf("a clean shutdown exited with %v", err)
	}
}

// An orderly shutdown with a stream still running has time to settle what it can, so it
// leaves nothing for a reconciler to guess about the attempt's ownership.
func TestAnOrderlyShutdownSettlesWhatIsInFlight(t *testing.T) {
	w := newWorld(t)
	p := w.start()
	body := w.startStream(p)
	defer func() { _ = body.Close() }()

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the gateway didn't stop")
	}
	// The stream was cut short with no usage report, so its cost is unknown, and the
	// reservation stays held until usage arrives or the grace period passes.
	a := w.onlyAttempt()
	if a.state != accounting.StateUncertain {
		t.Fatalf("attempt = %+v", a)
	}
	if _, reserved := w.period(); reserved != a.reserved {
		t.Fatalf("reserved = %d", reserved)
	}
}
