package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/names"
)

func printDomain(cmd *cobra.Command, d apitypes.Domain) {
	out := cmd.OutOrStdout()
	if d.Verified {
		fmt.Fprintf(out, "%s points here, and serves.\n", d.Address)
		return
	}
	fmt.Fprintf(out, "%s does not point here yet", d.Address)
	if d.Problem != "" {
		fmt.Fprintf(out, ": %s", d.Problem)
	}
	fmt.Fprintf(out, ".\n\nThe server looks again every few minutes, or: moor domains verify %s\n", d.Address)
}

func newDomainsCmd() *cobra.Command {
	var f appFlags
	var env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "domains",
		Short: "List and attach hostnames of your own",
		Long: `Attach a hostname of your own to an environment, or a path on one, so that several
apps can share a hostname. Its DNS must point here before it serves: a CNAME to the
environment's own hostname, or an address of this server.

A path is kept in what the app hears, which X-Forwarded-Prefix also says, unless
--strip-prefix takes it away. Redirects and cookies from the app are made to fit it.
Links in what the app sends are its own affair.`,
		Example: `  moor domains add portal.example.com
  moor domains add tools.example.com/reports --app reports
  moor domains verify portal.example.com
  moor domains remove portal.example.com`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			list, err := c.Domains(cmd.Context(), app, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s of %s has no domains of its own. Attach one with: moor domains add portal.example.com\n", env, app)
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "DOMAIN\tSTATE\tDNS")
			for _, d := range list {
				state := "not yet pointing here"
				if d.Verified {
					state = "serving"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", d.Address, state, d.Record)
			}
			return w.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newDomainAddCmd(), newDomainVerifyCmd(), newDomainRemoveCmd())
	return cmd
}

func newDomainAddCmd() *cobra.Command {
	var f appFlags
	var env string
	var strip bool
	cmd := &cobra.Command{
		Use:   "add HOSTNAME[/PATH]",
		Short: "Attach a domain, which serves once its DNS points here",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			d, err := c.AddDomain(cmd.Context(), app, env, apitypes.NewDomain{Address: args[0], StripPrefix: strip})
			if err != nil {
				return err
			}
			printDomain(cmd, d)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&strip, "strip-prefix", false, "take the path away from what the app hears")
	return cmd
}

func newDomainVerifyCmd() *cobra.Command {
	var f appFlags
	var env string
	cmd := &cobra.Command{
		Use:   "verify HOSTNAME[/PATH]",
		Short: "Look now at where a domain points",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			d, err := c.VerifyDomain(cmd.Context(), app, env, args[0])
			if err != nil {
				return err
			}
			printDomain(cmd, d)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	return cmd
}

func newDomainRemoveCmd() *cobra.Command {
	var f appFlags
	var env string
	cmd := &cobra.Command{
		Use:   "remove HOSTNAME[/PATH]",
		Short: "Detach a domain, at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if err := c.RemoveDomain(cmd.Context(), app, env, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Detached %s.\n", args[0])
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	return cmd
}
