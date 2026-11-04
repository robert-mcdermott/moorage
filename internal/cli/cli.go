// Package cli implements the moor command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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

// exitStatus ends moor with a status of its own, having said what there is to say.
type exitStatus struct{ code int }

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// usageError marks a mistake in how a command was invoked.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// Main runs the command line with args (without the program name) and
// returns the process exit status.
func Main(args []string, stdout, stderr io.Writer) int {
	return mainWith(args, os.Stdin, stdout, stderr)
}

func mainWith(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := newRoot(stdout, stderr)
	root.SetIn(stdin)
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(context.Background())
	if err == nil {
		return exitOK
	}
	var status exitStatus
	if errors.As(err, &status) {
		return status.code
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
	root.AddCommand(newVersionCmd(), newServerCmd(), newDeployCmd(), newAppsCmd(), newReleasesCmd(), newRollbackCmd(), newRedeployCmd(), newScaleCmd(), newDeploymentsCmd(), newEnvCmd(), newManifestCmd(), newPsCmd(), newLogsCmd(), newRestartCmd(), newGitCmd(), newKeysCmd(),
		newLoginCmd(), newLogoutCmd(), newWhoamiCmd(), newContextCmd(), newTokensCmd(), newPasswordCmd(), newProjectsCmd(), newAuditCmd(), newEnvironmentsCmd(), newPreviewsCmd(), newPromoteCmd(), newLockCmd(), newUnlockCmd(), newSleepCmd(), newWakeCmd(), newAccessCmd(), newShareCmd(), newDomainsCmd(), newRunCmd(), newCronCmd(), newRegistryCmd(), newTrustCmd(), newEventsCmd())
	return root
}

// newLogger returns the server's logger, writing to w at the configured
// level and format (both already validated) and, when file is set, also as
// JSON to that rotated log file.
func newLogger(w io.Writer, c config.Log, file io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if c.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	}
	if file != nil {
		h = slog.NewMultiHandler(h, slog.NewJSONHandler(file, opts))
	}
	return slog.New(h)
}
