package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
