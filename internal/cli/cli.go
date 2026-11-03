// Package cli implements the moor command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
)

// Exit statuses.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usageError marks a mistake in how a command was invoked.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// Main runs the command line with args (without the program name) and
// returns the process exit status.
func Main(args []string, stdout, stderr io.Writer) int {
	root := newRoot(stdout, stderr)
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(context.Background())
	if err == nil {
		return exitOK
	}
	fmt.Fprintf(stderr, "moor: %v\n", err)
	var usage usageError
	if errors.As(err, &usage) || strings.HasPrefix(err.Error(), "unknown command") {
		fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
		return exitUsage
	}
	return exitError
}

func newRoot(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "moor",
		Short: "Moorage: push code, get a URL, watch it run",
		Long: `Moorage deploys apps to a server of your own and gives each one a URL.
The moor binary is both the server and the command line that talks to it.`,
		Version:       buildinfo.Get().String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(newVersionCmd(), newServerCmd(), newDeployCmd(), newAppsCmd(), newReleasesCmd(), newRollbackCmd())
	return root
}

// newLogger returns the server's logger, writing to w at the configured
// level and format (both already validated).
func newLogger(w io.Writer, c config.Log) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
