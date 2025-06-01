// Command llmgate runs the gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/api"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/config"
	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/provider/anthropic"
	"github.com/Bispulm/llmgate/internal/provider/openai"
	"github.com/Bispulm/llmgate/internal/routing"
)

const (
	usage = "usage: llmgate serve -config <path>"

	// requestExitTimeout bounds the wait for canceled requests to finish recording.
	requestExitTimeout = 10 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "llmgate:", err)
		os.Exit(1)
	}
}

// run executes one command. getenv and stderr are parameters so tests don't
// depend on the process environment. The optional started callback receives the
// bound address.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, started func(addr string)) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], getenv, stderr, started)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func serve(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, started func(addr string)) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the configuration file")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if *configPath == "" {
		return errors.New("serve needs -config")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	secrets, err := cfg.ResolveSecrets(getenv)
	if err != nil {
		return err
	}
	routes, err := buildRoutes(cfg, secrets)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	logger.Warn("budgets are not enforced and usage is only logged: this build is for local development")
	handler := api.New(api.Options{
		Logger:   logger,
		Auth:     auth.NewDevKey(secrets.DevKey),
		Routes:   routes,
		Recorder: accounting.NewLogRecorder(logger),
		Limits: api.Limits{
			MaxRequestBytes:    cfg.Limits.MaxRequestBytes,
			RequestTimeout:     cfg.Limits.RequestTimeout,
			FirstEventTimeout:  cfg.Limits.FirstEventTimeout,
			StreamIdleTimeout:  cfg.Limits.StreamIdleTimeout,
			CompletionTimeout:  cfg.Limits.CompletionTimeout,
			ClientWriteTimeout: cfg.Limits.ClientWriteTimeout,
			BodyReadTimeout:    cfg.Limits.BodyReadTimeout,
		},
	})

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}

	// Canceling this context ends in-flight requests, and with them their upstream
	// calls. It is separate from ctx so that a shutdown signal lets them drain first.
	baseCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		// WriteTimeout stays unset: it would cut off long streams. Each write gets
		// its own deadline instead.
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	logger.Info("gateway listening", "addr", listener.Addr().String())
	if started != nil {
		started(listener.Addr().String())
	}

	select {
	case err := <-serveErr:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "drain_timeout", cfg.Server.ShutdownTimeout.String())
	handler.SetDraining()

	drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.ShutdownTimeout)
	defer cancelDrain()
	if err := server.Shutdown(drainCtx); err != nil {
		logger.Warn("drain timed out, closing remaining requests", "error", err.Error())
		cancelRequests()
		_ = server.Close()

		// Close doesn't wait for handlers, and each one records its attempt as it
		// finishes. Exiting now would lose those records.
		exitCtx, cancelExit := context.WithTimeout(context.WithoutCancel(ctx), requestExitTimeout)
		defer cancelExit()
		if err := handler.WaitForRequests(exitCtx); err != nil {
			logger.Error("exiting with requests still running, so their attempts may be unrecorded", "error", err.Error())
		}
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

// buildRoutes creates each configured provider and the alias table that uses them.
func buildRoutes(cfg *config.Config, secrets *config.Secrets) (*routing.Table, error) {
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	limiters := make(map[string]*limits.FairLimiter, len(cfg.Providers))
	for _, p := range cfg.Providers {
		adapter, err := newProvider(p, secrets.ProviderKeys[p.Name], cfg.Limits)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.Name, err)
		}
		providers[p.Name] = adapter
		if p.MaxConcurrency > 0 {
			// Upstream capacity belongs to the account, so routes share one limiter.
			limiters[p.Name] = limits.NewFairLimiter(limits.Config{
				MaxConcurrency:    p.MaxConcurrency,
				MaxQueue:          p.MaxQueue,
				MaxQueuePerKey:    p.MaxQueuePerTeam,
				MaxInFlightPerKey: p.MaxInFlightPerTeam,
				QueueTimeout:      p.QueueTimeout,
			})
		}
	}

	aliases := make([]routing.Alias, len(cfg.Models))
	for i, m := range cfg.Models {
		routes := make([]routing.Route, len(m.Routes))
		for j, r := range m.Routes {
			routes[j] = routing.Route{
				ProviderName: r.Provider,
				Provider:     providers[r.Provider],
				Model:        r.Model,
				Tier:         r.Tier,
				Weight:       r.Weight,
				Limiter:      limiters[r.Provider],
			}
		}
		aliases[i] = routing.Alias{
			Name:            m.Alias,
			MaxOutputTokens: m.MaxOutputTokens,
			MaxAttempts:     m.MaxAttempts,
			Routes:          routes,
		}
	}
	return routing.NewTable(aliases, routing.Options{Breaker: routing.BreakerConfig{
		Window:         cfg.CircuitBreaker.Window,
		MinRequests:    cfg.CircuitBreaker.MinRequests,
		ErrorRate:      cfg.CircuitBreaker.ErrorRate,
		Cooldown:       cfg.CircuitBreaker.Cooldown,
		HalfOpenProbes: cfg.CircuitBreaker.HalfOpenProbes,
	}}), nil
}

func newProvider(p config.Provider, apiKey string, limits config.Limits) (provider.Provider, error) {
	switch p.Type {
	case config.ProviderAnthropic:
		return anthropic.New(anthropic.Options{
			BaseURL:          p.BaseURL,
			APIKey:           apiKey,
			MaxResponseBytes: limits.MaxResponseBytes,
			MaxEventBytes:    limits.MaxEventBytes,
		})
	default:
		return openai.New(openai.Options{
			BaseURL:          p.BaseURL,
			APIKey:           apiKey,
			UseMaxTokens:     p.MaxTokensParam == config.MaxTokens,
			MaxResponseBytes: limits.MaxResponseBytes,
			MaxEventBytes:    limits.MaxEventBytes,
		})
	}
}
