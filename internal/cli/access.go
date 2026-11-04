package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/names"
)

func printAccess(cmd *cobra.Command, app, env string, a apitypes.Access) {
	out := cmd.OutOrStdout()
	from := map[string]string{"setting": "set here", "manifest": "from its moorage.toml", "server": "the server's default"}[a.Source]
	fmt.Fprintf(out, "%s of %s\n\nGate      %s (%s)\n", env, app, a.Mode, from)
	if a.Mode == "login" {
		who := "those with a role in its project"
		if a.AnyUser {
			who = "anyone with an account"
		}
		fmt.Fprintf(out, "Who       %s\n", who)
	}
	if len(a.Allow) > 0 {
		fmt.Fprintf(out, "Only from %s\n", strings.Join(a.Allow, ", "))
	}
	if a.AskedInVain {
		fmt.Fprintf(out, "\nIts moorage.toml asks for no gate, which only a maintainer can allow: moor access set public\n")
	}
	if a.PasswordMissing {
		fmt.Fprintf(out, "\nIt asks for a password nobody has set, so nobody can reach it. Set one with: moor access set password\n")
	}
}

func newAccessCmd() *cobra.Command {
	var f appFlags
	var env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "access",
		Short: "Show and change who may reach an environment",
		Long: `Each environment has a gate, which applies to everything it serves:

  public    anyone
  password  a shared password; scripts send it with HTTP Basic
  login     people signed in to Moorage, with a role in its project, or with
            --any-user anyone with an account; the app hears who they are, and
            scripts send an API token with the gate scope
  share     only people with a share link

A share link works with every gate. A list of addresses can be combined with any
of them. Unless one is set here, the gate is what the manifest asks, or else the
server's default for that kind of environment.`,
		Example: `  moor access
  moor access set password --env staging
  moor access set login --allow-cidr 10.0.0.0/8
  moor access set default --env staging
  moor share --expires 3d`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			a, err := c.Access(cmd.Context(), app, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), a)
			}
			printAccess(cmd, app, env, a)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newAccessSetCmd())
	return cmd
}

func newAccessSetCmd() *cobra.Command {
	var f appFlags
	var env string
	var allow []string
	var anyUser, newPassword, anywhere bool
	cmd := &cobra.Command{
		Use:   "set public|password|login|share|default",
		Short: "Change an environment's gate, at once",
		Long: `Change an environment's gate, at once. default leaves it to the manifest and the
server. The password gate asks for its password, without echoing it, or reads it
from standard input. Changing the gate or its password ends everyone's way in.

--allow-cidr replaces the list of addresses it can be reached from, and --anywhere
takes the list away. Otherwise the list stays as it is.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			change := apitypes.AccessChange{Mode: args[0], AnyUser: anyUser}
			if len(allow) > 0 {
				change.Allow = &allow
			}
			if anywhere {
				change.Allow = &[]string{}
			}
			if change.Mode == "default" {
				change.Mode = ""
			}
			if change.Mode == "password" || newPassword {
				if change.Password, err = readHidden(cmd, "Password for "+env+" of "+app, "the password"); err != nil {
					return err
				}
			}
			a, err := c.SetAccess(cmd.Context(), app, env, change)
			if err != nil {
				return err
			}
			printAccess(cmd, app, env, a)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().StringSliceVar(&allow, "allow-cidr", nil, "addresses it can be reached from, such as 10.0.0.0/8")
	cmd.Flags().BoolVar(&anywhere, "anywhere", false, "take away the list of addresses")
	cmd.Flags().BoolVar(&anyUser, "any-user", false, "with login, let in anyone with an account")
	cmd.Flags().BoolVar(&newPassword, "password", false, "set the password too, for when the manifest asks for the gate")
	return cmd
}

func newShareCmd() *cobra.Command {
	var f appFlags
	var env, expires string
	var maxUses int
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Make a link that lets someone reach an environment",
		Long: `Make a link that lets someone reach an environment, whatever its gate, without an
account or a password. It is shown only now. It lasts seven days unless you say
otherwise, up to a year, and can be revoked.

A link with --max-uses asks for a click, so that what fetches addresses to preview
them does not use it up.`,
		Example: `  moor share --expires 3d --env staging
  moor share --max-uses 1
  moor share list
  moor share revoke shr_01J…`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			l, err := c.CreateShareLink(cmd.Context(), app, env, apitypes.NewShareLink{Expires: expires, MaxUses: maxUses})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", l.URL)
			fmt.Fprintf(cmd.ErrOrStderr(), "\nIt lasts until %s, and is shown only now.\n", l.ExpiresAt.Local().Format("2006-01-02 15:04"))
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().StringVar(&expires, "expires", "", "how long it lasts, such as 3d or 12h (default 7d)")
	cmd.Flags().IntVar(&maxUses, "max-uses", 0, "how many times it can be used (default: no limit)")
	cmd.AddCommand(newShareListCmd(), newShareRevokeCmd())
	return cmd
}

func newShareListCmd() *cobra.Command {
	var f appFlags
	var env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List an environment's share links",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			list, err := c.ShareLinks(cmd.Context(), app, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No share links. Make one with: moor share")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "ID\tCREATED BY\tEXPIRES\tUSES\tSTATE")
			for _, l := range list {
				state := "in use"
				if !l.RevokedAt.IsZero() {
					state = "revoked"
				}
				uses := fmt.Sprint(l.Uses)
				if l.MaxUses > 0 {
					uses += fmt.Sprintf(" of %d", l.MaxUses)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", l.ID, l.CreatedBy, l.ExpiresAt.Local().Format("2006-01-02"), uses, state)
			}
			return w.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newShareRevokeCmd() *cobra.Command {
	var f appFlags
	var env string
	cmd := &cobra.Command{
		Use:   "revoke ID",
		Short: "End a share link, and within a minute everyone it let in",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if err := c.RevokeShareLink(cmd.Context(), app, env, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s.\n", args[0])
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	return cmd
}
