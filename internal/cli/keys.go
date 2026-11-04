package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

func newKeysCmd() *cobra.Command {
	var (
		conn   connection
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "List the SSH keys that may push",
		Long: `List the SSH public keys that may push to the apps' repositories over SSH.
Register yours with 'moor keys add', remove one with 'moor keys remove'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			keys, err := c.SSHKeys(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), keys)
			}
			if len(keys) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No SSH keys yet. Register yours with: moor keys add")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tFINGERPRINT\tADDED\tLAST USED")
			for _, k := range keys {
				used := "never"
				if !k.LastUsedAt.IsZero() {
					used = ago(k.LastUsedAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.Name, k.Fingerprint, ago(k.CreatedAt), used)
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newKeysAddCmd(), newKeysRemoveCmd())
	return cmd
}

func newKeysAddCmd() *cobra.Command {
	var (
		conn connection
		name string
	)
	cmd := &cobra.Command{
		Use:   "add [FILE]",
		Short: "Register an SSH public key",
		Long: `Register an SSH public key, so that git pushes over SSH signed with it are
accepted. FILE is a .pub file, or - for standard input; without it, the
first of ~/.ssh/id_ed25519.pub, id_ecdsa.pub and id_rsa.pub. The key is
named after its comment unless --name says otherwise.`,
		Example: `  moor keys add
  moor keys add ~/.ssh/work.pub --name work-laptop`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file := ""
			if len(args) == 1 {
				file = args[0]
			}
			data, from, err := readPublicKey(file, cmd.InOrStdin())
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			k, err := c.AddSSHKey(cmd.Context(), apitypes.NewSSHKey{Name: name, PublicKey: string(data)})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Registered %s as %s (%s).\n", from, k.Name, k.Fingerprint)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&name, "name", "", "the key's name (default: its comment)")
	return cmd
}

func newKeysRemoveCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "remove NAME|FINGERPRINT",
		Short: "Remove an SSH key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			k, err := c.RemoveSSHKey(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s (%s): it can no longer push.\n", k.Name, k.Fingerprint)
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}

// readPublicKey reads a public key from file, standard input ("-"), or the
// user's default key, and says where it came from.
func readPublicKey(file string, stdin io.Reader) ([]byte, string, error) {
	if file == "-" {
		data, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
		return data, "the key", err
	}
	if file != "" {
		data, err := os.ReadFile(file)
		return data, file, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	for _, n := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		p := filepath.Join(home, ".ssh", n)
		if data, err := os.ReadFile(p); err == nil {
			return data, p, nil
		}
	}
	return nil, "", errors.New("no key in ~/.ssh (id_ed25519.pub, id_ecdsa.pub, id_rsa.pub): make one with ssh-keygen -t ed25519, or name a .pub file")
}
