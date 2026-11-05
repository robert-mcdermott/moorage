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
  moor services update db
  moor services upgrade db --to 19
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
	cmd.AddCommand(newServiceLogsCmd(), newServiceRemoveCmd(), newServiceUpdateCmd(), newServiceUpgradeCmd())
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

func newServiceUpdateCmd() *cobra.Command {
	var f appFlags
	var env string
	cmd := &cobra.Command{
		Use:   "update NAME",
		Short: "Take the newest image of a service's version",
		Long: `Take the newest image of a service's version, such as PostgreSQL 18.7 where it ran 18.6, and restart it. What it holds stays. This does not happen by itself: an image
is recorded when a service is made, and an update is something that you ask for.`,
		Example: `  moor services update db`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			said, err := c.UpdateService(cmd.Context(), app, env, args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), said)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", "production", "the environment")
	return cmd
}

func newServiceUpgradeCmd() *cobra.Command {
	var f appFlags
	var env, to string
	var finish bool
	cmd := &cobra.Command{
		Use:   "upgrade NAME",
		Short: "Move a service to a newer major version",
		Long: `Move a PostgreSQL service to a newer major version: a dump with the newer version's tools, a new volume, a restore and a check that it holds what it did. The app is
stopped for that, as what it wrote after the dump would be lost, and is started again however it goes. The old volume stays until you say that all is well with --finish, and
anything going wrong leaves the old one as it was. Say the new version in the manifest too, if it names one.`,
		Example: `  moor services upgrade db --to 19
  moor services upgrade db --finish`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (to == "") == !finish {
				return usageError{fmt.Errorf("say --to VERSION, or --finish to let go of the old volume")}
			}
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if finish {
				if err := c.FinishServiceUpgrade(cmd.Context(), app, env, args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Let go of what %s held before its last upgrade.\n", args[0])
				return nil
			}
			said, err := c.UpgradeService(cmd.Context(), app, env, args[0], to)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), said)
			fmt.Fprintf(cmd.OutOrStdout(), "It runs PostgreSQL %s, and the old volume stays until: moor services upgrade %s --finish\n", to, args[0])
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", "production", "the environment")
	cmd.Flags().StringVar(&to, "to", "", "the major version to move to")
	cmd.Flags().BoolVar(&finish, "finish", false, "let go of what it held before its last upgrade")
	return cmd
}
