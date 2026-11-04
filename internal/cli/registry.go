package cli

import (
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

func newRegistryCmd() *cobra.Command {
	var (
		conn   connection
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "List the container registries the server has credentials for",
		Long: `List the container registries the server has credentials for. It uses them to
pull images and to build from private base images. Managing them takes an admin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			registries, err := c.Registries(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), registries)
			}
			if len(registries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "The server has no registry credentials. Public images need none; for a private registry, use moor registry login.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "HOST\tUSERNAME\tADDED")
			for _, r := range registries {
				fmt.Fprintf(w, "%s\t%s\t%s\n", r.Host, r.Username, ago(r.CreatedAt))
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newRegistryLoginCmd(), newRegistryLogoutCmd())
	return cmd
}

func newRegistryLoginCmd() *cobra.Command {
	var (
		conn     connection
		username string
	)
	cmd := &cobra.Command{
		Use:   "login HOST",
		Short: "Store a container registry's credentials on the server",
		Long: `Store a container registry's credentials on the server, encrypted with its
master key. The password is asked for, or read from standard input. Nothing checks
it now: the next pull does.`,
		Example: `  moor registry login ghcr.io --username acme-deploy
  moor registry login registry.example.com:5000 --username ci < password.txt`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if username == "" {
				return usageError{errors.New("which user? pass --username NAME")}
			}
			password, err := readHidden(cmd, "Password for "+username+" at "+args[0], "the password")
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			r, err := c.RegistryLogin(cmd.Context(), args[0], apitypes.RegistryLogin{Username: username, Password: password})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "The server now signs in to %s as %s.\n", r.Host, r.Username)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&username, "username", "", "the user to sign in as")
	return cmd
}

func newRegistryLogoutCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "logout HOST",
		Short: "Remove a container registry's credentials from the server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			if err := c.RegistryLogout(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed the credentials for %s.\n", args[0])
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}
