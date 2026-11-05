package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/mock"
	"github.com/Bispulm/llmgate/internal/sse"
)

const testConfig = `
server:
  listen: 127.0.0.1:0
  shutdown_timeout: 2s
auth:
  dev_key_env: TEST_DEV_KEY
providers:
  - name: primary
    type: openai-compatible
    base_url: http://127.0.0.1:18301/v1
    api_key_env: TEST_PROVIDER_KEY
models:
  - alias: fast
    max_output_tokens: 64
    routes:
      - provider: primary
        model: alpha
`

func writeTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEnv(key string) string {
	return map[string]string{"TEST_DEV_KEY": "dev", "TEST_PROVIDER_KEY": "provider"}[key]
}

func TestServeRunsUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", writeTestConfig(t)}, process{getenv: testEnv, stdout: io.Discard, stderr: io.Discard, started: func(l listening) { addrs <- l.Gateway }})
	}()

	var addr string
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("run returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after cancel")
	}
}

// startServe runs the gateway command with the given configuration, stopping it
// when the test ends.
func startServe(t *testing.T, config string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", path}, process{getenv: testEnv, stdout: io.Discard, stderr: io.Discard, started: func(l listening) { addrs <- l.Gateway }})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run returned %v", err)
		}
	})

	select {
	case addr := <-addrs:
		return addr
	case err := <-done:
		done <- err
		t.Fatalf("run returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return ""
}

func TestServeProxiesToTheConfiguredProvider(t *testing.T) {
	upstream := httptest.NewServer(mock.New())
	defer upstream.Close()

	config := strings.Replace(testConfig, "http://127.0.0.1:18301/v1", upstream.URL+"/v1", 1)
	addr := startServe(t, config)

	post := func(t *testing.T, key, body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
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
	const message = `"messages":[{"role":"user","content":"hello"}]`

	t.Run("non-streaming", func(t *testing.T) {
		resp, body := post(t, "dev", `{"model":"fast",`+message+`}`)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "[alpha]") {
			t.Fatalf("status %d, body %s", resp.StatusCode, body)
		}
	})
	t.Run("streaming", func(t *testing.T) {
		resp, body := post(t, "dev", `{"model":"fast","stream":true,`+message+`}`)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "[alpha]") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Fatalf("status %d, body %s", resp.StatusCode, body)
		}
	})
	t.Run("the configured client key is required", func(t *testing.T) {
		if resp, _ := post(t, "wrong", `{"model":"fast",`+message+`}`); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("the configured output limit applies", func(t *testing.T) {
		if resp, _ := post(t, "dev", `{"model":"fast","max_tokens":65,`+message+`}`); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}

func TestServeAppliesProviderConcurrencyLimits(t *testing.T) {
	m := mock.New()
	upstream := httptest.NewServer(m)
	defer upstream.Close()

	config := strings.Replace(testConfig, "http://127.0.0.1:18301/v1", upstream.URL+"/v1", 1)
	config = strings.Replace(config, "api_key_env: TEST_PROVIDER_KEY", "api_key_env: TEST_PROVIDER_KEY\n    max_concurrency: 1", 1)
	config = strings.Replace(config, "model: alpha", "model: 'held;latency=1s'", 1)
	addr := startServe(t, config)

	send := func() int {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions",
			strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			t.Error(err)
			return 0
		}
		req.Header.Set("Authorization", "Bearer dev")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return 0
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	first := make(chan int, 1)
	go func() { first <- send() }()
	deadline := time.Now().Add(5 * time.Second)
	for m.Stats()["held"].Requests != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first request never reached the provider")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := send(); got != http.StatusServiceUnavailable {
		t.Fatalf("second request = %d, want it shed while the first runs", got)
	}
	if got := <-first; got != http.StatusOK {
		t.Fatalf("first request = %d", got)
	}
}

func TestServeProxiesToAnAnthropicProvider(t *testing.T) {
	upstream := httptest.NewServer(mock.New())
	defer upstream.Close()

	config := strings.Replace(testConfig, "http://127.0.0.1:18301/v1", upstream.URL, 1)
	config = strings.Replace(config, "type: openai-compatible", "type: anthropic", 1)
	addr := startServe(t, config)

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions",
		strings.NewReader(`{"model":"fast","messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer dev")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"object":"chat.completion"`) || !strings.Contains(string(body), "[alpha]") {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
}

// startStream begins a streaming request and reads its first event, so the request
// is known to be in flight.
func startStream(t *testing.T, addr string) (*http.Response, *sse.Decoder) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions",
		strings.NewReader(`{"model":"fast","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer dev")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	decoder := sse.NewDecoder(resp.Body, 1<<16)
	if _, err := decoder.Next(); err != nil {
		t.Fatal(err)
	}
	return resp, decoder
}

// lastEvent reads a stream to its end and returns its final event's data.
func lastEvent(decoder *sse.Decoder) string {
	last := ""
	for {
		ev, err := decoder.Next()
		if err != nil {
			return last
		}
		last = string(ev.Data)
	}
}

func TestShutdown(t *testing.T) {
	serveWith := func(t *testing.T, logs io.Writer, upstreamURL, model, drain string) (string, context.CancelFunc, chan error) {
		t.Helper()
		config := strings.Replace(testConfig, "http://127.0.0.1:18301/v1", upstreamURL+"/v1", 1)
		config = strings.Replace(config, "model: alpha", "model: "+quoteYAML(model), 1)
		config = strings.Replace(config, "shutdown_timeout: 2s", "shutdown_timeout: "+drain, 1)
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		addrs := make(chan string, 1)
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, []string{"serve", "--config", path}, process{getenv: testEnv, stdout: io.Discard, stderr: logs, started: func(l listening) { addrs <- l.Gateway }})
		}()
		select {
		case addr := <-addrs:
			return addr, cancel, done
		case err := <-done:
			t.Fatalf("run returned early: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("server did not start")
		}
		return "", cancel, done
	}
	waitForRun := func(t *testing.T, done chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("run did not return")
		}
	}

	t.Run("a stream that finishes within the drain timeout completes", func(t *testing.T) {
		upstream := httptest.NewServer(mock.New())
		defer upstream.Close()
		addr, cancel, done := serveWith(t, io.Discard, upstream.URL, "alpha;tokens=6;interval=50ms", "5s")
		_, decoder := startStream(t, addr)
		cancel()
		if got := lastEvent(decoder); got != "[DONE]" {
			t.Fatalf("last event = %q", got)
		}
		waitForRun(t, done)
	})
	t.Run("a stream outliving the drain timeout is ended, along with its upstream call", func(t *testing.T) {
		m := mock.New()
		upstream := httptest.NewServer(m)
		defer upstream.Close()
		var logs syncWriter
		addr, cancel, done := serveWith(t, &logs, upstream.URL, "stuck;tokens=64;interval=1s", "200ms")
		_, decoder := startStream(t, addr)

		start := time.Now()
		cancel()
		waitForRun(t, done)
		if got := time.Since(start); got > 5*time.Second {
			t.Fatalf("shutdown took %v", got)
		}
		// The process may exit as soon as run returns, so the record must exist by now.
		if !strings.Contains(logs.String(), `"msg":"upstream attempt"`) {
			t.Fatalf("the ended stream's attempt was not recorded before exit:\n%s", logs.String())
		}
		if got := lastEvent(decoder); got == "[DONE]" {
			t.Fatal("a stream ended by shutdown looks successful")
		}
		deadline := time.Now().Add(5 * time.Second)
		for m.Stats()["stuck"].Canceled != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("upstream call was not canceled: %+v", m.Stats()["stuck"])
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func quoteYAML(s string) string { return "'" + s + "'" }

// syncWriter is a log destination that can be read while handlers still write.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestServeLogsThatBudgetsAreNotEnforced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", writeTestConfig(t)}, process{getenv: testEnv, stdout: io.Discard, stderr: &logs, started: func(listening) { close(started) }})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("run returned early: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "budgets are not enforced") {
		t.Fatalf("logs = %s", logs.String())
	}
}

func TestRunReportsUsageErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no command", nil, "usage"},
		{"unknown command", []string{"nope"}, "unknown command"},
		{"serve without a config", []string{"serve"}, "config"},
		{"unknown flag", []string{"serve", "-nope"}, "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(context.Background(), tt.args, process{getenv: testEnv, stdout: io.Discard, stderr: io.Discard})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestServeRefusesToStartWithoutCredentials(t *testing.T) {
	err := run(context.Background(), []string{"serve", "--config", writeTestConfig(t)},
		process{getenv: func(string) string { return "" }, stdout: io.Discard, stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "TEST_DEV_KEY") {
		t.Fatalf("error = %v", err)
	}
}

func TestServeExportsMetricsOnTheirOwnListener(t *testing.T) {
	upstream := httptest.NewServer(mock.New())
	defer upstream.Close()
	config := strings.Replace(testConfig, "http://127.0.0.1:18301/v1", upstream.URL+"/v1", 1) + "metrics:\n  listen: 127.0.0.1:0\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan listening, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", path}, process{getenv: testEnv, stdout: io.Discard, stderr: io.Discard, started: func(l listening) { started <- l }})
	}()
	var addrs listening
	select {
	case addrs = <-started:
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve didn't start")
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	if addrs.Metrics == "" || addrs.Metrics == addrs.Gateway {
		t.Fatalf("addresses = %+v", addrs)
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+addrs.Gateway+"/v1/chat/completions", strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer dev")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	get := func(url string) (int, string) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	status, body := get("http://" + addrs.Metrics + "/metrics")
	for _, want := range []string{`llmgate_requests_total{alias="fast",status="200",stream="false"} 1`, `llmgate_breaker_state{model="alpha",provider="primary",state="closed"} 1`, "go_goroutines"} {
		if status != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("status %d, missing %s", status, want)
		}
	}
	if status, _ := get("http://" + addrs.Gateway + "/metrics"); status != http.StatusNotFound {
		t.Fatalf("the gateway's listener serves metrics: %d", status)
	}
}
