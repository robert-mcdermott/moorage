package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

func newServerUsersCmd() *cobra.Command {
	var (
		conn   connection
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "users",
		Short: "List the server's users",
		Long: `List the server's users. An admin manages users; until projects arrive, every
user may do everything with apps.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			users, err := c.Users(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), users)
			}
			if len(users) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No users yet: claim the server with the setup link that moor server status shows.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "EMAIL\tNAME\tROLE\tSTATE\tCREATED")
			for _, u := range users {
				state := "active"
				if u.Disabled {
					state = "disabled"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", u.Email, u.Name, u.Role, state, ago(u.CreatedAt))
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newUsersCreateCmd(), newUsersInviteCmd(), newUsersRoleCmd(),
		newUsersEnabledCmd("disable", "Disable a user: their sessions end, and their tokens and keys stop working", true),
		newUsersEnabledCmd("enable", "Enable a disabled user again", false))
	return cmd
}

func printInvitation(cmd *cobra.Command, inv apitypes.Invitation, what string) {
	fmt.Fprintf(cmd.OutOrStdout(), "%s Send them this link, to choose a password. It works once, until %s:\n\n  %s\n",
		what, inv.ExpiresAt.Local().Format("2 Jan 15:04"), inv.URL)
}

func newUsersCreateCmd() *cobra.Command {
	var (
		conn  connection
		name  string
		admin bool
	)
	cmd := &cobra.Command{
		Use:   "create EMAIL",
		Short: "Create a user, and print a link for them to choose a password",
		Example: `  moor server users create ada@example.com --name "Ada Lovelace"
  moor server users create ops@example.com --admin`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			req := apitypes.NewUser{Email: args[0], Name: name}
			if admin {
				req.Role = "admin"
			}
			inv, err := c.CreateUser(cmd.Context(), req)
			if err != nil {
				return err
			}
			printInvitation(cmd, inv, fmt.Sprintf("Created %s (%s).", inv.User.Email, inv.User.Role))
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&name, "name", "", "their name (default: the first part of the address)")
	cmd.Flags().BoolVar(&admin, "admin", false, "make them an admin")
	return cmd
}

func newUsersInviteCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "invite EMAIL",
		Short: "Print a new link for a user to choose a password",
		Long: `Print a new one-time link for a user to choose a password: for one who has
forgotten theirs, or whose first link has expired. It replaces any earlier link,
and using it ends their sessions.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			inv, err := c.InviteUser(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printInvitation(cmd, inv, "A new link for "+inv.User.Email+".")
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}

func newUsersRoleCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:       "role EMAIL admin|member",
		Short:     "Change a user's role",
		Args:      cobra.ExactArgs(2),
		ValidArgs: []string{"admin", "member"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			u, err := c.ChangeUser(cmd.Context(), args[0], apitypes.UserChange{Role: &args[1]})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s.\n", u.Email, u.Role)
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}

func newUsersEnabledCmd(use, short string, disabled bool) *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   use + " EMAIL",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			u, err := c.ChangeUser(cmd.Context(), args[0], apitypes.UserChange{Disabled: &disabled})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %sd.\n", u.Email, use)
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}
