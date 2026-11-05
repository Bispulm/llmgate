package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/spf13/cobra"

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
	// requestExitTimeout bounds the wait for canceled requests to finish recording.
	requestExitTimeout = 10 * time.Second
	// adminShutdownTimeout bounds finishing admin requests at shutdown.
	adminShutdownTimeout = 5 * time.Second
)

func serveCommand(p process) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the gateway",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), configPath, p)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the configuration file")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func serve(ctx context.Context, configPath string, p process) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	secrets, err := cfg.ResolveSecrets(p.getenv)
	if err != nil {
		return err
	}
	routes, err := buildRoutes(cfg, secrets)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(p.stderr, nil))
	opts := api.Options{
		Logger:   logger,
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
			AdmissionTimeout:   cfg.Limits.AdmissionTimeout,
		},
	}

	var tenant *tenantRuntime
	if cfg.Tenant() {
		tenant, err = startTenant(ctx, cfg, secrets, routes, logger)
		if err != nil {
			return err
		}
		defer tenant.close()
		opts.Auth, opts.Ledger, opts.TeamLimiter, opts.Cache = tenant.keys, tenant.ledger, tenant.limiter, tenant.cache
	} else {
		logger.Warn("development mode: budgets are not enforced and usage is only logged")
		opts.Auth = auth.NewDevKey(secrets.DevKey)
	}
	handler := api.New(opts)

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	addrs := listening{Gateway: listener.Addr().String()}

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
	serveErr := make(chan error, 2)
	go func() { serveErr <- server.Serve(listener) }()

	var adminServer *http.Server
	var background sync.WaitGroup
	stopBackground := func() {}
	if tenant != nil {
		adminListener, err := lc.Listen(ctx, "tcp", cfg.Admin.Listen)
		if err != nil {
			_ = server.Close()
			return fmt.Errorf("listening for the admin API: %w", err)
		}
		addrs.Admin = adminListener.Addr().String()
		adminServer = &http.Server{Handler: tenant.admin, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
		go func() { serveErr <- adminServer.Serve(adminListener) }()

		var reconcileCtx context.Context
		reconcileCtx, stopBackground = context.WithCancel(context.WithoutCancel(ctx))
		background.Go(func() { tenant.runReconciler(reconcileCtx, logger) })
		logger.Info("admin API listening", "addr", addrs.Admin)
	}

	logger.Info("gateway listening", "addr", addrs.Gateway, "auth_mode", cfg.Auth.Mode)
	if p.started != nil {
		p.started(addrs)
	}

	select {
	case err := <-serveErr:
		_ = server.Close()
		if adminServer != nil {
			_ = adminServer.Close()
		}
		stopBackground()
		background.Wait()
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

		// Close doesn't wait for handlers, and each one settles its attempt as it
		// finishes. Exiting now would leave those to the reconciler.
		exitCtx, cancelExit := context.WithTimeout(context.WithoutCancel(ctx), requestExitTimeout)
		defer cancelExit()
		if err := handler.WaitForRequests(exitCtx); err != nil {
			logger.Error("exiting with requests still running, so their attempts may be unrecorded", "error", err.Error())
		}
	}
	if adminServer != nil {
		adminCtx, cancelAdmin := context.WithTimeout(context.WithoutCancel(ctx), adminShutdownTimeout)
		defer cancelAdmin()
		if err := adminServer.Shutdown(adminCtx); err != nil {
			_ = adminServer.Close()
		}
	}
	stopBackground()
	background.Wait()

	for range cap(serveErr) {
		select {
		case err := <-serveErr:
			if !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serving: %w", err)
			}
		default:
		}
	}
	return nil
}

// buildRoutes creates each configured provider and the alias table that uses them.
func buildRoutes(cfg *config.Config, secrets *config.Secrets) (*routing.Table, error) {
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	scopes := make(map[string]string, len(cfg.Providers))
	limiters := make(map[string]*limits.FairLimiter, len(cfg.Providers))
	for _, p := range cfg.Providers {
		adapter, err := newProvider(p, secrets.ProviderKeys[p.Name], cfg.Limits)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.Name, err)
		}
		providers[p.Name] = adapter
		scopes[p.Name] = p.Type + " " + p.BaseURL
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
				Scope:        scopes[r.Provider],
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
			CacheTTL:        m.Cache.TTL,
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
