package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/applogs"
	"github.com/robert-mcdermott/moorage/internal/client"
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
			if len(ins) == 0 {
				if e, err := c.Environment(cmd.Context(), appName, env); err == nil && e.Asleep {
					fmt.Fprintf(cmd.OutOrStdout(), "%s is asleep. The next request wakes it, or: moor wake\n", appName)
					return nil
				}
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

// parseFormation reads what `moor scale` asks for: web=3 worker=1.
func parseFormation(args []string) (map[string]int, error) {
	formation := map[string]int{}
	for _, arg := range args {
		name, count, ok := strings.Cut(arg, "=")
		n, err := strconv.Atoi(count)
		if !ok || name == "" || err != nil || n < 0 {
			return nil, usageError{fmt.Errorf("%q is not a process and a number, such as web=3", arg)}
		}
		formation[name] = n
	}
	return formation, nil
}

func newScaleCmd() *cobra.Command {
	var conn connection
	var app, env string
	cmd := &cobra.Command{
		Use:   "scale [PROCESS=N ...]",
		Short: "Show or change how many instances of each process run",
		Long: `Set how many instances of each named process run. New ones start beside those
running; those beyond the number finish their requests and stop. If a new
instance cannot start, the app goes back to how it was.

The setting stays, over the manifest's instances, for the releases to come.
Without arguments, it shows how many run.`,
		Example: `  moor scale --app shop
  moor scale web=3 worker=1 --app shop`,
		RunE: func(cmd *cobra.Command, args []string) error {
			formation, err := parseFormation(args)
			if err != nil {
				return err
			}
			appName, _, err := resolveApp(app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			var state apitypes.Environment
			if len(formation) == 0 {
				state, err = c.Environment(cmd.Context(), appName, env)
			} else {
				state, err = c.Scale(cmd.Context(), appName, env, formation)
			}
			if err != nil {
				return err
			}
			if len(formation) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Scaled %s.\n\n", appName)
			}
			return printInstances(cmd, appName, state.Instances)
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	return cmd
}

func newLogsCmd() *cobra.Command {
	var conn connection
	var app, env string
	var q client.LogQuery
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show an app's logs",
		Long: `Show the output of an app's instances, merged by time. Lines marked with *
are notes from Moorage (starts, exits, restarts); lines marked with ! came
from standard error.

--source chooses what to show, from stdout, stderr, moorage, build, access
and events; the first three unless asked.`,
		Example: `  moor logs --app api
  moor logs -f --app api --process worker
  moor logs --app api --source stderr,build --since 1h --grep timeout
  moor logs --app api --problems --since 24h`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := applogs.SplitSources(q.Source); err != nil {
				return usageError{err}
			}
			appName, _, err := resolveApp(app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if q.Follow {
				var stop context.CancelFunc
				ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
			}
			out := cmd.OutOrStdout()
			return c.Logs(ctx, appName, env, q, func(l apitypes.LogLine) error {
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
				who := l.Source
				if l.Source == "" || l.Source == applogs.Stdout || l.Source == applogs.Stderr || l.Source == applogs.Moorage {
					who = fmt.Sprintf("%s.%d", l.Process, l.Index)
				}
				text := l.Text
				if l.Message != "" {
					text = l.Message
				}
				_, err := fmt.Fprintf(out, "%s %s %s %s\n", l.Time.Local().Format("15:04:05.000"), who, mark, text)
				return err
			})
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	f.StringVar(&env, "env", names.Production, "environment")
	f.StringVar(&q.Process, "process", "", "show one process only, such as web")
	f.StringVar(&q.Instance, "instance", "", "show one instance only: its number or its ID")
	f.StringVar(&q.Source, "source", "", "sources to show, separated by commas: stdout, stderr, moorage, build, access, events")
	f.StringVar(&q.Grep, "grep", "", "show the lines with this text, whatever its case")
	f.BoolVar(&q.Problems, "problems", false, "show the lines that report a problem: errors, warnings, standard error, failed requests")
	f.StringVar(&q.Since, "since", "", "show lines since then: how long ago (15m, 2h, 7d) or a moment")
	f.StringVar(&q.Until, "until", "", "show lines until then")
	f.IntVarP(&q.Tail, "tail", "n", 100, "lines to show from the end")
	f.BoolVarP(&q.Follow, "follow", "f", false, "keep showing new lines")
	f.BoolVar(&asJSON, "json", false, "print each line as JSON")
	return cmd
}
