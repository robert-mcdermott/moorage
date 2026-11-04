package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

func newTokensCmd() *cobra.Command {
	var (
		conn   connection
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "List your API tokens",
		Long: `List your API tokens: for scripts and CI, and for git over HTTPS. Create one
with 'moor tokens create NAME', revoke one with 'moor tokens revoke NAME'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			tokens, err := c.Tokens(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), tokens)
			}
			if len(tokens) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No tokens yet. Create one with: moor tokens create NAME")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tSCOPES\tAPP\tEXPIRES\tLAST USED")
			for _, t := range tokens {
				app, expires, used := "-", "never", "never"
				if t.App != "" {
					app = t.App
				}
				if !t.ExpiresAt.IsZero() {
					expires = t.ExpiresAt.Format(time.DateOnly)
				}
				if !t.LastUsedAt.IsZero() {
					used = ago(t.LastUsedAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, strings.Join(t.Scopes, ","), app, expires, used)
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newTokensCreateCmd(), newTokensRevokeCmd())
	return cmd
}

func newTokensCreateCmd() *cobra.Command {
	var (
		conn    connection
		scopes  []string
		app     string
		expires string
	)
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an API token; it is shown this once",
		Long: `Create an API token. Its scope is what it may do, up to what you may do:

  read    see apps, deployments, releases and logs; fetch their repositories
  deploy  also deploy, restart, roll back, push, and see variables
  admin   also change variables, and manage your tokens and keys

--app restricts it to one app, and --expires makes it stop working after a
time, such as 90d.`,
		Example: `  moor tokens create ci --scope deploy --app shop --expires 90d`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := apitypes.NewToken{Name: args[0], Scopes: scopes, App: app}
			if expires != "" {
				var d manifest.Duration
				if err := d.UnmarshalText([]byte(expires)); err != nil {
					return err
				}
				req.ExpiresAt = time.Now().Add(d.Std()).UTC()
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			t, err := c.CreateToken(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), t.Secret)
			fmt.Fprintf(cmd.ErrOrStderr(), "Created the token %s (%s). This is the only time it is shown.\n", t.Name, strings.Join(t.Scopes, ", "))
			return nil
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.StringSliceVar(&scopes, "scope", []string{"deploy"}, "what it may do: read, deploy or admin")
	f.StringVar(&app, "app", "", "restrict it to one app")
	f.StringVar(&expires, "expires", "", "stop working after this long, such as 90d")
	return cmd
}

func newTokensRevokeCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "revoke NAME",
		Short: "Revoke an API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			t, err := c.RevokeToken(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked the token %s: it no longer works.\n", t.Name)
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}
