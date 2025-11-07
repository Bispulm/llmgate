package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/mock"
	"github.com/Bispulm/llmgate/internal/testenv"
)

const adminToken = "a-test-admin-token-1234"

const tenantConfig = `
server:
  listen: 127.0.0.1:0
  shutdown_timeout: 2s
auth:
  mode: tenant
  key_cache_ttl: 1s
database:
  url_env: TEST_DATABASE_URL
redis:
  url_env: TEST_REDIS_URL
  key_prefix: "PREFIX"
admin:
  listen: 127.0.0.1:0
  token_env: TEST_ADMIN_TOKEN
pricing:
  version: test-v1
  prices:
    - provider: mock
      model: alpha;tokens=3
      input_usd_per_mtok: "1"
      cached_input_usd_per_mtok: "0.1"
      cache_write_usd_per_mtok: "1.25"
      output_usd_per_mtok: "2"
accounting:
  reconcile_interval: 50ms
providers:
  - name: mock
    type: openai-compatible
    base_url: UPSTREAM
    api_key_env: TEST_PROVIDER_KEY
models:
  - alias: fast
    max_output_tokens: 64
    cache:
      ttl: 1m
    routes:
      - provider: mock
        model: alpha;tokens=3
`

type tenantGateway struct {
	t       *testing.T
	env     map[string]string
	addrs   listening
	mock    *mock.Server
	cancel  context.CancelFunc
	done    chan error
	logs    *syncWriter
	gateway *http.Client
}

func (g *tenantGateway) getenv(k string) string { return g.env[k] }

// cli runs one command against this gateway's admin API, and returns what it printed.
func (g *tenantGateway) cli(args ...string) (string, error) {
	g.t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), append(args, "--admin-url", "http://"+g.addrs.Admin), process{getenv: g.getenv, stdout: &out, stderr: io.Discard})
	return out.String(), err
}

func (g *tenantGateway) mustCLI(args ...string) string {
	g.t.Helper()
	out, err := g.cli(args...)
	if err != nil {
		g.t.Fatalf("llmgate %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (g *tenantGateway) chat(key string) (*http.Response, string) {
	g.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+g.addrs.Gateway+"/v1/chat/completions",
		strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		g.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := g.gateway.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		g.t.Fatal(err)
	}
	return resp, string(body)
}

func tenantConfigFile(t *testing.T, upstream string) string {
	t.Helper()
	content := strings.ReplaceAll(tenantConfig, "UPSTREAM", upstream+"/v1")
	content = strings.ReplaceAll(content, "PREFIX", "test:"+strings.ToLower(rand.Text())+":")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startTenantGateway(t *testing.T, databaseURL string) *tenantGateway {
	t.Helper()
	redisURL := os.Getenv(testenv.RedisURLEnv)
	if redisURL == "" {
		testenv.Redis(t) // Skips, or fails when integration tests are required.
	}
	return startTenantGatewayOn(t, databaseURL, redisURL)
}

func startTenantGatewayOn(t *testing.T, databaseURL, redisURL string) *tenantGateway {
	t.Helper()
	m := mock.New()
	upstream := httptest.NewServer(m)
	t.Cleanup(upstream.Close)

	g := &tenantGateway{
		t:    t,
		mock: m,
		env: map[string]string{
			"TEST_DATABASE_URL": databaseURL, "TEST_REDIS_URL": redisURL,
			"TEST_ADMIN_TOKEN": adminToken, "TEST_PROVIDER_KEY": "provider", "LLMGATE_ADMIN_TOKEN": adminToken,
		},
		done:    make(chan error, 1),
		logs:    &syncWriter{},
		gateway: &http.Client{Timeout: 10 * time.Second},
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	started := make(chan listening, 1)
	path := tenantConfigFile(t, upstream.URL)
	go func() {
		g.done <- run(ctx, []string{"serve", "--config", path}, process{getenv: g.getenv, stdout: io.Discard, stderr: g.logs, started: func(l listening) { started <- l }})
	}()
	select {
	case g.addrs = <-started:
	case err := <-g.done:
		t.Fatalf("serve returned early: %v\n%s", err, g.logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	t.Cleanup(func() {
		g.cancel()
		if err := <-g.done; err != nil {
			t.Errorf("serve: %v", err)
		}
		g.gateway.CloseIdleConnections()
	})
	return g
}

func TestTenantModeFromTeamToUsage(t *testing.T) {
	g := startTenantGateway(t, testenv.PostgresURL(t))

	g.mustCLI("teams", "create", "acme", "--budget-usd", "0.01", "--requests-per-minute", "100")
	created := g.mustCLI("keys", "create", "acme", "--name", "ci")
	key := strings.SplitN(created, "\n", 2)[0]
	if !strings.HasPrefix(key, "lgk_") || !strings.Contains(created, "only time") {
		t.Fatalf("keys create printed %q", created)
	}

	resp, body := g.chat(key)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "[alpha]") {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Llmgate-Budget-Enforcement"); got != "strict" {
		t.Fatalf("enforcement = %q", got)
	}

	// Settlement finishes as the handler returns, a moment after the client has its answer.
	var usage string
	deadline := time.Now().Add(5 * time.Second)
	for {
		usage = g.mustCLI("usage", "acme", "--attempts", "5")
		if strings.Contains(usage, "1 reconciled") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{"team acme", "1 requests", "1 reconciled", "mock", "alpha;tokens=3", "reported"} {
		if !strings.Contains(usage, want) {
			t.Fatalf("usage is missing %q:\n%s", want, usage)
		}
	}

	var parsed struct {
		Usage struct {
			Budget struct {
				SettledMicros int64 `json:"settled_micros"`
			} `json:"budget"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(g.mustCLI("usage", "acme", "--json")), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Usage.Budget.SettledMicros == 0 {
		t.Fatal("nothing was charged")
	}

	t.Run("an opted-in repeat comes from the cache", func(t *testing.T) {
		for i, want := range []string{"miss", "hit"} {
			req, err := http.NewRequest(http.MethodPost, "http://"+g.addrs.Gateway+"/v1/chat/completions",
				strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"cache me"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-Llmgate-Cache", "allow")
			resp, err := g.gateway.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if got := resp.Header.Get("X-Llmgate-Cache"); resp.StatusCode != http.StatusOK || got != want {
				t.Fatalf("request %d: status %d, cache %q", i+1, resp.StatusCode, got)
			}
		}
	})

	t.Run("an exhausted team is refused while another carries on", func(t *testing.T) {
		g.mustCLI("teams", "create", "broke", "--budget-usd", "0.0001")
		poor := strings.SplitN(g.mustCLI("keys", "create", "broke"), "\n", 2)[0]
		resp, body := g.chat(poor)
		if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "budget_exhausted") {
			t.Fatalf("status = %d: %s", resp.StatusCode, body)
		}
		if resp, _ := g.chat(key); resp.StatusCode != http.StatusOK {
			t.Fatalf("the other team got %d", resp.StatusCode)
		}
		g.mustCLI("budgets", "set", "broke", "--limit-usd", "1")
		if resp, body := g.chat(poor); resp.StatusCode != http.StatusOK {
			t.Fatalf("after raising the budget: status = %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("listing and showing", func(t *testing.T) {
		if out := g.mustCLI("teams", "list"); !strings.Contains(out, "acme") || !strings.Contains(out, "$0.01") {
			t.Fatalf("teams list:\n%s", out)
		}
		if out := g.mustCLI("budgets", "show", "acme"); !strings.Contains(out, "available") {
			t.Fatalf("budgets show:\n%s", out)
		}
		if out := g.mustCLI("providers", "status"); !strings.Contains(out, "fast") || !strings.Contains(out, "closed") {
			t.Fatalf("providers status:\n%s", out)
		}
		if out := g.mustCLI("reconcile"); !strings.Contains(out, "released") {
			t.Fatalf("reconcile:\n%s", out)
		}
		if out := g.mustCLI("keys", "list", "acme"); !strings.Contains(out, "active") || strings.Contains(out, key) {
			t.Fatalf("keys list:\n%s", out)
		}
	})

	t.Run("a revoked key stops working on this replica at once", func(t *testing.T) {
		parts := strings.SplitN(key, "_", 3)
		g.mustCLI("keys", "revoke", parts[0]+"_"+parts[1])
		if resp, _ := g.chat(key); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("the admin API refuses a wrong token", func(t *testing.T) {
		g.env["LLMGATE_ADMIN_TOKEN"] = "not-the-admin-token"
		defer func() { g.env["LLMGATE_ADMIN_TOKEN"] = adminToken }()
		if _, err := g.cli("teams", "list"); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("the admin API isn't on the gateway's listener", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "http://"+g.addrs.Gateway+"/admin/v1/teams", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := g.gateway.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
}

func TestServeRefusesAnUnmigratedDatabase(t *testing.T) {
	url := testenv.UnmigratedPostgresURL(t)
	path := tenantConfigFile(t, "http://127.0.0.1:1")
	env := map[string]string{"TEST_DATABASE_URL": url, "TEST_REDIS_URL": "redis://127.0.0.1:1", "TEST_ADMIN_TOKEN": adminToken, "TEST_PROVIDER_KEY": "p"}
	getenv := func(k string) string { return env[k] }
	err := run(context.Background(), []string{"serve", "--config", path}, process{getenv: getenv, stdout: io.Discard, stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "llmgate migrate") {
		t.Fatalf("err = %v", err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"migrate", "--config", path}, process{getenv: getenv, stdout: &out, stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "migrations applied") {
		t.Fatalf("migrate printed %q", out.String())
	}
	g := startTenantGateway(t, url)
	if out := g.mustCLI("teams", "list"); !strings.Contains(out, "TEAM") {
		t.Fatalf("teams list:\n%s", out)
	}
}

func TestAdminCommandsNeedAToken(t *testing.T) {
	err := run(context.Background(), []string{"teams", "list"}, process{getenv: func(string) string { return "" }, stdout: io.Discard, stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "LLMGATE_ADMIN_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestUSDFormatting(t *testing.T) {
	for micros, want := range map[int64]string{0: "$0.00", 10_000: "$0.01", 5_000_000: "$5.00", 1_234_567: "$1.234567", 100: "$0.0001", -2_500_000: "-$2.50"} {
		if got := usd(micros); got != want {
			t.Errorf("usd(%d) = %q, want %q", micros, got, want)
		}
	}
}

// A Redis that hangs, rather than refusing connections, has to cost a request almost
// nothing: team limits fail closed quickly, and the cache is a quick miss.
func TestAHungRedisCostsLittle(t *testing.T) {
	g := startTenantGatewayOn(t, testenv.PostgresURL(t), "redis://"+testenv.SilentRedis(t)+"/0")
	g.mustCLI("teams", "create", "limited", "--budget-usd", "1", "--requests-per-minute", "100")
	g.mustCLI("teams", "create", "open", "--budget-usd", "1")
	limited := strings.SplitN(g.mustCLI("keys", "create", "limited"), "\n", 2)[0]
	open := strings.SplitN(g.mustCLI("keys", "create", "open"), "\n", 2)[0]

	for range 3 {
		start := time.Now()
		resp, body := g.chat(limited)
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "rate_limit_unavailable") {
			t.Fatalf("status = %d: %s", resp.StatusCode, body)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("team limits took %v to fail closed", elapsed)
		}
	}
	for range 3 {
		req, err := http.NewRequest(http.MethodPost, "http://"+g.addrs.Gateway+"/v1/chat/completions",
			strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+open)
		req.Header.Set("X-Llmgate-Cache", "allow")
		start := time.Now()
		resp, err := g.gateway.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Llmgate-Cache") != "miss" {
			t.Fatalf("status = %d, cache %q", resp.StatusCode, resp.Header.Get("X-Llmgate-Cache"))
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("a cache miss took %v", elapsed)
		}
	}
}
