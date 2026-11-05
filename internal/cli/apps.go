package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/ignore"
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

// client connects as resolve says.
func (c *connection) client() (*client.Client, error) {
	serverURL, token, caCert, err := c.resolve()
	if err != nil {
		return nil, err
	}
	return client.New(serverURL, token, caCert)
}

// resolve picks the server and credentials: --server and --token, or
// $MOOR_SERVER and $MOOR_TOKEN; failing those, the current context (or, given
// only a server, its context); failing that, the server configured on this
// machine, over its loopback address, with its admin token when this user can
// read it. $MOORAGE_CONFIG names that server on purpose, so it comes before the
// current context.
func (c *connection) resolve() (serverURL, token, caCert string, err error) {
	serverURL = firstOf(c.server, os.Getenv("MOOR_SERVER"))
	token = firstOf(c.token, os.Getenv("MOOR_TOKEN"))
	caCert = firstOf(c.caCert, os.Getenv("MOOR_CA_CERT"))
	cfg, err := loadClientConfig()
	if err != nil {
		return "", "", "", err
	}
	if token == "" {
		var ctx *clientContext
		switch {
		case serverURL != "":
			ctx = cfg.forServer(serverURL)
		case os.Getenv("MOORAGE_CONFIG") == "":
			ctx = cfg.current()
		}
		if ctx != nil {
			serverURL, token = ctx.Server, ctx.Token
			caCert = firstOf(caCert, ctx.CACert)
		}
	}
	if serverURL == "" || token == "" {
		localURL, localToken, err := localServer()
		if serverURL == "" {
			if err != nil {
				return "", "", "", fmt.Errorf("no server to talk to: sign in with moor login, pass --server and --token, or set MOOR_SERVER and MOOR_TOKEN (%v)", err)
			}
			serverURL = localURL
		}
		if token == "" {
			token = localToken
		}
	}
	return serverURL, token, caCert, nil
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

// resolveApp picks the app from --app, $MOOR_APP, [app].name in the
// current directory's moorage.toml (which it also returns), or the app of
// the repository's moorage git remote.
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
		app = remoteApp(defaultRemote)
	}
	if app == "" {
		return "", m, errors.New("which app? pass --app NAME, set MOOR_APP, set name under [app] in moorage.toml, or add a git remote with moor git remote")
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
		conn                     connection
		app, env, image          string
		static, artifact, detach bool
	)
	cmd := &cobra.Command{
		Use:   "deploy [DIR]",
		Short: "Deploy to an app",
		Long: `Deploy to an app's production environment. The app is created if it does
not exist yet.

Without a flag, DIR (default: the current directory) is source: it is
uploaded, leaving out what .gitignore and .moorignore exclude, and the server
builds it as moorage.toml says (or as it detects: Go, Python, Node.js, a
static site), runs the release command, and rolls the new release out. The
build's output is shown as it happens; Ctrl-C stops following, not the
deployment.

With --static, the files under DIR are uploaded and served as they are,
following the [static] rules of moorage.toml in the current directory.
DIR defaults to [static].dir in moorage.toml, or the current directory.

With --artifact, DIR (default: the current directory) holds an app that is
ready to run, such as a compiled binary; the [processes] of moorage.toml in
the current directory say how to run it.

With --image, the server fetches an image and runs it in a container, pinned to
the digest it has now. The moorage.toml in DIR (default: the current directory)
is sent with it, if there is one, and no other file. A private registry needs its
credentials on the server: see moor registry login.

A new release takes over once its instances are healthy; until then the
previous release keeps serving.`,
		Example: `  moor deploy                       # build and deploy the current directory
  moor deploy ./api --app api
  moor deploy --static ./public --app docs
  moor deploy --artifact ./dist --app api
  moor deploy --image ghcr.io/acme/api:1.4 --app api`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if static && artifact {
				return usageError{errors.New("pass --static or --artifact, not both")}
			}
			if image != "" && (static || artifact) {
				return usageError{errors.New("pass --image alone, without --static or --artifact")}
			}
			if image != "" {
				if err := manifest.ValidateImage(image); err != nil {
					return usageError{err}
				}
			}
			if !static && !artifact {
				return deploySource(cmd, &conn, app, env, firstOf(firstArg(args), "."), image, detach)
			}
			appName, m, err := resolveApp(app)
			if err != nil {
				return err
			}
			var plan manifest.Plan
			var dir string
			if static {
				configured := ""
				if m != nil && m.Static != nil {
					configured = m.Static.Dir
				}
				dir = firstOf(firstArg(args), configured, ".")
				plan = m.StaticPlan()
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
	f.StringVar(&image, "image", "", "run this image in a container, pinned to its digest, instead of building DIR")
	f.BoolVar(&static, "static", false, "upload a directory of files and serve them as they are")
	f.BoolVar(&artifact, "artifact", false, "upload an app that is ready to run and run its [processes]")
	f.BoolVar(&detach, "detach", false, "return once the deployment is queued, without following it")
	f.StringVar(&app, "app", "", "app to deploy to (default: $MOOR_APP or [app].name in moorage.toml)")
	f.StringVar(&env, "env", names.Production, "environment to deploy to")
	return cmd
}

// alwaysLeftOut are never uploaded as source: the server makes its own.
var alwaysLeftOut = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true, ".DS_Store": true}

// deploySource uploads dir as source and follows its deployment.
func deploySource(cmd *cobra.Command, conn *connection, appFlag, env, dir, image string, detach bool) error {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		return err
	}
	appName := firstOf(appFlag, os.Getenv("MOOR_APP"))
	if appName == "" && m != nil {
		appName = m.App.Name
	}
	if appName == "" {
		return errors.New("which app? pass --app NAME, set MOOR_APP, or set name under [app] in moorage.toml")
	}
	if err := names.ValidateApp(appName); err != nil {
		return err
	}
	ignored, err := ignore.ForTree(dir, ".gitignore", ".moorignore")
	if err != nil {
		return err
	}
	skip := func(rel string, d fs.DirEntry) bool {
		return alwaysLeftOut[d.Name()] || ignored.Match(rel, d.IsDir())
	}
	c, err := conn.client()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if image != "" {
		d, _, err := c.DeployImage(cmd.Context(), appName, env, image, dir, func(rel string, d fs.DirEntry) bool {
			return d.IsDir() || rel != manifest.FileName
		})
		if err != nil {
			return err
		}
		if d.AppCreated {
			fmt.Fprintf(out, "Created app %s.\n", d.App)
		}
		fmt.Fprintf(out, "Deployment %s of %s to %s on %s is %s.\n", d.ID, image, appName, c.Server(), d.Status)
		if detach {
			fmt.Fprintf(out, "\nFollow it with: moor deployments follow %s\n", d.ID)
			return nil
		}
		return followDeployment(cmd, c, d.ID)
	}
	fmt.Fprintf(out, "Uploading %s to %s on %s\n", dir, appName, c.Server())
	d, stats, err := c.DeploySource(cmd.Context(), appName, env, dir, skip)
	if err != nil {
		return err
	}
	if len(stats.Skipped) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "left out %d symbolic links (only regular files are uploaded): %s\n", len(stats.Skipped), strings.Join(firstN(stats.Skipped, 3), ", "))
	}
	if d.AppCreated {
		fmt.Fprintf(out, "Created app %s.\n", d.App)
	}
	fmt.Fprintf(out, "Uploaded %d files (%s); deployment %s is %s.\n", stats.Files, humanBytes(stats.Bytes), d.ID, d.Status)
	if detach {
		fmt.Fprintf(out, "\nFollow it with: moor deployments follow %s\n", d.ID)
		return nil
	}
	return followDeployment(cmd, c, d.ID)
}

// followDeployment prints a deployment's log until it ends, and fails if
// the deployment did. Ctrl-C stops following; the deployment continues.
func followDeployment(cmd *cobra.Command, c *client.Client, id string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	out := cmd.OutOrStdout()
	d, err := c.FollowDeployment(ctx, id, func(l apitypes.LogLine) error {
		if l.Stream == "M" {
			fmt.Fprintln(out, l.Text)
		} else {
			fmt.Fprintln(out, "             "+l.Text)
		}
		return nil
	})
	if ctx.Err() != nil {
		fmt.Fprintf(out, "\nStopped following; the deployment continues. Follow it again with: moor deployments follow %s\n", id)
		return nil
	}
	if err != nil {
		return err
	}
	switch d.Status {
	case apitypes.StatusLive:
		return nil
	case apitypes.StatusFailed:
		return fmt.Errorf("deployment %s failed: %s", d.ID, d.Error)
	}
	return fmt.Errorf("deployment %s ended %s: %s", d.ID, d.Status, d.Error)
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], "…")
	}
	return s
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
			fmt.Fprintln(w, "NAME\tPROJECT\tURL\tRELEASE\tDEPLOYED")
			for _, a := range apps {
				release, deployed := "-", "-"
				if a.Production != nil {
					release = "v" + strconv.Itoa(a.Production.Version)
					deployed = ago(a.Production.CreatedAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Name, orDash(a.Project), a.URL, release, deployed)
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	var createConn, moveConn connection
	var project, moveTo string
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an app in a project",
		Long: `Create an app, before its first deployment. Deploying also creates one, in the
default project: this is how an app starts in another.`,
		Example: "  moor apps create shop --project payments",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := createConn.client()
			if err != nil {
				return err
			}
			a, err := c.CreateApp(cmd.Context(), args[0], project)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created %s in the project %s. It will be at %s.\n", a.Name, a.Project, a.URL)
			return nil
		},
	}
	createConn.addFlags(create)
	create.Flags().StringVar(&project, "project", "", "the project it goes in (default: the default project)")
	move := &cobra.Command{
		Use:     "move NAME --project PROJECT",
		Short:   "Put an app in another project",
		Long:    "Put an app in another project. It takes being a maintainer of both.",
		Example: "  moor apps move shop --project payments",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if moveTo == "" {
				return usageError{errors.New("say which project: --project NAME")}
			}
			c, err := moveConn.client()
			if err != nil {
				return err
			}
			a, err := c.MoveApp(cmd.Context(), args[0], moveTo)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now in the project %s.\n", a.Name, a.Project)
			return nil
		},
	}
	moveConn.addFlags(move)
	move.Flags().StringVar(&moveTo, "project", "", "the project it goes in")
	cmd.AddCommand(create, move, newAppInfoCmd(), newAppRenameCmd(), newAppDeleteCmd())
	return cmd
}

func newAppInfoCmd() *cobra.Command {
	var conn connection
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "info NAME",
		Short: "Show an app's address and project, and how much room it takes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			info, err := c.AppInfo(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), info)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "App         %s\nAddress     %s\nProject     %s\nBuilds      %s\nLogs        %s\nRepository  %s\nData        %s\n",
				info.Name, info.URL, info.Project, humanBytes(info.Builds), humanBytes(info.Logs), humanBytes(info.Repository), humanBytes(info.Data))
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newAppRenameCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "rename NAME NEW",
		Short: "Give an app another name",
		Long: `Give an app another name, which is another address and another git remote. Its
instances stop and start again under the new name, so there is a moment of downtime.

Change [app].name in its moorage.toml, and its git remote, to match: a deploy by the
old name makes a new app.`,
		Example: "  moor apps rename shop store",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			a, err := c.RenameApp(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s, at %s.\nChange [app].name in its moorage.toml and its git remote to match.\n", args[0], a.Name, a.URL)
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}

func newAppDeleteCmd() *cobra.Command {
	var conn connection
	var keepData, yes bool
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete an app, and everything of it",
		Long: `Delete an app: its instances, variables, releases, builds, logs and repository, and
its data directory unless --keep-data keeps it. This cannot be undone. It asks you to type
the app's name first, unless --yes.`,
		Example: `  moor apps delete shop
  moor apps delete shop --keep-data --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c, err := conn.client()
			if err != nil {
				return err
			}
			if !yes {
				info, err := c.AppInfo(cmd.Context(), name)
				if err != nil {
					return err
				}
				out := cmd.ErrOrStderr()
				fmt.Fprintf(out, "This deletes %s, its builds (%s), logs (%s) and repository (%s)", name, humanBytes(info.Builds), humanBytes(info.Logs), humanBytes(info.Repository))
				if keepData {
					fmt.Fprintf(out, ", and keeps its data directory (%s).\n", humanBytes(info.Data))
				} else {
					fmt.Fprintf(out, ", and its data directory (%s).\n", humanBytes(info.Data))
				}
				fmt.Fprintf(out, "Type %s to go on: ", name)
				answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if strings.TrimSpace(answer) != name {
					return errors.New("that is not its name: nothing was deleted")
				}
			}
			if err := c.DeleteApp(cmd.Context(), name, keepData); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s.\n", name)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&keepData, "keep-data", false, "keep its data directory")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask")
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
			addresses := slices.ContainsFunc(rels, func(r apitypes.Release) bool { return r.URL != "" })
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			if addresses {
				fmt.Fprintln(w, "\tRELEASE\tREASON\tFILES\tSIZE\tCREATED\tADDRESS")
			} else {
				fmt.Fprintln(w, "\tRELEASE\tREASON\tFILES\tSIZE\tCREATED")
			}
			for _, r := range rels {
				mark := ""
				if r.Current {
					mark = "*"
				}
				files := "-"
				if r.Files > 0 {
					files = strconv.Itoa(r.Files)
				}
				fmt.Fprintf(w, "%s\tv%d\t%s\t%s\t%s\t%s", mark, r.Version, r.Reason, files, humanBytes(r.Size), ago(r.CreatedAt))
				if addresses {
					fmt.Fprintf(w, "\t%s", orDash(r.URL))
				}
				fmt.Fprintln(w)
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.PersistentFlags().StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	cmd.PersistentFlags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newReleaseShowCmd(&conn, &app, &env), newReleaseDiffCmd(&conn, &app, &env))
	return cmd
}

// parseVersion reads a release version, such as v3 or 3.
func parseVersion(s string) (int, error) {
	v, err := strconv.Atoi(strings.TrimPrefix(s, "v"))
	if err != nil || v <= 0 {
		return 0, usageError{fmt.Errorf("%q is not a release version such as v3", s)}
	}
	return v, nil
}

func newReleaseShowCmd(conn *connection, app, env *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "show vN",
		Short:   "Show a release and its resolved manifest",
		Example: "  moor releases show v3 --app docs",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, err := parseVersion(args[0])
			if err != nil {
				return err
			}
			appName, _, err := resolveApp(*app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			rel, err := c.Release(cmd.Context(), appName, *env, version)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), rel)
			}
			what := rel.Manifest
			if what == "" {
				what = "What it runs is for those who can deploy it.\n"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "v%d of %s: %s, %s (the build %s)\n\n%s", rel.Version, appName, rel.Reason, ago(rel.CreatedAt), rel.Build, what)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newReleaseDiffCmd(conn *connection, app, env *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "diff vN vM",
		Short: "Show what changed from one release to another",
		Long: `Show what changed from one release to another: whether the build did, the
lines of the resolved manifest, and which variables. Of variables it shows
the names only.`,
		Example: "  moor releases diff v3 v4 --app docs",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, err := parseVersion(args[0])
			if err != nil {
				return err
			}
			to, err := parseVersion(args[1])
			if err != nil {
				return err
			}
			appName, _, err := resolveApp(*app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			diff, err := c.ReleaseDiff(cmd.Context(), appName, *env, from, to)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), diff)
			}
			printDiff(cmd.OutOrStdout(), diff)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func printDiff(w io.Writer, diff apitypes.ReleaseDiff) {
	if diff.BuildChanged {
		fmt.Fprintf(w, "v%d and v%d are different builds.\n", diff.From, diff.To)
	} else {
		fmt.Fprintf(w, "v%d and v%d are the same build.\n", diff.From, diff.To)
	}
	if len(diff.Manifest) == 0 {
		fmt.Fprintln(w, "What else changed is for those who can deploy it.")
		return
	}
	changed := false
	for _, l := range diff.Manifest {
		changed = changed || l.Op != apitypes.DiffSame
	}
	if changed {
		fmt.Fprintln(w, "\nThe resolved manifest:")
		for _, l := range diff.Manifest {
			if l.Op != apitypes.DiffSame {
				fmt.Fprintf(w, "%s %s\n", l.Op, l.Text)
			}
		}
	}
	if len(diff.Vars) > 0 {
		fmt.Fprintln(w, "\nVariables:")
		for _, v := range diff.Vars {
			secret := ""
			if v.Secret {
				secret = " (secret)"
			}
			fmt.Fprintf(w, "  %s %s%s\n", v.Change, v.Name, secret)
		}
	}
	if !changed && len(diff.Vars) == 0 {
		fmt.Fprintln(w, "Nothing else differs.")
	}
}

// newRedeployCmd builds the current release's source again.
func newRedeployCmd() *cobra.Command {
	var conn connection
	var app, env string
	var detach bool
	cmd := &cobra.Command{
		Use:   "redeploy",
		Short: "Build the current release's source again, and roll it out",
		Long: `Build what the current release was built from again, and roll it out: the
same commit, the same upload or the same image name. Use it when a toolchain
has moved, an image's tag points elsewhere, or a build variable has changed.

A release that was uploaded ready to run has nothing to build.`,
		Example: "  moor redeploy --app docs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			appName, _, err := resolveApp(app)
			if err != nil {
				return err
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			d, err := c.Redeploy(cmd.Context(), appName, env)
			if err != nil {
				return err
			}
			if detach {
				fmt.Fprintf(cmd.OutOrStdout(), "Deployment %s is queued; follow it with: moor deployments follow %s\n", d.ID, d.ID)
				return nil
			}
			return followDeployment(cmd, c, d.ID)
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&app, "app", "", "app (default: $MOOR_APP or [app].name in moorage.toml)")
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&detach, "detach", false, "queue the deployment and return")
	return cmd
}

func newRollbackCmd() *cobra.Command {
	var conn connection
	var app, env string
	var withConfig bool
	cmd := &cobra.Command{
		Use:   "rollback [vN]",
		Short: "Serve an earlier release again",
		Long: `Publish an earlier release again, as a new release. Without a version, the
release before the current one. No rebuild is needed: it takes seconds.

The new release runs with the current variables; --with-config restores
the variables the earlier release ran with.`,
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
			d, err := c.RollbackWith(cmd.Context(), appName, env, apitypes.RollbackRequest{Version: version, WithConfig: withConfig})
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
	cmd.Flags().BoolVar(&withConfig, "with-config", false, "restore the variables the earlier release ran with")
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
