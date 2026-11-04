package cli

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/serverconfig"
)

func newServerConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Write out the server's settings, or bring them in",
		Long: `The server's settings, as a file to keep, to review, or to bring to another server: people, projects,
registries, notifications, backups and sign-in. It has no secrets in it: where one belongs, it names a variable
of the environment. server.toml is apart from this, and apps come from their manifests.`,
	}
	cmd.AddCommand(newServerConfigExportCmd(), newServerConfigImportCmd())
	return cmd
}

func newServerConfigExportCmd() *cobra.Command {
	var (
		conn   connection
		output string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write out the server's settings",
		Example: `  moor server config export > moorage-config.toml
  moor server config export --output moorage-config.toml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			f, err := c.ServerConfig(cmd.Context())
			if err != nil {
				return err
			}
			body, err := serverconfig.Encode(f, ", exported on "+time.Now().UTC().Format("2 January 2006")+" with moor "+buildinfo.Version)
			if err != nil {
				return err
			}
			if output != "" {
				return os.WriteFile(output, body, 0o600)
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file, and not to the standard output")
	return cmd
}

func newServerConfigImportCmd() *cobra.Command {
	var (
		conn   connection
		dryRun bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Make the server match a configuration, in what it mentions",
		Long: `Import makes the server match a file from moor server config export, in what the file mentions. It adds and
changes, and removes nothing. A secret written as ${NAME} is read from this command's environment, and if that is
not set, the server keeps the secret it has.`,
		Example: `  moor server config import moorage-config.toml --dry-run
  MOORAGE_SSO_CLIENT_SECRET=... moor server config import moorage-config.toml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			f, err := serverconfig.Parse(body)
			if err != nil {
				return err
			}
			serverconfig.Expand(&f, os.Getenv)
			c, err := conn.client()
			if err != nil {
				return err
			}
			report, err := c.ImportServerConfig(cmd.Context(), f, dryRun)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), report)
			}
			return printConfigReport(cmd, report)
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only say what this would do")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func printConfigReport(cmd *cobra.Command, report apitypes.ConfigReport) error {
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	counts := map[string]int{}
	for _, item := range report.Items {
		counts[item.Action]++
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Action, item.What, item.Name, item.Note)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if report.DryRun {
		fmt.Fprintf(out, "\nThat would create %d, change %d and leave %d as they are. Nothing has changed.\n", counts[apitypes.ConfigCreate], counts[apitypes.ConfigChange], counts[apitypes.ConfigSame])
	} else {
		fmt.Fprintf(out, "\nCreated %d, changed %d and left %d as they are.\n", counts[apitypes.ConfigCreate], counts[apitypes.ConfigChange], counts[apitypes.ConfigSame])
	}
	if counts[apitypes.ConfigSkip] > 0 {
		fmt.Fprintf(out, "%d left out, for what they need.\n", counts[apitypes.ConfigSkip])
	}
	if counts[apitypes.ConfigFailed] > 0 {
		return fmt.Errorf("%d could not be done", counts[apitypes.ConfigFailed])
	}
	return nil
}
