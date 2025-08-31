package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
server:
  listen: 127.0.0.1:18300
auth:
  dev_key_env: LLMGATE_DEV_KEY
limits:
  request_timeout: 45s
providers:
  - name: primary
    type: openai-compatible
    base_url: http://127.0.0.1:18301/v1
    api_key_env: PRIMARY_KEY
models:
  - alias: fast
    max_output_tokens: 1024
    routes:
      - provider: primary
        model: alpha
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadParsesAndAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:18300" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Limits.RequestTimeout != 45*time.Second {
		t.Errorf("explicit request_timeout = %v", cfg.Limits.RequestTimeout)
	}
	if cfg.Limits.MaxEventBytes != DefaultMaxEventBytes {
		t.Errorf("default max_event_bytes = %d", cfg.Limits.MaxEventBytes)
	}
	if cfg.Limits.StreamIdleTimeout != DefaultStreamIdleTimeout {
		t.Errorf("default stream_idle_timeout = %v", cfg.Limits.StreamIdleTimeout)
	}
	if got := cfg.Limits.CompletionTimeout; got != 45*time.Second/2 {
		t.Errorf("default completion_timeout = %v: half the deadline leaves time to fall back", got)
	}
	if got := cfg.Limits.AdmissionTimeout; got != DefaultAdmissionTimeout {
		t.Errorf("default admission_timeout = %v", got)
	}
	if got := cfg.Providers[0].MaxTokensParam; got != MaxCompletionTokens {
		t.Errorf("default max_tokens_param = %q", got)
	}
	if got := cfg.Models[0].Routes[0]; got.Provider != "primary" || got.Model != "alpha" {
		t.Errorf("route = %+v", got)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name          string
		old, new      string
		wantInMessage string
	}{
		{"unknown field", "listen: 127.0.0.1:18300", "listen: 127.0.0.1:18300\n  extra: 1", "extra"},
		{"public listener without tenant auth", "listen: 127.0.0.1:18300", "listen: 0.0.0.0:18300", "loopback"},
		{"listener with no host", "listen: 127.0.0.1:18300", "listen: :18300", "loopback"},
		{"missing listener", "listen: 127.0.0.1:18300", "listen: ''", "listen"},
		{"missing dev key env", "dev_key_env: LLMGATE_DEV_KEY", "dev_key_env: ''", "dev_key_env"},
		{"no base url", "base_url: http://127.0.0.1:18301/v1", "base_url: ''", "base_url"},
		{"unsupported scheme", "base_url: http://127.0.0.1:18301/v1", "base_url: ftp://example.com", "base_url"},
		{"plaintext to a remote host", "base_url: http://127.0.0.1:18301/v1", "base_url: http://api.example.com/v1", "https"},
		{"unknown provider type", "type: openai-compatible", "type: nonsense", "type"},
		{"no api key env", "api_key_env: PRIMARY_KEY", "api_key_env: ''", "api_key_env"},
		{"bad max_tokens_param", "api_key_env: PRIMARY_KEY", "api_key_env: PRIMARY_KEY\n    max_tokens_param: nope", "max_tokens_param"},
		{"route to unknown provider", "provider: primary", "provider: missing", "missing"},
		{"queue with no concurrency limit", "api_key_env: PRIMARY_KEY", "api_key_env: PRIMARY_KEY\n    max_queue: 5", "max_concurrency"},
		{"per-team limit with no concurrency limit", "api_key_env: PRIMARY_KEY", "api_key_env: PRIMARY_KEY\n    max_in_flight_per_team: 5", "max_concurrency"},
		{"negative concurrency", "api_key_env: PRIMARY_KEY", "api_key_env: PRIMARY_KEY\n    max_concurrency: -1", "negative"},
		{"zero output limit", "max_output_tokens: 1024", "max_output_tokens: 0", "max_output_tokens"},
		{"negative timeout", "request_timeout: 45s", "request_timeout: -1s", "request_timeout"},
		{"empty alias", "alias: fast", "alias: ''", "alias"},
		{"a first-event timeout past the deadline", "request_timeout: 45s", "request_timeout: 45s\n  first_event_timeout: 46s", "first_event_timeout"},
		{"a completion timeout past the deadline", "request_timeout: 45s", "request_timeout: 45s\n  completion_timeout: 46s", "completion_timeout"},
		{"an admission timeout past the deadline", "request_timeout: 45s", "request_timeout: 45s\n  admission_timeout: 46s", "admission_timeout"},
		{
			"the same route twice",
			"        model: alpha",
			"        model: alpha\n      - provider: primary\n        model: alpha",
			"duplicates",
		},
		{"negative tier", "        model: alpha", "        model: alpha\n        tier: -1", "tier"},
		{"negative weight", "        model: alpha", "        model: alpha\n        weight: -1", "weight"},
		{"too many attempts", "max_output_tokens: 1024", "max_output_tokens: 1024\n    max_attempts: 11", "max_attempts"},
		{"negative attempts", "max_output_tokens: 1024", "max_output_tokens: 1024\n    max_attempts: -1", "max_attempts"},
		{"error rate over 1", "limits:", "circuit_breaker:\n  error_rate: 1.5\nlimits:", "error_rate"},
		{"negative cooldown", "limits:", "circuit_breaker:\n  cooldown: -1s\nlimits:", "circuit_breaker"},
		{"negative probes", "limits:", "circuit_breaker:\n  half_open_probes: -1\nlimits:", "circuit_breaker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(validYAML, tt.old) {
				t.Fatalf("test does not apply: %q not in the base config", tt.old)
			}
			_, err := Load(writeConfig(t, strings.Replace(validYAML, tt.old, tt.new, 1)))
			if err == nil {
				t.Fatal("Load accepted the config")
			}
			if !strings.Contains(err.Error(), tt.wantInMessage) {
				t.Fatalf("error %q does not mention %q", err, tt.wantInMessage)
			}
		})
	}
}

func TestLoadRejectsDuplicates(t *testing.T) {
	t.Run("provider names", func(t *testing.T) {
		dup := strings.Replace(validYAML, "models:", `  - name: primary
    type: openai-compatible
    base_url: http://127.0.0.1:18301/v1
    api_key_env: OTHER_KEY
models:`, 1)
		if _, err := Load(writeConfig(t, dup)); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("aliases", func(t *testing.T) {
		dup := validYAML + `  - alias: fast
    max_output_tokens: 1
    routes:
      - provider: primary
        model: beta
`
		if _, err := Load(writeConfig(t, dup)); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestLoadAcceptsLoopbackListeners(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:18300", "localhost:18300", "[::1]:18300", "127.0.0.1:0"} {
		content := strings.Replace(validYAML, "listen: 127.0.0.1:18300", "listen: "+quoteYAML(listen), 1)
		if _, err := Load(writeConfig(t, content)); err != nil {
			t.Errorf("listen %q: %v", listen, err)
		}
	}
}

func quoteYAML(s string) string { return "'" + s + "'" }

func TestLoadAcceptsFallbackRoutes(t *testing.T) {
	content := strings.Replace(validYAML, "        model: alpha", `        model: alpha
        weight: 3
      - provider: primary
        model: beta
        tier: 1`, 1)
	content = strings.Replace(content, "max_output_tokens: 1024", "max_output_tokens: 1024\n    max_attempts: 3", 1)
	content = strings.Replace(content, "limits:", `circuit_breaker:
  window: 30s
  min_requests: 5
  error_rate: 0.25
  cooldown: 10s
  half_open_probes: 1
limits:`, 1)
	cfg, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatal(err)
	}
	model := cfg.Models[0]
	if model.MaxAttempts != 3 || len(model.Routes) != 2 || model.Routes[0].Weight != 3 || model.Routes[1].Tier != 1 {
		t.Fatalf("model = %+v", model)
	}
	if b := cfg.CircuitBreaker; b.MinRequests != 5 || b.ErrorRate != 0.25 || b.Cooldown != 10*time.Second || b.HalfOpenProbes != 1 || b.Window != 30*time.Second {
		t.Fatalf("breaker = %+v", b)
	}
}

func TestLoadAcceptsConcurrencyLimits(t *testing.T) {
	content := strings.Replace(validYAML, "api_key_env: PRIMARY_KEY", `api_key_env: PRIMARY_KEY
    max_concurrency: 10
    max_queue: 20
    max_queue_per_team: 5
    max_in_flight_per_team: 4`, 1)
	cfg, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers[0]
	if p.MaxConcurrency != 10 || p.MaxQueue != 20 || p.MaxQueuePerTeam != 5 || p.MaxInFlightPerTeam != 4 {
		t.Fatalf("provider = %+v", p)
	}
	if p.QueueTimeout != DefaultQueueTimeout {
		t.Fatalf("queue timeout = %v", p.QueueTimeout)
	}
}

func TestLoadAcceptsAnAnthropicProvider(t *testing.T) {
	content := strings.Replace(validYAML, "type: openai-compatible", "type: anthropic", 1)
	cfg, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].Type != ProviderAnthropic {
		t.Fatalf("type = %q", cfg.Providers[0].Type)
	}
}

func TestLoadAcceptsHTTPSToRemoteHosts(t *testing.T) {
	content := strings.Replace(validYAML, "http://127.0.0.1:18301/v1", "https://api.example.com/v1", 1)
	if _, err := Load(writeConfig(t, content)); err != nil {
		t.Fatal(err)
	}
}

// Credentials belong in the environment. A literal key in a config file must be
// refused, and the refusal must not repeat the key into logs.
func TestLoadRefusesLiteralCredentialsWithoutEchoingThem(t *testing.T) {
	const secret = "sk-this-value-must-not-appear"
	content := strings.Replace(validYAML, "api_key_env: PRIMARY_KEY", "api_key: "+secret, 1)
	_, err := Load(writeConfig(t, content))
	if err == nil {
		t.Fatal("Load accepted a literal credential")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the credential: %v", err)
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}

func TestResolveSecrets(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PRIMARY_KEY": "provider-secret", "LLMGATE_DEV_KEY": "dev-secret"}

	t.Run("reads every credential from the environment", func(t *testing.T) {
		got, err := cfg.ResolveSecrets(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if got.ProviderKeys["primary"] != "provider-secret" || got.DevKey != "dev-secret" {
			t.Fatalf("secrets not resolved")
		}
	})
	for _, missing := range []string{"PRIMARY_KEY", "LLMGATE_DEV_KEY"} {
		t.Run("names a missing variable: "+missing, func(t *testing.T) {
			_, err := cfg.ResolveSecrets(func(k string) string {
				if k == missing {
					return ""
				}
				return env[k]
			})
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "secret") && strings.Contains(err.Error(), env[missing]) {
				t.Fatalf("error leaks a value: %v", err)
			}
		})
	}
}

func TestMetricsListener(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML+"metrics:\n  listen: 127.0.0.1:18303\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metrics.Listen != "127.0.0.1:18303" {
		t.Fatalf("metrics = %+v", cfg.Metrics)
	}
	if cfg, err := Load(writeConfig(t, validYAML)); err != nil || cfg.Metrics.Listen != "" {
		t.Fatalf("metrics should be off unless asked for: %+v, %v", cfg, err)
	}
	for name, extra := range map[string]string{
		"public without saying so":  "metrics:\n  listen: 0.0.0.0:18303\n",
		"the gateway's own address": "metrics:\n  listen: 127.0.0.1:18300\n",
		"not an address":            "metrics:\n  listen: nonsense\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, validYAML+extra)); err == nil || !strings.Contains(err.Error(), "metrics") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := Load(writeConfig(t, validYAML+"metrics:\n  listen: 0.0.0.0:18303\n  allow_non_loopback: true\n")); err != nil {
		t.Fatal(err)
	}
}
