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

	"github.com/Bispulm/llmgate/internal/api"
	"github.com/Bispulm/llmgate/internal/config"
)

const usage = "usage: llmgate serve -config <path>"

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
	if _, err := cfg.ResolveSecrets(getenv); err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	handler := api.New(api.Options{Logger: logger})

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
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}
