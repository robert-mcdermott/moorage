package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/names"
)

func newEventsCmd() *cobra.Command {
	var (
		conn           connection
		app, env       string
		limit          int
		follow, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show what happened on the server",
		Long: `Show what happened on the server, oldest first: deployments, releases, instances
that exited or sickened, and changes of variables (their names, never their values).
Events are kept for 90 days. -f goes on showing them as they happen.`,
		Example: `  moor events
  moor events --app shop -n 20
  moor events -f`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			list, err := c.Events(cmd.Context(), app, env, limit)
			if err != nil {
				return err
			}
			slices.Reverse(list) // oldest first, as a log reads
			out := cmd.OutOrStdout()
			if asJSON && !follow {
				return printJSON(out, list)
			}
			after := ""
			if n := len(list); n > 0 {
				after = list[n-1].ID
			}
			if asJSON {
				for _, e := range list {
					if err := printJSON(out, e); err != nil {
						return err
					}
				}
			} else if err := printEvents(out, list); err != nil {
				return err
			}
			if !follow {
				if len(list) == 0 {
					fmt.Fprintln(out, "Nothing has happened yet.")
				}
				return nil
			}
			err = c.FollowEvents(cmd.Context(), app, env, after, func(e apitypes.Event) error {
				if asJSON {
					return printJSON(out, e)
				}
				return printEvents(out, []apitypes.Event{e})
			})
			if errors.Is(err, cmd.Context().Err()) {
				return nil
			}
			return err
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "only what happened to this app")
	f.StringVar(&env, "env", "", "only what happened to this environment")
	f.IntVarP(&limit, "limit", "n", 50, "how many to show")
	f.BoolVarP(&follow, "follow", "f", false, "go on showing them as they happen")
	f.BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func printEvents(out io.Writer, list []apitypes.Event) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, e := range list {
		who := e.App
		if who == "" {
			who = "-"
		}
		if e.Env != "" && e.Env != names.Production {
			who += " · " + e.Env
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.At.Local().Format("Jan 02 15:04:05"), who, e.Summary)
	}
	return w.Flush()
}
