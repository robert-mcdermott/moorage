package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/server"
)

// connection holds the flags of commands that talk to a server.
type connection struct {
	server, token, caCert string
}

func (c *connection) addFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&c.server, "server", "", "server URL (default: $MOOR_SERVER, or the server configured on this machine)")
	f.StringVar(&c.token, "token", "", "API token (default: $MOOR_TOKEN, or the admin token of the server on this machine)")
	f.StringVar(&c.caCert, "ca-cert", "", "PEM file of an extra CA to trust, such as the server's internal CA (default: $MOOR_CA_CERT)")
}

// client connects to --server or $MOOR_SERVER; without either, to the
// server configured on this machine, over its loopback address, with its
// admin token when this user can read it.
func (c *connection) client() (*client.Client, error) {
	serverURL := firstOf(c.server, os.Getenv("MOOR_SERVER"))
	token := firstOf(c.token, os.Getenv("MOOR_TOKEN"))
	if serverURL == "" || token == "" {
		localURL, localToken, err := localServer()
		if serverURL == "" {
			if err != nil {
				return nil, fmt.Errorf("no server to talk to: pass --server and --token, or set MOOR_SERVER and MOOR_TOKEN (%v)", err)
			}
			serverURL = localURL
		}
		if token == "" {
			token = localToken
		}
	}
	return client.New(serverURL, token, firstOf(c.caCert, os.Getenv("MOOR_CA_CERT")))
}

// localServer returns the loopback URL of the server configured on this
// machine and its admin token, if readable.
func localServer() (url, token string, err error) {
	path, err := configPath("")
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", "", fmt.Errorf("no server is configured on this machine at %s", path)
	}
	cfg, err := config.Load(path, os.Getenv)
	if err != nil {
		return "", "", err
	}
	host, port, err := net.SplitHostPort(cfg.Listen.HTTP)
	if err != nil {
		return "", "", err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if data, err := os.ReadFile(server.NewLayout(cfg.DataDir).AdminToken); err == nil {
		token = strings.TrimSpace(string(data))
	}
	return "http://" + net.JoinHostPort(host, port), token, nil
}

// resolveApp picks the app from --app, $MOOR_APP or [app].name in the
// current directory's moorage.toml, which it also returns.
func resolveApp(flag string) (string, *manifest.Manifest, error) {
	m, err := manifest.Load(".")
	if err != nil {
		return "", nil, err
	}
	app := firstOf(flag, os.Getenv("MOOR_APP"))
	if app == "" && m != nil {
		app = m.App.Name
	}
	if app == "" {
		return "", m, errors.New("which app? pass --app NAME, set MOOR_APP, or set name under [app] in moorage.toml")
	}
	return app, m, names.ValidateApp(app)
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func newDeployCmd() *cobra.Command {
	var (
		conn             connection
		app, env         string
		static, artifact bool
	)
	cmd := &cobra.Command{
		Use:   "deploy [DIR]",
		Short: "Deploy to an app",
		Long: `Deploy to an app's production environment. The app is created if it does
not exist yet.

With --static, the files under DIR are uploaded and served as they are,
following the [static] rules of moorage.toml in the current directory.
DIR defaults to [static].dir in moorage.toml, or the current directory.

With --artifact, DIR (default: the current directory) holds an app that is
ready to run, such as a compiled binary; the [processes] of moorage.toml in
the current directory say how to run it. The new release takes over once
its instances are healthy; until then the previous release keeps serving.

Building apps from their source arrives in a later release.`,
		Example: `  moor deploy --static ./public --app docs
  moor deploy --artifact ./dist --app api
  moor deploy --static              # with [app].name and [static].dir in moorage.toml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if static == artifact {
				return usageError{errors.New("pass --static (a directory of files served as they are) or --artifact (an app ready to run); building from source arrives in a later release")}
			}
			appName, m, err := resolveApp(app)
			if err != nil {
				return err
			}
			var plan manifest.Plan
			var dir string
			if static {
				rules := manifest.Static{}
				if m != nil && m.Static != nil {
					rules = *m.Static
				}
				dir = firstOf(firstArg(args), rules.Dir, ".")
				rules.Dir = ""
				plan = manifest.Plan{Kind: manifest.KindStatic, Static: &rules}
			} else {
				if m == nil || len(m.Processes) == 0 {
					return errors.New("--artifact needs [processes] in moorage.toml in the current directory, such as:\n\n  [processes.web]\n  command = \"./server --port $PORT\"")
				}
				dir = firstOf(firstArg(args), ".")
				plan = manifest.Plan{Kind: manifest.KindProcess, Processes: m.Processes, Deploy: m.Deploy, Env: m.Env}
			}
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			skip := func(rel string, d fs.DirEntry) bool {
				return d.Name() == ".DS_Store" || (dir == "." && rel == manifest.FileName)
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Deploying %s to %s on %s\n", dir, appName, c.Server())
			d, stats, err := c.Deploy(cmd.Context(), appName, env, dir, plan, skip)
			for _, s := range stats.Skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %s: only regular files are uploaded\n", s)
			}
			if err != nil {
				return err
			}
			if d.AppCreated {
				fmt.Fprintf(out, "Created app %s.\n", d.App)
			}
			fmt.Fprintf(out, "Deployed %s v%d: %d files, %s.\n\n  %s\n", d.App, d.Release.Version, d.Release.Files, humanBytes(d.Release.Size), d.URL)
			return nil
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.BoolVar(&static, "static", false, "upload a directory of files and serve them as they are")
	f.BoolVar(&artifact, "artifact", false, "upload an app that is ready to run and run its [processes]")
	f.StringVar(&app, "app", "", "app to deploy to (default: $MOOR_APP or [app].name in moorage.toml)")
	f.StringVar(&env, "env", names.Production, "environment to deploy to")
	return cmd
}

func newAppsCmd() *cobra.Command {
	var conn connection
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "List the apps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			apps, err := c.Apps(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), apps)
			}
			if len(apps) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No apps yet. Deploy one with: moor deploy --static DIR --app NAME")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tURL\tRELEASE\tDEPLOYED")
			for _, a := range apps {
				release, deployed := "-", "-"
				if a.Production != nil {
					release = "v" + strconv.Itoa(a.Production.Version)
					deployed = ago(a.Production.CreatedAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Name, a.URL, release, deployed)
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newReleasesCmd() *cobra.Command {
	var conn connection
	var app, env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "releases",
		Short: "List an app's releases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			appName, _, err := resolveApp(app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			rels, err := c.Releases(cmd.Context(), appName, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), rels)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "\tRELEASE\tREASON\tFILES\tSIZE\tCREATED")
			for _, r := range rels {
				mark := ""
				if r.Current {
					mark = "*"
				}
				fmt.Fprintf(w, "%s\tv%d\t%s\t%d\t%s\t%s\n", mark, r.Version, r.Reason, r.Files, humanBytes(r.Size), ago(r.CreatedAt))
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newRollbackCmd() *cobra.Command {
	var conn connection
	var app, env string
	cmd := &cobra.Command{
		Use:   "rollback [vN]",
		Short: "Serve an earlier release again",
		Long: `Publish an earlier release again, as a new release. Without a version, the
release before the current one. No rebuild is needed: it takes seconds.`,
		Example: `  moor rollback --app docs
  moor rollback v3 --app docs`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version := 0
			if len(args) == 1 {
				v, err := strconv.Atoi(strings.TrimPrefix(args[0], "v"))
				if err != nil || v <= 0 {
					return usageError{fmt.Errorf("%q is not a release version such as v3", args[0])}
				}
				version = v
			}
			appName, _, err := resolveApp(app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			d, err := c.Rollback(cmd.Context(), appName, env, version)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s now serves v%d (the build %s).\n\n  %s\n", d.App, d.Release.Version, d.Release.Build, d.URL)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	return cmd
}

func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	}
	return t.Local().Format("2006-01-02")
}
