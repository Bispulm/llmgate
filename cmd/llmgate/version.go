package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Set at release time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// buildInfo fills in the commit from the module's build information when the release
// flags weren't set, as in a plain go build from a checkout.
func buildInfo() (v, c, d string) {
	v, c, d = version, commit, date
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			}
		}
	}
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	return v, c, d
}

func versionCommand(p process) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			v, c, d := buildInfo()
			_, err := fmt.Fprintf(p.stdout, "llmgate %s, commit %s, built %s, %s\n", v, c, d, runtime.Version())
			return err
		},
	}
}
