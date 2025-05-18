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
)

// Config is the validated contents of a configuration file.
type Config struct {
	Server         Server         `yaml:"server"`
	Auth           Auth           `yaml:"auth"`
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
}

// Auth configures client authentication. Until tenant keys exist this is one
// development key, which is why Load only accepts loopback listeners.
type Auth struct {
	DevKeyEnv string `yaml:"dev_key_env"`
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
}

// Provider is one upstream API.
type Provider struct {
	Name           string `yaml:"name"`
	Type           string `yaml:"type"`
	BaseURL        string `yaml:"base_url"`
	APIKeyEnv      string `yaml:"api_key_env"`
	MaxTokensParam string `yaml:"max_tokens_param"`

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
	MaxAttempts int     `yaml:"max_attempts"`
	Routes      []Route `yaml:"routes"`
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
}

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
	if secrets.DevKey = getenv(c.Auth.DevKeyEnv); secrets.DevKey == "" {
		return nil, fmt.Errorf("environment variable %s is not set", c.Auth.DevKeyEnv)
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

func (c *Config) applyDefaults() {
	setDefault(&c.Server.ShutdownTimeout, DefaultShutdownTimeout)
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
	if err := validateListen(c.Server.Listen); err != nil {
		return err
	}
	if c.Auth.DevKeyEnv == "" {
		return errors.New("auth.dev_key_env is required")
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

// validateListen allows loopback addresses only. The only client authentication
// so far is a shared development key, which must not be reachable from a network.
func validateListen(listen string) error {
	if listen == "" {
		return errors.New("server.listen is required")
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("server.listen: %w", err)
	}
	if !isLoopback(host) {
		return fmt.Errorf("server.listen %q must be a loopback address until tenant authentication exists", listen)
	}
	return nil
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
	return validateBaseURL(p.BaseURL)
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
func validateBaseURL(raw string) error {
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
		if !isLoopback(u.Hostname()) {
			return errors.New("base_url must use https for a host other than loopback")
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
