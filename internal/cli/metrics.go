package cli

import (
	"fmt"
	"math"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/names"
)

// sparkline draws values in a line of text, each a little block as high as it is.
func sparkline(values []float64, width int) string {
	if len(values) == 0 {
		return ""
	}
	if len(values) > width {
		merged := make([]float64, width)
		for i, v := range values {
			j := i * width / len(values)
			merged[j] = math.Max(merged[j], v)
		}
		values = merged
	}
	top := 0.0
	for _, v := range values {
		top = math.Max(top, v)
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range values {
		i := 0
		if top > 0 {
			i = int(v / top * float64(len(blocks)-1))
		}
		b.WriteRune(blocks[i])
	}
	return b.String()
}

func metricText(unit string, v float64) string {
	switch unit {
	case "rate":
		return fmt.Sprintf("%.1f/min", v)
	case "ms":
		if v >= 1000 {
			return fmt.Sprintf("%.2f s", v/1000)
		}
		return fmt.Sprintf("%.0f ms", v)
	case "percent":
		return fmt.Sprintf("%.1f%%", v)
	case "bytes":
		return humanBytes(int64(v))
	}
	return fmt.Sprintf("%g", v)
}

func newMetricsCmd() *cobra.Command {
	var f appFlags
	var env, span, series string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Show what an environment has been doing",
		Long: `Show an environment's requests, response times, CPU and memory, as a line of text for each,
with its latest value. --range is live, 1h, 24h, 7d or 30d. The console's Metrics tab has the
same as charts.`,
		Example: `  moor metrics
  moor metrics --range 24h --series latency
  moor metrics --env staging --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			m, err := c.Metrics(cmd.Context(), app, env, span)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), m)
			}
			out := cmd.OutOrStdout()
			shown := 0
			for _, chart := range m.Charts {
				if series != "" && chart.Key != series {
					continue
				}
				if len(chart.Lines) == 0 {
					continue
				}
				shown++
				fmt.Fprintln(out, chart.Title)
				for _, line := range chart.Lines {
					values := make([]float64, 0, len(line.Points))
					for _, p := range line.Points {
						values = append(values, p.Value)
					}
					latest := "-"
					if n := len(values); n > 0 {
						latest = metricText(chart.Unit, values[n-1])
					}
					fmt.Fprintf(out, "  %-14s %s  %s\n", line.Name, sparkline(values, 60), latest)
				}
				fmt.Fprintln(out)
			}
			if shown == 0 {
				fmt.Fprintf(out, "%s of %s has nothing to show for that yet.\n", env, app)
			}
			if len(m.Deployments) > 0 {
				versions := make([]string, 0, len(m.Deployments))
				for _, d := range m.Deployments {
					versions = append(versions, fmt.Sprintf("v%d", d.Version))
				}
				fmt.Fprintf(out, "Released meanwhile: %s\n", strings.Join(versions, ", "))
			}
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().StringVar(&span, "range", "1h", "live, 1h, 24h, 7d or 30d")
	cmd.Flags().StringVar(&series, "series", "", "only requests, latency, cpu or memory")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}
