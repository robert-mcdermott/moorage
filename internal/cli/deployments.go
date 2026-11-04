package cli

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/names"
)

func newDeploymentsCmd() *cobra.Command {
	var conn connection
	var app, env string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "deployments",
		Short: "List an app's deployments",
		Long: `List an app's latest deployments: what was deployed, how it went, and the
release it made. Show one with 'moor deployments show ID', and follow one
that is running with 'moor deployments follow ID'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			appName, _, err := resolveApp(app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			deps, err := c.Deployments(cmd.Context(), appName, env, limit)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), deps)
			}
			if len(deps) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s has no deployments yet.\n", appName)
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "ID\tSOURCE\tSTATUS\tRELEASE\tTOOK\tCREATED")
			for _, d := range deps {
				release, took := "-", "-"
				if d.Release != nil {
					release = "v" + strconv.Itoa(d.Release.Version)
				}
				if !d.FinishedAt.IsZero() {
					took = d.FinishedAt.Sub(d.CreatedAt).Round(10 * time.Millisecond).String()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, source(d), d.Status, release, took, ago(d.CreatedAt))
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	f.StringVar(&env, "env", names.Production, "environment")
	f.IntVar(&limit, "limit", 20, "how many to list")
	f.BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newDeploymentShowCmd(), newDeploymentFollowCmd())
	return cmd
}

func newDeploymentShowCmd() *cobra.Command {
	var conn connection
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show ID",
		Short: "Show a deployment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			d, err := c.Deployment(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), d)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Deployment\t%s\n", d.ID)
			fmt.Fprintf(w, "App\t%s (%s)\n", d.App, d.Env)
			fmt.Fprintf(w, "Source\t%s\n", source(d))
			if d.Message != "" {
				fmt.Fprintf(w, "Commit\t%s %s\n", d.Commit, d.Message)
			}
			fmt.Fprintf(w, "Status\t%s\n", d.Status)
			if d.Release != nil {
				fmt.Fprintf(w, "Release\tv%d\n", d.Release.Version)
			}
			if d.Build != "" {
				fmt.Fprintf(w, "Build\t%s\n", d.Build)
			}
			fmt.Fprintf(w, "Created\t%s\n", d.CreatedAt.Local().Format(time.DateTime))
			if !d.FinishedAt.IsZero() {
				fmt.Fprintf(w, "Took\t%s\n", d.FinishedAt.Sub(d.CreatedAt).Round(10*time.Millisecond))
			}
			if d.Error != "" {
				fmt.Fprintf(w, "Error\t%s\n", d.Error)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if len(d.Output) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\nLast output:\n  %s\n", strings.Join(d.Output, "\n  "))
			}
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newDeploymentFollowCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "follow ID",
		Short: "Print a deployment's log as it happens",
		Long: `Print a deployment's log from its start and, while it runs, as it is
written. Fails if the deployment failed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			return followDeployment(cmd, c, args[0])
		},
	}
	conn.addFlags(cmd)
	return cmd
}

// source says what a deployment deployed: for a push, the branch and the
// commit.
func source(d apitypes.Deployment) string {
	if d.Source == "image" {
		return "image " + d.Ref
	}
	if d.Commit == "" {
		return d.Source
	}
	commit := d.Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return "push " + strings.TrimPrefix(d.Ref, "refs/heads/") + "@" + commit
}
