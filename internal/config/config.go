// Package config loads and validates the gateway's configuration file.
//
// Configuration never holds a credential. It names environment variables, which
// are read once at startup by ResolveSecrets.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Bispulm/llmgate/internal/accounting"
)

// Provider types.
const (
	ProviderOpenAICompatible = "openai-compatible"
	ProviderAnthropic        = "anthropic"
)

// Upstream names for the output limit parameter. OpenAI deprecated max_tokens in
// favor of max_completion_tokens, but some compatible servers only accept the
// older name.
const (
	MaxCompletionTokens = "max_completion_tokens"
	MaxTokens           = "max_tokens"
)

// Defaults for limits not set in the file.
const (
	DefaultMaxRequestBytes    int64 = 4 << 20
	DefaultMaxResponseBytes   int64 = 16 << 20
	DefaultMaxEventBytes            = 1 << 20
	DefaultRequestTimeout           = 120 * time.Second
	DefaultFirstEventTimeout        = 30 * time.Second
	DefaultStreamIdleTimeout        = 30 * time.Second
	DefaultClientWriteTimeout       = 10 * time.Second
	DefaultBodyReadTimeout          = 10 * time.Second
	DefaultShutdownTimeout          = 30 * time.Second
	DefaultQueueTimeout             = 5 * time.Second
	DefaultAdmissionTimeout         = 5 * time.Second
	DefaultKeyCacheTTL              = 30 * time.Second
	DefaultReconcileInterval        = 15 * time.Second
	DefaultUncertainGrace           = 10 * time.Minute
	DefaultDatabaseMaxConns         = 20
	DefaultRedisKeyPrefix           = "llmgate:"
	DefaultMaxCacheEntryBytes       = 256 << 10

	// MaxCacheTTL and MaxCacheEntryBytes bound what one cached answer can occupy.
	MaxCacheTTL        = 24 * time.Hour
	MaxCacheEntryBytes = 4 << 20

	// MaxKeyCacheTTL bounds how long a revoked key can keep working on another replica.
	MaxKeyCacheTTL = 5 * time.Minute
	// MinAdminTokenLength keeps the admin token from being guessable.
	MinAdminTokenLength = 16
)

// Authentication modes.
const (
	// AuthDev accepts one shared key, enforces no budget, and only listens on loopback.
	AuthDev = "dev"
	// AuthTenant uses virtual keys, budgets, and team limits, backed by Postgres and Redis.
	AuthTenant = "tenant"
)

// What team limits do when Redis can't be reached. Budgets don't depend on Redis.
const (
	RedisFailureClosed = "closed"
	RedisFailureOpen   = "open"
)

// How to charge an attempt the provider refused, with no usage report.
const (
	RejectedChargeNone         = "none"
	RejectedChargeConservative = "conservative"
)

// Config is the validated contents of a configuration file.
type Config struct {
	Server         Server         `yaml:"server"`
	Auth           Auth           `yaml:"auth"`
	Database       Database       `yaml:"database"`
	Redis          Redis          `yaml:"redis"`
	RateLimits     RateLimits     `yaml:"rate_limits"`
	Admin          Admin          `yaml:"admin"`
	Pricing        Pricing        `yaml:"pricing"`
	Accounting     Accounting     `yaml:"accounting"`
	Cache          Cache          `yaml:"cache"`
	Metrics        Metrics        `yaml:"metrics"`
	Tracing        Tracing        `yaml:"tracing"`
	Limits         Limits         `yaml:"limits"`
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
	Providers      []Provider     `yaml:"providers"`
	Models         []Model        `yaml:"models"`
}

// CircuitBreaker configures every route's breaker. Each route has its own, in each
// process. Zero values take the defaults in the routing package.
type CircuitBreaker struct {
	Window         time.Duration `yaml:"window"`
	MinRequests    int           `yaml:"min_requests"`
	ErrorRate      float64       `yaml:"error_rate"`
	Cooldown       time.Duration `yaml:"cooldown"`
	HalfOpenProbes int           `yaml:"half_open_probes"`
}

// Server configures the client-facing listener.
type Server struct {
	Listen          string        `yaml:"listen"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// DrainDelay is how long a stopping replica keeps serving while reporting that it
	// isn't ready, so that a load balancer stops sending it new connections first.
	DrainDelay time.Duration `yaml:"drain_delay"`
}

// Auth configures client authentication. Development mode accepts one shared key and
// enforces no budget, which is why it only accepts loopback listeners.
type Auth struct {
	Mode      string `yaml:"mode"`
	DevKeyEnv string `yaml:"dev_key_env"`
	// KeyCacheTTL is the most a revoked key, or a team's old limits, can keep working.
	KeyCacheTTL time.Duration `yaml:"key_cache_ttl"`
}

// Database is the Postgres that holds teams, keys, and the ledger.
type Database struct {
	URLEnv   string `yaml:"url_env"`
	MaxConns int32  `yaml:"max_conns"`
}

// Redis holds team rate limits.
type Redis struct {
	URLEnv string `yaml:"url_env"`
	// KeyPrefix namespaces the gateway's keys, so that environments can share a Redis.
	KeyPrefix string `yaml:"key_prefix"`
}

// RateLimits configures team limits.
type RateLimits struct {
	// OnRedisFailure is closed, which refuses requests, or open, which falls back to a
	// limit in each replica. Budgets stay enforced either way.
	OnRedisFailure string `yaml:"on_redis_failure"`
}

// Admin configures the admin API, which has its own listener and token.
type Admin struct {
	Listen   string `yaml:"listen"`
	TokenEnv string `yaml:"token_env"`
	// AllowNonLoopback has to be set to serve the admin API beyond this machine.
	AllowNonLoopback bool `yaml:"allow_non_loopback"`
}

// Pricing is a labeled, immutable price list. Changing a price means a new version.
type Pricing struct {
	Version string  `yaml:"version"`
	Prices  []Price `yaml:"prices"`
}

// Price is one route's rates, as quoted decimal strings of USD per million tokens.
type Price struct {
	Provider    string  `yaml:"provider"`
	Model       string  `yaml:"model"`
	Input       Decimal `yaml:"input_usd_per_mtok"`
	CachedInput Decimal `yaml:"cached_input_usd_per_mtok"`
	CacheWrite  Decimal `yaml:"cache_write_usd_per_mtok"`
	Output      Decimal `yaml:"output_usd_per_mtok"`
}

// Decimal is a number written as a quoted string. An unquoted number is rejected,
// because YAML reads it as a float, which can't hold most prices exactly.
type Decimal struct {
	Value  string
	quoted bool
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Decimal) UnmarshalYAML(node *yaml.Node) error {
	d.Value = node.Value
	d.quoted = node.Kind == yaml.ScalarNode && node.Tag == "!!str"
	return nil
}

// Metrics configures the Prometheus listener, which is off unless listen is set. It has
// no authentication, so it stays on loopback unless allow_non_loopback is set.
type Metrics struct {
	Listen           string `yaml:"listen"`
	AllowNonLoopback bool   `yaml:"allow_non_loopback"`
}

// Tracing exports OpenTelemetry traces over OTLP/HTTP, and is off unless an endpoint is
// set. Spans carry routing and accounting decisions, never request or response content.
type Tracing struct {
	OTLPEndpoint string `yaml:"otlp_endpoint"`
	// SampleRatio samples new traces. A caller's decision is always kept. Unset means 1,
	// and 0 means only what callers have chosen to sample.
	SampleRatio *float64 `yaml:"sample_ratio"`
}

// Ratio returns the sample ratio, 1 when unset.
func (t Tracing) Ratio() float64 {
	if t.SampleRatio == nil {
		return 1
	}
	return *t.SampleRatio
}

// Cache configures the response cache. Aliases opt in with their own TTL.
type Cache struct {
	// RedisURLEnv names a separate Redis for cached answers, so that memory pressure from
	// the cache can't evict rate-limit state. Without it the cache shares that Redis.
	RedisURLEnv   string `yaml:"redis_url_env"`
	MaxEntryBytes int    `yaml:"max_entry_bytes"`
}

// ModelCache lets clients opt in to cached answers for an alias.
type ModelCache struct {
	TTL time.Duration `yaml:"ttl"`
}

// Accounting configures settlement and reconciliation.
type Accounting struct {
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// UncertainGrace is how long an attempt of unknown cost waits for usage to arrive,
	// before being charged everything it reserved.
	UncertainGrace time.Duration `yaml:"uncertain_grace"`
	// RejectedAttemptCharge is none or conservative, for an attempt the provider refused.
	RejectedAttemptCharge string `yaml:"rejected_attempt_charge"`
}

// Limits bounds what one request can consume.
type Limits struct {
	MaxRequestBytes    int64         `yaml:"max_request_bytes"`
	MaxResponseBytes   int64         `yaml:"max_response_bytes"`
	MaxEventBytes      int           `yaml:"max_event_bytes"`
	RequestTimeout     time.Duration `yaml:"request_timeout"`
	FirstEventTimeout  time.Duration `yaml:"first_event_timeout"`
	StreamIdleTimeout  time.Duration `yaml:"stream_idle_timeout"`
	CompletionTimeout  time.Duration `yaml:"completion_timeout"`
	ClientWriteTimeout time.Duration `yaml:"client_write_timeout"`
	BodyReadTimeout    time.Duration `yaml:"body_read_timeout"`
	// AdmissionTimeout bounds each Postgres check before anything is sent, so that a
	// database that has hung refuses requests quickly.
	AdmissionTimeout time.Duration `yaml:"admission_timeout"`
}

// Provider is one upstream API.
type Provider struct {
	Name           string `yaml:"name"`
	Type           string `yaml:"type"`
	BaseURL        string `yaml:"base_url"`
	APIKeyEnv      string `yaml:"api_key_env"`
	MaxTokensParam string `yaml:"max_tokens_param"`
	// AllowInsecureHTTP permits plaintext to a host other than loopback, such as a mock
	// on a private container network. The provider credential travels in every request.
	AllowInsecureHTTP bool `yaml:"allow_insecure_http"`

	// MaxConcurrency limits attempts running at once on this provider, in each
	// process. Zero means no limit. The others only apply when it's set.
	MaxConcurrency     int `yaml:"max_concurrency"`
	MaxQueue           int `yaml:"max_queue"`
	MaxQueuePerTeam    int `yaml:"max_queue_per_team"`
	MaxInFlightPerTeam int `yaml:"max_in_flight_per_team"`
	// QueueTimeout is how long a request waits before trying another route.
	QueueTimeout time.Duration `yaml:"queue_timeout"`
}

// Model is a client-visible alias and the routes that can serve it.
type Model struct {
	Alias           string `yaml:"alias"`
	MaxOutputTokens int64  `yaml:"max_output_tokens"`
	// MaxAttempts bounds upstream attempts per request. Zero means one per route.
	MaxAttempts int        `yaml:"max_attempts"`
	Routes      []Route    `yaml:"routes"`
	Cache       ModelCache `yaml:"cache"`
}

// Route points an alias at a model on a provider.
type Route struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// Tier orders fallback. Lower tiers are tried first.
	Tier int `yaml:"tier"`
	// Weight shares traffic within a tier. Zero means 1.
	Weight int `yaml:"weight"`
}

// maxAttemptsLimit keeps a configuration from allowing unbounded retries.
const maxAttemptsLimit = 10

// Secrets holds credentials read from the environment. It is kept apart from
// Config so that a configuration value can be logged without risk.
type Secrets struct {
	DevKey       string
	ProviderKeys map[string]string
	DatabaseURL  string
	RedisURL     string
	AdminToken   string
	// CacheRedisURL is the cache's own Redis, or RedisURL if it has none.
	CacheRedisURL string
}

// Tenant reports whether this configuration uses virtual keys and budgets.
func (c *Config) Tenant() bool { return c.Auth.Mode == AuthTenant }

// Load reads and validates a configuration file. Unknown fields are errors, so
// a typo can't silently leave a limit at its default.
func Load(path string) (*Config, error) {
	file, err := os.Open(path) //nolint:gosec // The path comes from the operator's command line.
	if err != nil {
		return nil, fmt.Errorf("opening config: %w", err)
	}
	defer func() { _ = file.Close() }()

	var cfg Config
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.validateMode(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

// ResolveSecrets reads every configured credential using getenv, normally
// os.Getenv. It fails if any is unset, so a gateway can't start half configured.
func (c *Config) ResolveSecrets(getenv func(string) string) (*Secrets, error) {
	secrets := &Secrets{ProviderKeys: make(map[string]string, len(c.Providers))}
	required := []struct {
		name  string
		value *string
	}{{c.Auth.DevKeyEnv, &secrets.DevKey}}
	if c.Tenant() {
		required = []struct {
			name  string
			value *string
		}{{c.Database.URLEnv, &secrets.DatabaseURL}, {c.Redis.URLEnv, &secrets.RedisURL}, {c.Admin.TokenEnv, &secrets.AdminToken}}
	}
	for _, r := range required {
		if *r.value = getenv(r.name); *r.value == "" {
			return nil, fmt.Errorf("environment variable %s is not set", r.name)
		}
	}
	if c.Tenant() {
		secrets.CacheRedisURL = secrets.RedisURL
		if c.Cache.RedisURLEnv != "" {
			if secrets.CacheRedisURL = getenv(c.Cache.RedisURLEnv); secrets.CacheRedisURL == "" {
				return nil, fmt.Errorf("environment variable %s is not set", c.Cache.RedisURLEnv)
			}
		}
	}
	if c.Tenant() && len(secrets.AdminToken) < MinAdminTokenLength {
		return nil, fmt.Errorf("environment variable %s must hold at least %d characters", c.Admin.TokenEnv, MinAdminTokenLength)
	}
	for _, p := range c.Providers {
		key := getenv(p.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("environment variable %s for provider %q is not set", p.APIKeyEnv, p.Name)
		}
		secrets.ProviderKeys[p.Name] = key
	}
	return secrets, nil
}

// validateMode runs before defaults, so that it can see which settings were written.
func (c *Config) validateMode() error {
	setDefault(&c.Auth.Mode, AuthDev)
	switch c.Auth.Mode {
	case AuthDev:
		// Settings like these would look like protection that development mode doesn't give.
		if c.Database != (Database{}) || c.Redis != (Redis{}) || c.Admin != (Admin{}) || c.Pricing.Version != "" || len(c.Pricing.Prices) > 0 {
			return errors.New("database, redis, admin, and pricing need auth.mode: tenant")
		}
		if c.Cache != (Cache{}) {
			return errors.New("the cache needs auth.mode: tenant")
		}
		for _, m := range c.Models {
			if m.Cache != (ModelCache{}) {
				return fmt.Errorf("model %q: the cache needs auth.mode: tenant", m.Alias)
			}
		}
	case AuthTenant:
		if c.Auth.DevKeyEnv != "" {
			return errors.New("auth.dev_key_env can't be used with auth.mode: tenant")
		}
	default:
		return fmt.Errorf("auth.mode must be %s or %s", AuthDev, AuthTenant)
	}
	return nil
}

func (c *Config) applyDefaults() {
	setDefault(&c.Server.ShutdownTimeout, DefaultShutdownTimeout)
	if c.Tenant() {
		setDefault(&c.Auth.KeyCacheTTL, DefaultKeyCacheTTL)
		setDefault(&c.Database.MaxConns, DefaultDatabaseMaxConns)
		setDefault(&c.Redis.KeyPrefix, DefaultRedisKeyPrefix)
		setDefault(&c.RateLimits.OnRedisFailure, RedisFailureClosed)
		setDefault(&c.Accounting.ReconcileInterval, DefaultReconcileInterval)
		setDefault(&c.Accounting.UncertainGrace, DefaultUncertainGrace)
		setDefault(&c.Accounting.RejectedAttemptCharge, RejectedChargeNone)
		setDefault(&c.Cache.MaxEntryBytes, DefaultMaxCacheEntryBytes)
	}
	setDefault(&c.Limits.MaxRequestBytes, DefaultMaxRequestBytes)
	setDefault(&c.Limits.MaxResponseBytes, DefaultMaxResponseBytes)
	setDefault(&c.Limits.MaxEventBytes, DefaultMaxEventBytes)
	setDefault(&c.Limits.RequestTimeout, DefaultRequestTimeout)
	// These stay within the request's deadline, or they could never fire. Half of it
	// leaves a hung non-streaming attempt time to fall back.
	setDefault(&c.Limits.FirstEventTimeout, min(DefaultFirstEventTimeout, c.Limits.RequestTimeout))
	setDefault(&c.Limits.CompletionTimeout, c.Limits.RequestTimeout/2)
	setDefault(&c.Limits.StreamIdleTimeout, DefaultStreamIdleTimeout)
	setDefault(&c.Limits.ClientWriteTimeout, DefaultClientWriteTimeout)
	setDefault(&c.Limits.BodyReadTimeout, DefaultBodyReadTimeout)
	setDefault(&c.Limits.AdmissionTimeout, min(DefaultAdmissionTimeout, c.Limits.RequestTimeout))
	for i := range c.Providers {
		setDefault(&c.Providers[i].MaxTokensParam, MaxCompletionTokens)
		setDefault(&c.Providers[i].QueueTimeout, DefaultQueueTimeout)
	}
}

func setDefault[T comparable](field *T, value T) {
	var zero T
	if *field == zero {
		*field = value
	}
}

func (c *Config) validate() error {
	if err := validateListen(c.Server.Listen, c.Tenant()); err != nil {
		return err
	}
	if c.Tenant() {
		if err := c.validateTenant(); err != nil {
			return err
		}
	} else if c.Auth.DevKeyEnv == "" {
		return errors.New("auth.dev_key_env is required")
	}
	if err := c.validateMetrics(); err != nil {
		return err
	}
	if err := c.Tracing.validate(); err != nil {
		return err
	}
	if err := c.Limits.validate(); err != nil {
		return err
	}
	if err := c.CircuitBreaker.validate(); err != nil {
		return err
	}
	if c.Server.ShutdownTimeout < 0 {
		return errors.New("server.shutdown_timeout must not be negative")
	}
	if c.Server.DrainDelay < 0 || c.Server.DrainDelay > time.Minute {
		return errors.New("server.drain_delay must be between 0 and 1m")
	}
	if len(c.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	providers := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d].name is required", i)
		}
		if providers[p.Name] {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
		providers[p.Name] = true
		if err := p.validate(); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
	}
	if len(c.Models) == 0 {
		return errors.New("at least one model alias is required")
	}
	aliases := make(map[string]bool, len(c.Models))
	for i, m := range c.Models {
		if m.Alias == "" {
			return fmt.Errorf("models[%d].alias is required", i)
		}
		if aliases[m.Alias] {
			return fmt.Errorf("duplicate model alias %q", m.Alias)
		}
		aliases[m.Alias] = true
		if err := m.validate(providers); err != nil {
			return fmt.Errorf("model %q: %w", m.Alias, err)
		}
	}
	return nil
}

// validateListen allows loopback addresses only in development mode, because a shared
// development key must not be reachable from a network.
func validateListen(listen string, tenant bool) error {
	if listen == "" {
		return errors.New("server.listen is required")
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("server.listen: %w", err)
	}
	if !tenant && !isLoopback(host) {
		return fmt.Errorf("server.listen %q must be a loopback address unless auth.mode is tenant", listen)
	}
	return nil
}

func (c *Config) validateTenant() error {
	switch {
	case c.Database.URLEnv == "":
		return errors.New("database.url_env is required")
	case c.Database.MaxConns < 0:
		return errors.New("database.max_conns must not be negative")
	case c.Redis.URLEnv == "":
		return errors.New("redis.url_env is required")
	case c.Admin.TokenEnv == "":
		return errors.New("admin.token_env is required")
	case c.Auth.KeyCacheTTL < 0 || c.Auth.KeyCacheTTL > MaxKeyCacheTTL:
		return fmt.Errorf("auth.key_cache_ttl must be between 0 and %v, since it bounds revocation", MaxKeyCacheTTL)
	case c.Accounting.ReconcileInterval < 0:
		return errors.New("accounting.reconcile_interval must not be negative")
	case c.Accounting.UncertainGrace < 0:
		return errors.New("accounting.uncertain_grace must not be negative")
	}
	switch c.RateLimits.OnRedisFailure {
	case RedisFailureClosed, RedisFailureOpen:
	default:
		return fmt.Errorf("rate_limits.on_redis_failure must be %s or %s", RedisFailureClosed, RedisFailureOpen)
	}
	switch c.Accounting.RejectedAttemptCharge {
	case RejectedChargeNone, RejectedChargeConservative:
	default:
		return fmt.Errorf("accounting.rejected_attempt_charge must be %s or %s", RejectedChargeNone, RejectedChargeConservative)
	}
	if c.Cache.MaxEntryBytes < 0 || c.Cache.MaxEntryBytes > MaxCacheEntryBytes {
		return fmt.Errorf("cache.max_entry_bytes must be between 0 and %d", MaxCacheEntryBytes)
	}
	if err := c.validateAdminListen(); err != nil {
		return err
	}
	if c.Pricing.Version == "" {
		return errors.New("pricing.version is required")
	}
	if _, err := c.PriceList(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateAdminListen() error {
	if c.Admin.Listen == "" {
		return errors.New("admin.listen is required")
	}
	host, _, err := net.SplitHostPort(c.Admin.Listen)
	if err != nil {
		return fmt.Errorf("admin.listen: %w", err)
	}
	if !isLoopback(host) && !c.Admin.AllowNonLoopback {
		return fmt.Errorf("admin.listen %q is not loopback, which needs admin.allow_non_loopback: true", c.Admin.Listen)
	}
	// Port 0 asks for any free port, so two of them never collide.
	_, adminPort, _ := net.SplitHostPort(c.Admin.Listen)
	serverHost, serverPort, _ := net.SplitHostPort(c.Server.Listen)
	if adminPort != "0" && adminPort == serverPort && (host == serverHost || isUnspecified(host) || isUnspecified(serverHost)) {
		return errors.New("admin.listen and server.listen can't be the same address")
	}
	return nil
}

func (c *Config) validateMetrics() error {
	if c.Metrics.Listen == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(c.Metrics.Listen)
	if err != nil {
		return fmt.Errorf("metrics.listen: %w", err)
	}
	if !isLoopback(host) && !c.Metrics.AllowNonLoopback {
		return fmt.Errorf("metrics.listen %q is not loopback, which needs metrics.allow_non_loopback: true", c.Metrics.Listen)
	}
	for _, other := range []string{c.Server.Listen, c.Admin.Listen} {
		otherHost, otherPort, err := net.SplitHostPort(other)
		if err != nil || port == "0" {
			continue
		}
		if port == otherPort && (host == otherHost || isUnspecified(host) || isUnspecified(otherHost)) {
			return fmt.Errorf("metrics.listen can't share an address with another listener")
		}
	}
	return nil
}

// validate refuses plaintext to anything but loopback, since spans name teams and routes.
func (t Tracing) validate() error {
	if t.OTLPEndpoint == "" {
		if t.SampleRatio != nil {
			return errors.New("tracing.sample_ratio needs tracing.otlp_endpoint")
		}
		return nil
	}
	if t.Ratio() < 0 || t.Ratio() > 1 {
		return errors.New("tracing.sample_ratio must be between 0 and 1")
	}
	u, err := url.Parse(t.OTLPEndpoint)
	if err != nil || u.Host == "" {
		return errors.New("tracing.otlp_endpoint must be a URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return errors.New("tracing.otlp_endpoint must use https for a host other than loopback")
	}
	return nil
}

func isUnspecified(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// PriceList converts the configured prices, and checks that every route has exactly one.
// A route without a price couldn't have its cost bounded, so it's a startup error rather
// than something discovered on a request.
func (c *Config) PriceList() (accounting.PriceList, error) {
	list := accounting.PriceList{Label: c.Pricing.Version}
	priced := make(map[[2]string]bool, len(c.Pricing.Prices))
	for i, p := range c.Pricing.Prices {
		key := [2]string{p.Provider, p.Model}
		if p.Provider == "" || p.Model == "" {
			return list, fmt.Errorf("pricing.prices[%d] needs a provider and a model", i)
		}
		if priced[key] {
			return list, fmt.Errorf("pricing.prices[%d] duplicates an earlier price", i)
		}
		priced[key] = true
		entry := accounting.PriceEntry{Provider: p.Provider, Model: p.Model}
		fields := []struct {
			name  string
			value Decimal
			into  *int64
		}{
			{"input_usd_per_mtok", p.Input, &entry.Prices.Input},
			{"cached_input_usd_per_mtok", p.CachedInput, &entry.Prices.CachedInput},
			{"cache_write_usd_per_mtok", p.CacheWrite, &entry.Prices.CacheWrite},
			{"output_usd_per_mtok", p.Output, &entry.Prices.Output},
		}
		for _, f := range fields {
			if f.value.Value == "" {
				return list, fmt.Errorf("pricing.prices[%d].%s is required", i, f.name)
			}
			if !f.value.quoted {
				return list, fmt.Errorf("pricing.prices[%d].%s must be a quoted string, like \"1.25\"", i, f.name)
			}
			pico, err := accounting.ParseUSDPerMTok(f.value.Value)
			if err != nil {
				return list, fmt.Errorf("pricing.prices[%d].%s: %w", i, f.name, err)
			}
			*f.into = pico
		}
		list.Entries = append(list.Entries, entry)
	}
	for _, m := range c.Models {
		for _, r := range m.Routes {
			if !priced[[2]string{r.Provider, r.Model}] {
				return list, fmt.Errorf("model %q: the route to %s/%s has no price", m.Alias, r.Provider, r.Model)
			}
		}
	}
	return list, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (l Limits) validate() error {
	sizes := []struct {
		name  string
		value int64
	}{
		{"max_request_bytes", l.MaxRequestBytes},
		{"max_response_bytes", l.MaxResponseBytes},
		{"max_event_bytes", int64(l.MaxEventBytes)},
	}
	for _, s := range sizes {
		if s.value < 0 {
			return fmt.Errorf("limits.%s must be positive", s.name)
		}
	}
	timeouts := []struct {
		name  string
		value time.Duration
	}{
		{"request_timeout", l.RequestTimeout},
		{"first_event_timeout", l.FirstEventTimeout},
		{"completion_timeout", l.CompletionTimeout},
		{"stream_idle_timeout", l.StreamIdleTimeout},
		{"client_write_timeout", l.ClientWriteTimeout},
		{"body_read_timeout", l.BodyReadTimeout},
		{"admission_timeout", l.AdmissionTimeout},
	}
	for _, t := range timeouts {
		if t.value < 0 {
			return fmt.Errorf("limits.%s must be positive", t.name)
		}
	}
	// An attempt's own timers are what tell a hung provider from a long request. Past the
	// request's deadline they could never fire, so a hung provider would never count.
	if l.FirstEventTimeout > l.RequestTimeout {
		return errors.New("limits.first_event_timeout must not exceed limits.request_timeout")
	}
	if l.CompletionTimeout > l.RequestTimeout {
		return errors.New("limits.completion_timeout must not exceed limits.request_timeout")
	}
	if l.AdmissionTimeout > l.RequestTimeout {
		return errors.New("limits.admission_timeout must not exceed limits.request_timeout")
	}
	return nil
}

func (p Provider) validate() error {
	switch p.Type {
	case ProviderOpenAICompatible, ProviderAnthropic:
	default:
		return fmt.Errorf("unknown type %q", p.Type)
	}
	if p.APIKeyEnv == "" {
		return errors.New("api_key_env is required")
	}
	switch p.MaxTokensParam {
	case MaxCompletionTokens, MaxTokens:
	default:
		return fmt.Errorf("unknown max_tokens_param %q", p.MaxTokensParam)
	}
	if err := p.validateConcurrency(); err != nil {
		return err
	}
	return validateBaseURL(p.BaseURL, p.AllowInsecureHTTP)
}

func (p Provider) validateConcurrency() error {
	if p.MaxConcurrency < 0 || p.MaxQueue < 0 || p.MaxQueuePerTeam < 0 || p.MaxInFlightPerTeam < 0 || p.QueueTimeout < 0 {
		return errors.New("concurrency settings must not be negative")
	}
	// Without this, these would look like protection while doing nothing.
	if p.MaxConcurrency == 0 && (p.MaxQueue > 0 || p.MaxQueuePerTeam > 0 || p.MaxInFlightPerTeam > 0) {
		return errors.New("queue and per-team limits need max_concurrency to be set")
	}
	return nil
}

// validateBaseURL refuses plaintext to anything but loopback, because the
// provider credential travels in every request.
func validateBaseURL(raw string, allowInsecure bool) error {
	if raw == "" {
		return errors.New("base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("base_url is not a valid URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) && !allowInsecure {
			return errors.New("base_url must use https for a host other than loopback, unless allow_insecure_http is set")
		}
	default:
		return errors.New("base_url must use http or https")
	}
	if u.Host == "" {
		return errors.New("base_url has no host")
	}
	return nil
}

func (m Model) validate(providers map[string]bool) error {
	if m.MaxOutputTokens <= 0 {
		return errors.New("max_output_tokens must be positive")
	}
	if len(m.Routes) == 0 {
		return errors.New("at least one route is required")
	}
	if m.Cache.TTL < 0 || m.Cache.TTL > MaxCacheTTL {
		return fmt.Errorf("cache.ttl must be between 0 and %v", MaxCacheTTL)
	}
	if m.MaxAttempts < 0 || m.MaxAttempts > maxAttemptsLimit {
		return fmt.Errorf("max_attempts must be between 0 and %d", maxAttemptsLimit)
	}
	seen := make(map[[2]string]bool, len(m.Routes))
	for i, r := range m.Routes {
		if !providers[r.Provider] {
			return fmt.Errorf("routes[%d] uses unknown provider %q", i, r.Provider)
		}
		if r.Model == "" {
			return fmt.Errorf("routes[%d].model is required", i)
		}
		if r.Tier < 0 {
			return fmt.Errorf("routes[%d].tier must not be negative", i)
		}
		if r.Weight < 0 {
			return fmt.Errorf("routes[%d].weight must not be negative", i)
		}
		key := [2]string{r.Provider, r.Model}
		if seen[key] {
			return fmt.Errorf("routes[%d] duplicates an earlier route", i)
		}
		seen[key] = true
	}
	return nil
}

func (b CircuitBreaker) validate() error {
	if b.Window < 0 || b.Cooldown < 0 {
		return errors.New("circuit_breaker durations must not be negative")
	}
	if b.MinRequests < 0 || b.HalfOpenProbes < 0 {
		return errors.New("circuit_breaker counts must not be negative")
	}
	if b.ErrorRate < 0 || b.ErrorRate > 1 {
		return errors.New("circuit_breaker.error_rate must be between 0 and 1")
	}
	return nil
}
