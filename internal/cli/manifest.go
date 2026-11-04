package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

func newManifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Check and explain moorage.toml",
		Long: `moorage.toml tells Moorage how to build and run an app. These commands
check it, show what Moorage will do with an app, and print the manifest's
JSON Schema for editors.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newManifestValidateCmd(), newManifestExplainCmd(), newManifestSchemaCmd())
	return cmd
}

func newManifestValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [DIR]",
		Short: "Check moorage.toml",
		Long: `Check the moorage.toml in DIR (default: the current directory): its syntax,
keys and values. Warnings point at values that look like credentials and at
settings this version accepts but does not act on yet.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := firstOf(firstArg(args), ".")
			m, err := manifest.Load(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			path := filepath.Join(dir, manifest.FileName)
			if m == nil {
				fmt.Fprintf(out, "There is no %s: Moorage detects how to build the app (see moor manifest explain).\n", path)
				return nil
			}
			warnings := manifest.Lint(m)
			for _, w := range warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			fmt.Fprintf(out, "%s is valid", path)
			if len(warnings) > 0 {
				fmt.Fprintf(out, " (%d warnings)", len(warnings))
			}
			fmt.Fprintln(out, ".")
			return nil
		},
	}
}

func newManifestExplainCmd() *cobra.Command {
	var asJSON bool
	var goos string
	cmd := &cobra.Command{
		Use:   "explain [DIR]",
		Short: "Show how Moorage will build and run an app",
		Long: `Show the plan Moorage makes for the app in DIR (default: the current
directory) from its moorage.toml and its files: the runtime, the toolchains,
the build commands, the processes and their environment. The server makes
the same plan when it builds the app; toolchain versions are resolved then.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := firstOf(firstArg(args), ".")
			plan, _, err := build.Resolve(dir, goos)
			if err != nil {
				var u *build.UnresolvedError
				if errors.As(err, &u) {
					return errors.New(u.Msg)
				}
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), plan)
			}
			manifest.Explain(cmd.OutOrStdout(), plan)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the plan as JSON")
	cmd.Flags().StringVar(&goos, "os", runtime.GOOS, "operating system of the server (linux, darwin or windows)")
	return cmd
}

func newManifestSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of moorage.toml",
		Long: `Print the JSON Schema of moorage.toml, for editors that validate and
complete TOML files (for example with a "#:schema ./moorage.schema.json"
comment at the top of the manifest).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(manifest.Schema())
			return err
		},
	}
}
