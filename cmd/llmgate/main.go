// Command llmgate runs the gateway, and administers it through its admin API.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// listening reports the addresses a server bound, which matters when a port is 0.
type listening struct {
	Gateway string
	Admin   string
	Metrics string
}

// process is what a command gets from its environment, so tests can supply their own.
type process struct {
	getenv  func(string) string
	stdout  io.Writer
	stderr  io.Writer
	started func(listening)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], process{getenv: os.Getenv, stdout: os.Stdout, stderr: os.Stderr}); err != nil {
		fmt.Fprintln(os.Stderr, "llmgate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, p process) error {
	root := &cobra.Command{
		Use:           "llmgate",
		Short:         "An OpenAI-compatible LLM gateway with budgets, routing, and failover",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("usage: llmgate <command>, and llmgate --help lists the commands")
		},
	}
	root.SetOut(p.stdout)
	root.SetErr(p.stderr)
	root.SetArgs(args)
	// Each command gets ctx from ExecuteContext, through cmd.Context().
	root.AddCommand(serveCommand(p), migrateCommand(p)) //nolint:contextcheck // See above.
	addAdminCommands(root, p)                           //nolint:contextcheck // See above.
	return root.ExecuteContext(ctx)
}
