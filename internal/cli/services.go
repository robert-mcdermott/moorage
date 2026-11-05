// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

func newServicesCmd() *cobra.Command {
	var f appFlags
	var env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "services",
		Short: "See and remove the services of an environment",
		Long: `A service is a database that an app declares under [services] in moorage.toml, and Moorage runs and connects for it. Each environment
has its own, with a password of its own, and the app is given its address as it starts. It needs a container engine, and nothing else here does.

A service that a manifest no longer names stays, with what it holds, until you remove it. Deleting an environment removes its services.`,
		Example: `  moor services --app shop
  moor services logs db --env staging
  moor services remove db --env my-branch --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			list, err := c.Services(cmd.Context(), app, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s of %s has no services. An app declares them under [services] in moorage.toml.\n", env, app)
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "SERVICE\tTYPE\tSTATUS\tGIVES THE APP\tPORT")
			for _, s := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", s.Name, serviceKind(s), serviceStatus(s), s.Variable, s.Port)
			}
			return w.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", "production", "the environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newServiceLogsCmd(), newServiceRemoveCmd())
	return cmd
}

func serviceKind(s apitypes.Service) string {
	if s.Version == "" {
		return s.Type
	}
	return s.Type + " " + s.Version
}

func serviceStatus(s apitypes.Service) string {
	var status string
	switch {
	case s.Unknown:
		status = "unknown: the engine cannot be asked"
	case s.Running:
		status = "running"
	case s.State == "stopped":
		status = "stopped"
	default:
		status = "not running, and should be"
	}
	if s.Unused {
		status += ", unused"
	}
	return status
}

func newServiceLogsCmd() *cobra.Command {
	var f appFlags
	var env string
	var tail int
	cmd := &cobra.Command{
		Use:     "logs NAME",
		Short:   "Read the newest of what a service has said",
		Example: `  moor services logs db --tail 500`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			text, err := c.ServiceLog(cmd.Context(), app, env, args[0], tail)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), text)
			if text != "" && !strings.HasSuffix(text, "\n") {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", "production", "the environment")
	cmd.Flags().IntVar(&tail, "tail", 100, "how many lines, up to 5,000")
	return cmd
}

func newServiceRemoveCmd() *cobra.Command {
	var f appFlags
	var env string
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a service, with what it holds",
		Long: `Remove a service, with what it holds, which cannot be undone. If the manifest still names it, the next deployment makes a new one, which starts
empty.`,
		Example: `  moor services remove db --env my-branch --yes`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if !yes {
				return usageError{fmt.Errorf("this removes %s of %s of %s, with what it holds, for good: say --yes", args[0], env, app)}
			}
			if err := c.RemoveService(cmd.Context(), app, env, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s of %s of %s, with what it held.\n", args[0], env, app)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", "production", "the environment")
	cmd.Flags().BoolVar(&yes, "yes", false, "say that you mean it")
	return cmd
}
