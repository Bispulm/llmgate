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
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/mock"
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
		done <- run(ctx, []string{"serve", "-config", writeTestConfig(t)}, testEnv, io.Discard, func(addr string) { addrs <- addr })
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
		done <- run(ctx, []string{"serve", "-config", path}, testEnv, io.Discard, func(addr string) { addrs <- addr })
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

func TestServeLogsThatBudgetsAreNotEnforced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "-config", writeTestConfig(t)}, testEnv, &logs, func(string) { close(started) })
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
		{"serve without a config", []string{"serve"}, "-config"},
		{"unknown flag", []string{"serve", "-nope"}, "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(context.Background(), tt.args, testEnv, io.Discard, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestServeRefusesToStartWithoutCredentials(t *testing.T) {
	err := run(context.Background(), []string{"serve", "-config", writeTestConfig(t)},
		func(string) string { return "" }, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "TEST_DEV_KEY") {
		t.Fatalf("error = %v", err)
	}
}
