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

func newTelemetryCmd() *cobra.Command {
	var conn connection
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "telemetry",
		Short: "Send what apps say and do somewhere else as well",
		Long: `Send what apps say to syslog, an OpenTelemetry collector or any HTTP endpoint, and what they do
to an OpenTelemetry collector. What is kept here stays, and a sink that is down costs nothing but its
own place in the queue, of 20,000 lines: past that, the oldest go, and it says how many did.`,
		Example: `  moor server telemetry add central --kind syslog --address tls://logs.example.com:6514
  moor server telemetry add collector --kind otlp --address https://otel.example.com:4318 --logs --metrics --header "Authorization: Bearer abc"
  moor server telemetry test central
  moor server telemetry rm central`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			list, err := c.TelemetrySinks(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing is being sent anywhere else. Add a sink with: moor server telemetry add NAME --kind otlp --address https://…")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tADDRESS\tSENDS\tAPP\tLAST SENT\tDROPPED\tPROBLEM")
			for _, k := range list {
				sends := []string{}
				if k.Logs {
					sends = append(sends, "logs")
				}
				if k.Metrics {
					sends = append(sends, "metrics")
				}
				app, last := k.App, "-"
				if app == "" {
					app = "all"
				}
				if !k.LastSentAt.IsZero() {
					last = ago(k.LastSentAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", k.Name, k.Kind, k.Address, strings.Join(sends, "+"), app, last, k.Dropped, k.LastError)
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(newTelemetryAddCmd(), newTelemetryRmCmd(), newTelemetryTestCmd())
	return cmd
}

func newTelemetryAddCmd() *cobra.Command {
	var conn connection
	var in apitypes.NewTelemetrySink
	var headers []string
	var logs bool
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Add a sink, or change one",
		Long: `Add somewhere to send what apps say and do, or change the one of that name. Logs go unless it
is given --metrics alone.

An address is udp://host:514, tcp://host:601 or tls://host:6514 for syslog. For OTLP it is what comes
before /v1/logs, as in https://otel.example.com:4318. For HTTP, it is where JSON batches are sent.

--header is sent with each request, sealed here and not shown again, and is left as it is if
you leave it out, unless the address changes. To be sent in the clear across the internet is not
for a header, so it takes an https address, or something on the local network.`,
		Example: `  moor server telemetry add central --kind syslog --address tls://logs.example.com:6514 --app shop
  moor server telemetry add collector --kind otlp --address https://otel.example.com:4318 --metrics --logs`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			in.Name, in.Logs = args[0], logs || !in.Metrics
			if len(headers) > 0 {
				in.Headers = map[string]string{}
				for _, h := range headers {
					name, value, found := strings.Cut(h, ":")
					if !found {
						return fmt.Errorf("%q is not a header: it goes Name: value", h)
					}
					in.Headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
				}
			}
			k, err := c.SetTelemetrySink(cmd.Context(), in)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is sending to %s. Try it with: moor server telemetry test %s\n", k.Name, k.Address, k.Name)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&in.Kind, "kind", "", "syslog, otlp or http")
	cmd.Flags().StringVar(&in.Address, "address", "", "where it is")
	cmd.Flags().StringArrayVar(&headers, "header", nil, "a header to send, as Name: value (more than one can be given)")
	cmd.Flags().BoolVar(&logs, "logs", false, "send what apps say (the default, unless --metrics is on its own)")
	cmd.Flags().BoolVar(&in.Metrics, "metrics", false, "send what they do, to an OTLP collector")
	cmd.Flags().StringVar(&in.App, "app", "", "only this app (default: all of them)")
	return cmd
}

func newTelemetryRmCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "rm NAME",
		Short: "Stop sending to a sink",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			return c.RemoveTelemetrySink(cmd.Context(), args[0])
		},
	}
	conn.addFlags(cmd)
	return cmd
}

func newTelemetryTestCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "test NAME",
		Short: "Send something to a sink now, and say how that went",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			if err := c.TestTelemetrySink(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "It got there.")
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}

func newAlertsCmd() *cobra.Command {
	var f appFlags
	var env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "alerts",
		Short: "Say when to be told about how an environment is doing",
		Long: `List an environment's thresholds. Past one for as long as it says, a notification goes to whoever
a rule sends "A threshold is crossed" to, and another when it comes right.`,
		Example: `  moor alerts add error-rate 5 --for 5m
  moor alerts add latency-p95 800 --for 10m --env staging
  moor alerts add memory 90
  moor alerts rm thr_01J…`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			list, err := c.Thresholds(cmd.Context(), app, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s of %s has no thresholds. Add one with: moor alerts add error-rate 5 --for 5m\n", env, app)
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tMETRIC\tABOVE\tFOR\tNOW\tSTATE")
			for _, t := range list {
				now, state := "-", "ok"
				if t.Value != nil {
					now = strconv.FormatFloat(*t.Value, 'f', 1, 64)
				}
				if t.Firing {
					state = "crossed"
				}
				fmt.Fprintf(w, "%s\t%s\t%g\t%dm\t%s\t%s\n", t.ID, t.Metric, t.Above, t.Minutes, now, state)
			}
			return w.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var addFlags appFlags
	var addEnv, lasting string
	add := &cobra.Command{
		Use:   "add METRIC ABOVE",
		Short: "Add a threshold",
		Long: `Add a threshold. METRIC is one of:

  error-rate    the percentage of answers that are 5xx, over the last minute
  latency-p95   how long the slowest 5 in 100 requests took at least, in milliseconds
  memory        the instance using the most, as a percentage of its [resources] limit
  cpu           the instance using the most, as a percentage of one core

A minute with no requests in it, or fewer than 5 for an error rate, says nothing either way.`,
		Example: "  moor alerts add error-rate 5 --for 5m",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			above, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				return fmt.Errorf("%q is not a number", args[1])
			}
			d, err := time.ParseDuration(lasting)
			if err != nil || d < time.Minute {
				return fmt.Errorf("--for takes a time of a minute or more, such as 5m, and not %q", lasting)
			}
			app, c, err := addFlags.open()
			if err != nil {
				return err
			}
			t, err := c.AddThreshold(cmd.Context(), app, addEnv, apitypes.NewThreshold{Metric: args[0], Above: above, Minutes: int(d / time.Minute)})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s above %g for %d minutes tells whoever a rule says. Its ID is %s.\n", t.Label, t.Above, t.Minutes, t.ID)
			return nil
		},
	}
	addFlags.add(add)
	add.Flags().StringVar(&addEnv, "env", names.Production, "environment")
	add.Flags().StringVar(&lasting, "for", "5m", "how long it has to go on for")

	var rmFlags appFlags
	var rmEnv string
	rm := &cobra.Command{
		Use:   "rm ID",
		Short: "Remove a threshold",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := rmFlags.open()
			if err != nil {
				return err
			}
			return c.RemoveThreshold(cmd.Context(), app, rmEnv, args[0])
		},
	}
	rmFlags.add(rm)
	rm.Flags().StringVar(&rmEnv, "env", names.Production, "environment")
	cmd.AddCommand(add, rm)
	return cmd
}
