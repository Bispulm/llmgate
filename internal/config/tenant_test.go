package config

import (
	"strings"
	"testing"
	"time"
)

const tenantYAML = `
server:
  listen: 0.0.0.0:18300
auth:
  mode: tenant
database:
  url_env: LLMGATE_DATABASE_URL
redis:
  url_env: LLMGATE_REDIS_URL
admin:
  listen: 127.0.0.1:18302
  token_env: LLMGATE_ADMIN_TOKEN
pricing:
  version: test-2026-11
  prices:
    - provider: primary
      model: alpha
      input_usd_per_mtok: "1.25"
      cached_input_usd_per_mtok: "0.125"
      cache_write_usd_per_mtok: "1.5625"
      output_usd_per_mtok: "10"
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

func TestTenantModeLoadsWithDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, tenantYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tenant() {
		t.Fatal("not tenant mode")
	}
	if cfg.Auth.KeyCacheTTL != DefaultKeyCacheTTL || cfg.Accounting.ReconcileInterval != DefaultReconcileInterval ||
		cfg.Accounting.UncertainGrace != DefaultUncertainGrace || cfg.Accounting.RejectedAttemptCharge != RejectedChargeNone {
		t.Fatalf("defaults: %+v %+v", cfg.Auth, cfg.Accounting)
	}
	if cfg.Redis.KeyPrefix != DefaultRedisKeyPrefix {
		t.Fatalf("key_prefix = %q", cfg.Redis.KeyPrefix)
	}
	if cfg.RateLimits.OnRedisFailure != RedisFailureClosed {
		t.Fatalf("on_redis_failure = %q: the default has to be closed", cfg.RateLimits.OnRedisFailure)
	}
	list, err := cfg.PriceList()
	if err != nil {
		t.Fatal(err)
	}
	if list.Label != "test-2026-11" || len(list.Entries) != 1 {
		t.Fatalf("list = %+v", list)
	}
	got := list.Entries[0].Prices
	if got.Input != 1_250_000 || got.CachedInput != 125_000 || got.CacheWrite != 1_562_500 || got.Output != 10_000_000 {
		t.Fatalf("prices = %+v", got)
	}
}

func TestTenantModeSecrets(t *testing.T) {
	cfg, err := Load(writeConfig(t, tenantYAML))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"LLMGATE_DATABASE_URL": "postgres://db", "LLMGATE_REDIS_URL": "redis://r",
		"LLMGATE_ADMIN_TOKEN": "a-long-enough-admin-token", "PRIMARY_KEY": "p",
	}
	secrets, err := cfg.ResolveSecrets(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if secrets.DatabaseURL != "postgres://db" || secrets.RedisURL != "redis://r" || secrets.AdminToken != "a-long-enough-admin-token" || secrets.DevKey != "" {
		t.Fatalf("secrets = %+v", secrets)
	}

	for _, name := range []string{"LLMGATE_DATABASE_URL", "LLMGATE_REDIS_URL", "LLMGATE_ADMIN_TOKEN"} {
		t.Run("missing "+name, func(t *testing.T) {
			_, err := cfg.ResolveSecrets(func(k string) string {
				if k == name {
					return ""
				}
				return env[k]
			})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("a short admin token", func(t *testing.T) {
		_, err := cfg.ResolveSecrets(func(k string) string {
			if k == "LLMGATE_ADMIN_TOKEN" {
				return "short"
			}
			return env[k]
		})
		if err == nil || !strings.Contains(err.Error(), "at least") || strings.Contains(err.Error(), "short") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTenantModeRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name          string
		old, new      string
		wantInMessage string
	}{
		{"a dev key alongside tenant keys", "mode: tenant", "mode: tenant\n  dev_key_env: DEV", "dev_key_env"},
		{"an unknown mode", "mode: tenant", "mode: open", "auth.mode"},
		{"no database", "url_env: LLMGATE_DATABASE_URL", "url_env: ''", "database.url_env"},
		{"no redis", "url_env: LLMGATE_REDIS_URL", "url_env: ''", "redis.url_env"},
		{"no admin token", "token_env: LLMGATE_ADMIN_TOKEN", "token_env: ''", "admin.token_env"},
		{"no admin listener", "listen: 127.0.0.1:18302", "listen: ''", "admin.listen"},
		{"a public admin listener", "listen: 127.0.0.1:18302", "listen: 0.0.0.0:18302", "allow_non_loopback"},
		{"an admin listener on the gateway's port", "listen: 127.0.0.1:18302", "listen: 0.0.0.0:18300\n  allow_non_loopback: true", "same address"},
		{"no price version", "version: test-2026-11", "version: ''", "pricing.version"},
		{"a route with no price", "model: alpha\n      input", "model: other\n      input", "no price"},
		{"a float price", `output_usd_per_mtok: "10"`, `output_usd_per_mtok: 10.5`, "output_usd_per_mtok"},
		{"an inexact price", `output_usd_per_mtok: "10"`, `output_usd_per_mtok: "0.0000001"`, "decimal places"},
		{"a missing price field", `      output_usd_per_mtok: "10"` + "\n", "", "output_usd_per_mtok"},
		{"an unknown redis policy", "auth:\n  mode: tenant", "rate_limits:\n  on_redis_failure: maybe\nauth:\n  mode: tenant", "on_redis_failure"},
		{"an unknown rejected charge", "auth:\n  mode: tenant", "accounting:\n  rejected_attempt_charge: some\nauth:\n  mode: tenant", "rejected_attempt_charge"},
		{"a negative grace", "auth:\n  mode: tenant", "accounting:\n  uncertain_grace: -1s\nauth:\n  mode: tenant", "uncertain_grace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(tenantYAML, tt.old) {
				t.Fatalf("test setup: %q not found", tt.old)
			}
			_, err := Load(writeConfig(t, strings.Replace(tenantYAML, tt.old, tt.new, 1)))
			if err == nil || !strings.Contains(err.Error(), tt.wantInMessage) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantInMessage)
			}
		})
	}
}

func TestTenantModeAllowsAnyFreePortForBoth(t *testing.T) {
	yaml := strings.Replace(strings.Replace(tenantYAML, "listen: 127.0.0.1:18302", "listen: 127.0.0.1:0", 1), "listen: 0.0.0.0:18300", "listen: 127.0.0.1:0", 1)
	if _, err := Load(writeConfig(t, yaml)); err != nil {
		t.Fatal(err)
	}
	same := strings.Replace(strings.Replace(tenantYAML, "listen: 127.0.0.1:18302", "listen: 127.0.0.1:18300", 1), "listen: 0.0.0.0:18300", "listen: 127.0.0.1:18300", 1)
	if _, err := Load(writeConfig(t, same)); err == nil || !strings.Contains(err.Error(), "same address") {
		t.Fatalf("err = %v", err)
	}
}

func TestTenantModeAllowsADeliberatelyPublicAdminListener(t *testing.T) {
	yaml := strings.Replace(tenantYAML, "listen: 127.0.0.1:18302", "listen: 0.0.0.0:18302\n  allow_non_loopback: true", 1)
	if _, err := Load(writeConfig(t, yaml)); err != nil {
		t.Fatal(err)
	}
}

// Development mode can't pick up tenant settings and look protected when it isn't.
func TestDevModeRejectsTenantSettings(t *testing.T) {
	for name, extra := range map[string]string{
		"database": "database:\n  url_env: X\n",
		"pricing":  "pricing:\n  version: v\n",
		"admin":    "admin:\n  listen: 127.0.0.1:18302\n",
		"redis":    "redis:\n  url_env: X\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, validYAML+extra))
			if err == nil || !strings.Contains(err.Error(), "tenant") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tenant() {
		t.Fatal("tenant mode without asking for it")
	}
	_ = time.Second
}

func TestTheTenantExampleLoads(t *testing.T) {
	if _, err := Load("../../configs/tenant.yaml"); err != nil {
		t.Fatal(err)
	}
}

func TestCacheSettings(t *testing.T) {
	withCache := strings.Replace(tenantYAML, "max_output_tokens: 1024", "max_output_tokens: 1024\n    cache:\n      ttl: 10m", 1)
	withCache = strings.Replace(withCache, "auth:\n  mode: tenant", "cache:\n  max_entry_bytes: 65536\n  redis_url_env: CACHE_REDIS\nauth:\n  mode: tenant", 1)
	cfg, err := Load(writeConfig(t, withCache))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models[0].Cache.TTL != 10*time.Minute || cfg.Cache.MaxEntryBytes != 65536 {
		t.Fatalf("cache = %+v %+v", cfg.Models[0].Cache, cfg.Cache)
	}
	env := map[string]string{"LLMGATE_DATABASE_URL": "db", "LLMGATE_REDIS_URL": "limits", "CACHE_REDIS": "cache", "LLMGATE_ADMIN_TOKEN": "a-long-enough-admin-token", "PRIMARY_KEY": "p"}
	secrets, err := cfg.ResolveSecrets(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if secrets.CacheRedisURL != "cache" {
		t.Fatalf("cache redis = %q", secrets.CacheRedisURL)
	}

	// Without its own Redis, the cache shares the one for rate limits.
	plain, err := Load(writeConfig(t, tenantYAML))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err = plain.ResolveSecrets(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if secrets.CacheRedisURL != "limits" || plain.Cache.MaxEntryBytes != DefaultMaxCacheEntryBytes {
		t.Fatalf("cache redis = %q, max entry = %d", secrets.CacheRedisURL, plain.Cache.MaxEntryBytes)
	}

	for name, bad := range map[string]string{
		"a negative TTL": strings.Replace(tenantYAML, "max_output_tokens: 1024", "max_output_tokens: 1024\n    cache:\n      ttl: -1s", 1),
		"a TTL over 24h": strings.Replace(tenantYAML, "max_output_tokens: 1024", "max_output_tokens: 1024\n    cache:\n      ttl: 25h", 1),
		"a huge entry":   strings.Replace(tenantYAML, "auth:\n  mode: tenant", "cache:\n  max_entry_bytes: 999999999\nauth:\n  mode: tenant", 1),
		"caching in dev": strings.Replace(validYAML, "max_output_tokens: 1024", "max_output_tokens: 1024\n    cache:\n      ttl: 1m", 1),
		"a cache in dev": validYAML + "cache:\n  max_entry_bytes: 1024\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, bad)); err == nil || !strings.Contains(err.Error(), "cache") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestTheComposeConfigLoads(t *testing.T) {
	if _, err := Load("../../configs/compose.yaml"); err != nil {
		t.Fatal(err)
	}
}
