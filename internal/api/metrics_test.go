package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/metrics"
)

func scrapeMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	ts := httptest.NewServer(m.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// What the gateway did shows up in its metrics, with no request content in them.
func TestTheGatewayReportsWhatItDid(t *testing.T) {
	const failing, working = "primary;fail=status;status=500", "backup;tokens=2"
	e := newTenantEnv(t, failing, working)
	key := e.addTeam("a", 100_000)
	poor := e.addTeam("poor", 10)
	m := metrics.New(metrics.Options{})
	url, client := e.instance(instanceOptions{metrics: m, cache: newTestCache(t), cacheTTL: time.Minute}, failing, working)

	if resp, body := call(t, client, url, key, chatBody(true, "")); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if resp, _ := call(t, client, url, poor, chatBody(false, "")); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	ask(t, client, url, key, chatBody(false, ""), true)
	ask(t, client, url, key, chatBody(false, ""), true)
	if resp, _ := call(t, client, url, "lgk_nope_nope", chatBody(false, "")); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var out string
	deadline := time.Now().Add(5 * time.Second)
	for {
		out = scrapeMetrics(t, m)
		if strings.Contains(out, `llmgate_charged_micros_total{kind="reported",model="backup;tokens=2",provider="openai"}`) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{
		`llmgate_requests_total{alias="fast",status="200",stream="true"} 1`,
		`llmgate_requests_total{alias="fast",status="429",stream="false"} 1`,
		`llmgate_requests_total{alias="",status="401",stream="false"} 1`,
		`llmgate_attempts_total{error_kind="server",model="primary;fail=status;status=500",outcome="failed",provider="openai",reason="primary"}`,
		`llmgate_fallbacks_total{model="backup;tokens=2",provider="openai"}`,
		`llmgate_budget_refusals_total 1`,
		`llmgate_cache_requests_total{alias="fast",result="miss"} 1`,
		`llmgate_cache_requests_total{alias="fast",result="hit"} 1`,
		`llmgate_charged_micros_total{kind="rejected",model="primary;fail=status;status=500",provider="openai"} 0`,
		`llmgate_charged_micros_total{kind="reported",model="backup;tokens=2",provider="openai"}`,
		`llmgate_time_to_first_event_seconds_count{model="backup;tokens=2",provider="openai"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, secret := range []string{testPrompt, key, poor, "lgk_nope"} {
		if strings.Contains(out, secret) {
			t.Errorf("the metrics contain %q", secret)
		}
	}
}

func TestTeamLimitDenialsAreCounted(t *testing.T) {
	const model = "alpha;tokens=1"
	e := newTenantEnv(t, model)
	key := e.addTeam("a", 100_000)
	m := metrics.New(metrics.Options{})
	url, client := e.instance(instanceOptions{metrics: m, limiter: &denyingLimiter{decision: limits.Decision{Reason: limits.ReasonTokens, RetryAfter: time.Second}}}, model)
	call(t, client, url, key, chatBody(false, ""))
	if out := scrapeMetrics(t, m); !strings.Contains(out, `llmgate_team_limit_denials_total{reason="tokens"} 1`) {
		t.Fatalf("no denial counted:\n%s", out)
	}
}
