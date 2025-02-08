// Command llmgate-mock runs the deterministic mock provider.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Bispulm/llmgate/internal/mock"
)

const (
	defaultListen   = "127.0.0.1:18301"
	shutdownTimeout = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "llmgate-mock:", err)
		os.Exit(1)
	}
}

// run serves until ctx ends. The optional started callback receives the bound
// address, which is how a caller listening on port 0 learns the port.
func run(ctx context.Context, args []string, stderr io.Writer, started func(addr string)) error {
	flags := flag.NewFlagSet("llmgate-mock", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", defaultListen, "address to listen on")
	if err := flags.Parse(args); err != nil {
		return err
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	server := &http.Server{
		Handler:           mock.New(),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	_, _ = fmt.Fprintln(stderr, "llmgate-mock listening on", listener.Addr())
	if started != nil {
		started(listener.Addr().String())
	}

	select {
	case err := <-serveErr:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("shutting down: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}
