package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/dotenv"
	"github.com/robert-mcdermott/moorage/internal/names"
)

// envFlags are the flags shared by the env commands.
type envFlags struct {
	conn     connection
	app, env string
}

func (f *envFlags) add(cmd *cobra.Command) {
	f.conn.addFlags(cmd)
	cmd.Flags().StringVar(&f.app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	cmd.Flags().StringVar(&f.env, "env", names.Production, "environment")
}

func newEnvCmd() *cobra.Command {
	var f envFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "env",
		Short: "List and change an app's variables",
		Long: `Variables set on the server configure an app without changing its code.
They override the [env] of moorage.toml, and a variable set for one
environment overrides one set for every environment (--all-envs).

Secret values (--secret) are encrypted on the server and never shown again;
they are kept out of the logs. Every change makes a new release of the
current build, rolled out without downtime (--no-restart keeps the change
for the next deploy).`,
		Example: `  moor env
  moor env set LOG_LEVEL=debug FEATURE_X=on
  moor env set --secret STRIPE_KEY        # asks for the value
  moor env set --secret DB_PASSWORD < password.txt
  moor env unset FEATURE_X
  moor env import .env --secret
  moor env export > production.env`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listVars(cmd, &f, asJSON)
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newEnvListCmd(), newEnvRevealCmd(), newEnvSetCmd(), newEnvUnsetCmd(), newEnvImportCmd(), newEnvExportCmd())
	return cmd
}

func newEnvListCmd() *cobra.Command {
	var f envFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List an app's variables",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listVars(cmd, &f, asJSON)
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func listVars(cmd *cobra.Command, f *envFlags, asJSON bool) error {
	appName, _, err := resolveApp(f.app)
	if err != nil {
		return err
	}
	c, err := f.conn.client()
	if err != nil {
		return err
	}
	vars, err := c.Vars(cmd.Context(), appName, f.env)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(cmd.OutOrStdout(), vars)
	}
	if len(vars) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%s has no variables in %s. Set one with: moor env set NAME=value\n", appName, f.env)
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tVALUE\tSOURCE\tNOTES")
	for _, v := range vars {
		value := v.Value
		if v.Secret {
			value = "(secret)"
		}
		source := map[string]string{"env": f.env, "app": "all environments", "manifest": "moorage.toml", "production": "inherited from production", "declared": "declared, not set"}[v.Source]
		var notes []string
		if v.Build {
			notes = append(notes, "also at build time")
		}
		if v.Overridden {
			notes = append(notes, "overridden")
		}
		if v.Missing {
			notes = append(notes, "required: the next deployment fails without it")
		}
		if v.Description != "" {
			notes = append(notes, v.Description)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Name, oneLine(value), source, strings.Join(notes, ", "))
	}
	return w.Flush()
}

// oneLine shortens a value for a table.
func oneLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > 60 {
		s = s[:59] + "…"
	}
	return s
}

// changeFlags are the flags of the commands that change variables.
type changeFlags struct {
	envFlags
	secret, build, allEnvs, noRestart, productionOnly bool
}

func (f *changeFlags) add(cmd *cobra.Command, setting bool) {
	f.envFlags.add(cmd)
	fl := cmd.Flags()
	if setting {
		fl.BoolVar(&f.secret, "secret", false, "store the values encrypted and never show them again")
		fl.BoolVar(&f.build, "build", false, "make the variables available to builds too")
		fl.BoolVar(&f.productionOnly, "production-only", false, "keep previews from inheriting the variables")
	}
	fl.BoolVar(&f.allEnvs, "all-envs", false, "change the variables of every environment of the app")
	fl.BoolVar(&f.noRestart, "no-restart", false, "keep the change for the next deploy instead of releasing it now")
}

func newEnvSetCmd() *cobra.Command {
	var f changeFlags
	cmd := &cobra.Command{
		Use:   "set NAME=value… | set --secret NAME",
		Short: "Set variables and release the change",
		Long: `Set variables and release the change: a new release of the current build,
rolled out without downtime.

A secret given as NAME alone, without a value, is read from standard input,
or asked for without echoing it, so that it stays out of the shell's history.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			change := apitypes.VarsChange{AllEnvs: f.allEnvs, NoRestart: f.noRestart}
			for _, arg := range args {
				name, value, ok := strings.Cut(arg, "=")
				if !ok {
					if !f.secret || len(args) != 1 {
						return usageError{fmt.Errorf("%q is not NAME=value (only a single --secret NAME is read from standard input)", arg)}
					}
					v, err := readSecret(cmd, name)
					if err != nil {
						return err
					}
					value = v
				}
				change.Set = append(change.Set, apitypes.VarSet{Name: name, Value: value, Secret: f.secret, Build: f.build, ProductionOnly: f.productionOnly})
			}
			return changeVars(cmd, &f.envFlags, change)
		},
	}
	f.add(cmd, true)
	return cmd
}

// readSecret reads a secret's value: asked for without echo on a terminal,
// otherwise all of standard input without its last line ending.
func readSecret(cmd *cobra.Command, name string) (string, error) {
	return readHidden(cmd, "Value of "+name, name)
}

// readHidden is readSecret, asking for name with prompt.
func readHidden(cmd *cobra.Command, prompt, name string) (string, error) {
	if file, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: ", prompt)
		value, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return string(value), nil
	}
	data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if value == "" {
		return "", fmt.Errorf("no value for %s on standard input", name)
	}
	return value, nil
}

func newEnvUnsetCmd() *cobra.Command {
	var f changeFlags
	cmd := &cobra.Command{
		Use:   "unset NAME…",
		Short: "Unset variables and release the change",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return changeVars(cmd, &f.envFlags, apitypes.VarsChange{Unset: args, AllEnvs: f.allEnvs, NoRestart: f.noRestart})
		},
	}
	f.add(cmd, false)
	return cmd
}

func newEnvImportCmd() *cobra.Command {
	var f changeFlags
	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Set the variables of a .env file",
		Long: `Set the variables of a .env file (NAME=value lines; # comments; values may
be quoted, and "export " in front of a line is allowed) in one change.
Use - to read standard input.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := cmd.InOrStdin()
			if args[0] != "-" {
				file, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer file.Close()
				r = file
			}
			vars, err := dotenv.Parse(r)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			if len(vars) == 0 {
				return fmt.Errorf("%s sets no variables", args[0])
			}
			change := apitypes.VarsChange{AllEnvs: f.allEnvs, NoRestart: f.noRestart}
			for _, kv := range vars {
				change.Set = append(change.Set, apitypes.VarSet{Name: kv.Name, Value: kv.Value, Secret: f.secret, Build: f.build})
			}
			return changeVars(cmd, &f.envFlags, change)
		},
	}
	f.add(cmd, true)
	return cmd
}

func newEnvRevealCmd() *cobra.Command {
	var f envFlags
	cmd := &cobra.Command{
		Use:   "reveal NAME",
		Short: "Print a secret's value",
		Long: `Print a secret's value. It takes being a maintainer of the app's project, and is
recorded in the audit log.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appName, _, err := resolveApp(f.app)
			if err != nil {
				return err
			}
			c, err := f.conn.client()
			if err != nil {
				return err
			}
			revealed, err := c.RevealVar(cmd.Context(), appName, f.env, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), revealed.Value)
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func newEnvExportCmd() *cobra.Command {
	var f envFlags
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Print the variables set on the server as a .env file",
		Long: `Print the variables set on the server for an environment, as a .env file.
Secrets are listed as comments, without their values.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			appName, _, err := resolveApp(f.app)
			if err != nil {
				return err
			}
			c, err := f.conn.client()
			if err != nil {
				return err
			}
			vars, err := c.Vars(cmd.Context(), appName, f.env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "# Variables of %s (%s)\n", appName, f.env)
			for _, v := range vars {
				switch {
				case v.Source == "manifest" || v.Overridden:
				case v.Secret:
					fmt.Fprintf(out, "# %s is a secret: its value is not shown\n", v.Name)
				default:
					fmt.Fprintf(out, "%s=%s\n", v.Name, dotenv.Quote(v.Value))
				}
			}
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func changeVars(cmd *cobra.Command, f *envFlags, change apitypes.VarsChange) error {
	appName, _, err := resolveApp(f.app)
	if err != nil {
		return err
	}
	c, err := f.conn.client()
	if err != nil {
		return err
	}
	res, err := c.ChangeVars(cmd.Context(), appName, f.env, change)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if res.AppCreated {
		fmt.Fprintf(out, "Created app %s.\n", appName)
	}
	for _, name := range res.NotSet {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s was not set\n", name)
	}
	scope := f.env
	if change.AllEnvs {
		scope = "every environment"
	}
	var done []string
	for _, v := range change.Set {
		label := v.Name
		if v.Secret {
			label += " (secret)"
		}
		done = append(done, label)
	}
	if len(done) > 0 {
		fmt.Fprintf(out, "Set %s for %s of %s.\n", strings.Join(done, ", "), scope, appName)
	}
	if unset := len(change.Unset) - len(res.NotSet); unset > 0 {
		fmt.Fprintf(out, "Unset %d variable%s for %s of %s.\n", unset, plural(unset), scope, appName)
	}
	switch {
	case res.Deployment != nil && res.Deployment.Release != nil:
		fmt.Fprintf(out, "Released v%d with the new variables.\n", res.Deployment.Release.Version)
	case change.NoRestart:
		fmt.Fprintln(out, "Not released: the next deploy will use them.")
	default:
		fmt.Fprintln(out, "Nothing is deployed yet: the first deploy will use them.")
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
