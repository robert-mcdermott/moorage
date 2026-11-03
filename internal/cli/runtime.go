package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/names"
)

func newPsCmd() *cobra.Command {
	var conn connection
	var app, env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List an app's running instances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			appName, _, err := resolveApp(app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			ins, err := c.Instances(cmd.Context(), appName, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), ins)
			}
			return printInstances(cmd, appName, ins)
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func printInstances(cmd *cobra.Command, app string, ins []apitypes.Instance) error {
	if len(ins) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%s runs no instances (it is a static site, or nothing is deployed).\n", app)
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "INSTANCE\tPROCESS\tRELEASE\tSTATUS\tHEALTH\tPORT\tPID\tRESTARTS\tUP")
	for _, in := range ins {
		health := "unhealthy"
		if in.Healthy {
			health = "healthy"
		}
		port, pid, up := "-", "-", "-"
		if in.Port != 0 {
			port = strconv.Itoa(in.Port)
		}
		if in.PID != 0 {
			pid = strconv.Itoa(in.PID)
		}
		if !in.StartedAt.IsZero() && in.Status == "running" {
			up = time.Since(in.StartedAt).Round(time.Second).String()
		}
		fmt.Fprintf(w, "%s\t%s.%d\tv%d\t%s\t%s\t%s\t%s\t%d\t%s\n",
			in.ID, in.Process, in.Index, in.Release, in.Status, health, port, pid, in.Restarts, up)
	}
	return w.Flush()
}

func newRestartCmd() *cobra.Command {
	var conn connection
	var app, env string
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Replace an app's instances without downtime",
		Long: `Start new instances of the current release, move traffic to them once they
are healthy, and stop the old ones after their requests finish.`,
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
			ins, err := c.Restart(cmd.Context(), appName, env)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restarted %s.\n\n", appName)
			return printInstances(cmd, appName, ins)
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	return cmd
}

func newLogsCmd() *cobra.Command {
	var conn connection
	var app, env, process string
	var tail int
	var follow, asJSON bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show an app's logs",
		Long: `Show the output of an app's instances, merged by time. Lines marked with *
are notes from Moorage (starts, exits, restarts); lines marked with ! came
from standard error.`,
		Example: `  moor logs --app api
  moor logs -f --app api --process worker`,
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
			ctx := cmd.Context()
			if follow {
				var stop context.CancelFunc
				ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
			}
			out := cmd.OutOrStdout()
			return c.Logs(ctx, appName, env, process, tail, follow, func(l apitypes.LogLine) error {
				if asJSON {
					return printJSON(out, l)
				}
				mark := " "
				switch l.Stream {
				case "M":
					mark = "*"
				case "E":
					mark = "!"
				}
				_, err := fmt.Fprintf(out, "%s %s.%d %s %s\n", l.Time.Local().Format("15:04:05.000"), l.Process, l.Index, mark, l.Text)
				return err
			})
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	f.StringVar(&env, "env", names.Production, "environment")
	f.StringVar(&process, "process", "", "show one process only, such as web")
	f.IntVarP(&tail, "tail", "n", 100, "lines to show from the end")
	f.BoolVarP(&follow, "follow", "f", false, "keep showing new lines")
	f.BoolVar(&asJSON, "json", false, "print each line as JSON")
	return cmd
}
