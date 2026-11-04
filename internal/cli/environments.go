package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/names"
)

// appFlags are what a command about an app takes.
type appFlags struct {
	conn connection
	app  string
}

func (f *appFlags) add(cmd *cobra.Command) {
	f.conn.addFlags(cmd)
	cmd.Flags().StringVar(&f.app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
}

func (f *appFlags) open() (string, *client.Client, error) {
	app, _, err := resolveApp(f.app)
	if err != nil {
		return "", nil, err
	}
	c, err := f.conn.client()
	return app, c, err
}

func releaseText(e apitypes.Environment) string {
	if e.Release == nil {
		return "-"
	}
	return fmt.Sprintf("v%d", e.Release.Version)
}

func expiryText(e apitypes.Environment) string {
	switch {
	case e.Pinned:
		return "pinned"
	case e.ExpiresAt.IsZero():
		return "-"
	}
	return e.ExpiresAt.Local().Format(time.DateOnly)
}

func newEnvironmentsCmd() *cobra.Command {
	var f appFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "environments",
		Aliases: []string{"envs"},
		Short:   "List and make the environments of an app",
		Long: `An environment is a place an app runs, with its own address, variables, data
and releases. Every app has production. A named environment, such as staging,
stays until it is removed; a preview comes and goes with its branch.

An environment other than production is at <app>--<environment> beside the app's
own address, and asks search engines not to index it.`,
		Example: `  moor environments --app shop
  moor environments create staging --branch develop
  moor deploy --env staging
  moor promote --from staging
  moor environments delete staging`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			list, err := c.Environments(cmd.Context(), app)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "ENVIRONMENT\tKIND\tBRANCH\tRELEASE\tLOCKED BY\tADDRESS")
			for _, e := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, e.Kind, orDash(e.Branch), releaseText(e), orDash(e.LockedBy), e.URL)
			}
			return w.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newEnvironmentCreateCmd(), newEnvironmentDeleteCmd())
	return cmd
}

func newEnvironmentCreateCmd() *cobra.Command {
	var f appFlags
	var branch string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Make a named environment",
		Long: `Make a named environment. It starts with the variables an app has for every
environment and an empty data directory, and has nothing to serve until something
is deployed to it. With --branch, a push to that branch deploys to it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			e, err := c.CreateEnvironment(cmd.Context(), app, apitypes.NewEnvironment{Name: args[0], Branch: branch})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Made %s of %s. Deploy to it with: moor deploy --app %s --env %s\n\n  %s\n", e.Name, app, app, e.Name, e.URL)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&branch, "branch", "", "the branch whose pushes deploy to it")
	return cmd
}

func newEnvironmentDeleteCmd() *cobra.Command {
	var f appFlags
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Remove an environment, with its data, logs and releases",
		Long:  "Remove an environment, with its data, logs and releases. Production cannot be removed: delete the app for that.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if err := c.DeleteEnvironment(cmd.Context(), app, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s of %s.\n", args[0], app)
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func newPreviewsCmd() *cobra.Command {
	var conn connection
	var app string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "previews",
		Short: "List and manage previews of branches",
		Long: `Pushing a branch makes a preview of it: its own build, at its own address, with
the variables of production (except those marked --production-only) and an empty
data directory. Deleting the branch destroys it, and so does going unused for the
time [previews].ttl says, seven days unless told otherwise, unless it is pinned.
An app may have ten at a time.`,
		Example: `  moor previews
  moor previews create feature/new-pricing --app shop
  moor previews pin feature-new-pricing --app shop
  moor previews destroy feature-new-pricing --app shop`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			list, err := c.Previews(cmd.Context())
			if err != nil {
				return err
			}
			if app != "" {
				only := list[:0]
				for _, e := range list {
					if e.App == app {
						only = append(only, e)
					}
				}
				list = only
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No previews yet. Pushing a branch makes one.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "APP\tPREVIEW\tBRANCH\tRELEASE\tEXPIRES\tADDRESS")
			for _, e := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.App, e.Name, orDash(e.Branch), releaseText(e), expiryText(e), e.URL)
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&app, "app", "", "only the previews of this app")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newPreviewCreateCmd(), newPreviewPinCmd("pin", true), newPreviewPinCmd("unpin", false), newPreviewDestroyCmd())
	return cmd
}

func newPreviewCreateCmd() *cobra.Command {
	var f appFlags
	var detach bool
	cmd := &cobra.Command{
		Use:   "create BRANCH",
		Short: "Make a preview of a branch that was pushed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			d, err := c.CreatePreview(cmd.Context(), app, args[0])
			if err != nil {
				return err
			}
			if detach {
				fmt.Fprintf(cmd.OutOrStdout(), "Deployment %s is queued; follow it with: moor deployments follow %s\n", d.ID, d.ID)
				return nil
			}
			return followDeployment(cmd, c, d.ID)
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&detach, "detach", false, "queue the deployment and return")
	return cmd
}

func newPreviewPinCmd(use string, pinned bool) *cobra.Command {
	var f appFlags
	short := "Keep a preview from expiring"
	if !pinned {
		short = "Let a preview expire again"
	}
	cmd := &cobra.Command{
		Use:   use + " NAME",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			e, err := c.PinEnvironment(cmd.Context(), app, args[0], pinned)
			if err != nil {
				return err
			}
			if pinned {
				fmt.Fprintf(cmd.OutOrStdout(), "%s of %s will not expire.\n", e.Name, app)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s of %s expires on %s, unless it is used.\n", e.Name, app, expiryText(e))
			}
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func newPreviewDestroyCmd() *cobra.Command {
	cmd := newEnvironmentDeleteCmd()
	cmd.Use = "destroy NAME"
	cmd.Short = "Destroy a preview, with its data, logs and releases"
	cmd.Long = "Destroy a preview, with its data, logs and releases. Its branch stays, and a new push to it makes another."
	return cmd
}

func newPromoteCmd() *cobra.Command {
	var f appFlags
	var from, to string
	cmd := &cobra.Command{
		Use:   "promote --from ENVIRONMENT",
		Short: "Release what one environment runs to another",
		Long: `Release the build one environment runs to another, without building it again,
with the variables of the environment it goes to. What was tried is what runs.`,
		Example: `  moor promote --from staging
  moor promote --from feature-new-pricing --to staging --app shop`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			d, err := c.Promote(cmd.Context(), app, from, to)
			if err != nil {
				return err
			}
			for _, warning := range d.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s of %s now serves what %s runs, as v%d.\n\n  %s\n", to, app, from, d.Release.Version, d.URL)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&from, "from", "", "the environment whose build to release")
	cmd.Flags().StringVar(&to, "to", names.Production, "the environment to release it to")
	cobra.CheckErr(cmd.MarkFlagRequired("from"))
	return cmd
}

func newLockCmd() *cobra.Command {
	return lockCommand("lock", "Stop deployments to an environment",
		`Stop deployments to an environment until it is unlocked, as after a rollback, while
what went wrong is looked into. Rollbacks and changes of variables go on working.`,
		func(cmd *cobra.Command, c *client.Client, app, env string) error {
			if _, err := c.LockEnvironment(cmd.Context(), app, env); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s of %s takes no deployments until it is unlocked.\n", env, app)
			return nil
		})
}

func newUnlockCmd() *cobra.Command {
	return lockCommand("unlock", "Let an environment take deployments again", "Let an environment take deployments again.",
		func(cmd *cobra.Command, c *client.Client, app, env string) error {
			if _, err := c.UnlockEnvironment(cmd.Context(), app, env); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s of %s takes deployments again.\n", env, app)
			return nil
		})
}

func lockCommand(use, short, long string, run func(*cobra.Command, *client.Client, string, string) error) *cobra.Command {
	var f appFlags
	var env string
	cmd := &cobra.Command{
		Use: use, Short: short, Long: long,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			return run(cmd, c, app, env)
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	return cmd
}

func newSleepCmd() *cobra.Command {
	return lockCommand("sleep", "Stop an environment's instances until a request wakes it",
		`Stop an environment's instances, except processes with sleep = false, until a
request wakes it. A preview does this by itself after 30 minutes with no requests,
and any environment can with sleep_after in [environments.NAME].`,
		func(cmd *cobra.Command, c *client.Client, app, env string) error {
			if _, err := c.SleepEnvironment(cmd.Context(), app, env); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s of %s is asleep. The next request wakes it, or: moor wake\n", env, app)
			return nil
		})
}

func newWakeCmd() *cobra.Command {
	return lockCommand("wake", "Start an environment that is asleep", "Start an environment that is asleep, and return when it is ready.",
		func(cmd *cobra.Command, c *client.Client, app, env string) error {
			e, err := c.WakeEnvironment(cmd.Context(), app, env)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s of %s is awake.\n\n  %s\n", env, app, e.URL)
			return nil
		})
}
