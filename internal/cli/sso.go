package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newServerSSOCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "sso",
		Short: "Say how people sign in through a provider",
		Long: `Say how people sign in through an OpenID Connect provider, which is set up in the console, in
Settings → Sign-in.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			got, err := c.SSO(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if got.Issuer == "" {
				fmt.Fprintln(out, "Single sign-on is not set up. Everyone signs in with a password.")
				return nil
			}
			fmt.Fprintf(out, "Provider:  %s\nClient ID: %s\nPasswords: ", got.Issuer, got.ClientID)
			if got.PasswordsOff {
				fmt.Fprintln(out, "off")
			} else {
				fmt.Fprintln(out, "on")
			}
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.AddCommand(newServerSSOOffCmd())
	return cmd
}

func newServerSSOOffCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "off",
		Short: "Turn off single sign-on, which gives passwords back",
		Long: `Turn off signing in through a provider, which gives passwords back: the way in if your provider
has gone away. On the server's own machine this needs no sign-in, and those who signed in only
through the provider need a new link for a password, from an administrator.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			if err := c.TurnOffSSO(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Single sign-on is off, and everyone signs in with a password.")
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}
