package cli

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/driver/native"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/service"
)

// serviceTarget is what the service commands work on: the service of this
// user (or of the system, as root) and the server's configuration.
type serviceTarget struct {
	cfgFlag string
	manager service.Manager
	path    string
	cfg     config.Config
	system  bool
}

func (t *serviceTarget) load() error {
	path, err := configPath(t.cfgFlag)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no server is configured at %s: write one with moor server init", path)
	}
	cfg, err := config.Load(path, os.Getenv)
	if err != nil {
		return err
	}
	t.system = platform.IsPrivileged()
	m, err := service.For(t.system)
	if errors.Is(err, service.ErrUnsupported) {
		return fmt.Errorf("%w; on %s, run moor server run under the service manager of your choice (the Windows service arrives in a later release)", err, runtime.GOOS)
	}
	if err != nil {
		return err
	}
	t.manager, t.path, t.cfg = m, path, cfg
	return nil
}

func (t *serviceTarget) addFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(&t.cfgFlag, "config", "", "server.toml (default: $MOORAGE_CONFIG or the per-platform location)")
}

// healthy waits for the server to answer on its HTTP listener.
func (t *serviceTarget) healthy(wait time.Duration) bool {
	host, port, err := net.SplitHostPort(t.cfg.Listen.HTTP)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(wait)
	for {
		res, err := client.Get("http://" + net.JoinHostPort(host, port) + "/.moorage/health")
		if err == nil {
			res.Body.Close()
			// A server that terminates TLS redirects plain HTTP; any answer
			// from it will do.
			if res.StatusCode < 500 {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stopApps stops every app instance of the server, as a server shutting
// down for good would.
func (t *serviceTarget) stopApps(cmd *cobra.Command) {
	d := native.New(filepath.Join(t.cfg.DataDir, "run", "shims"), nil, nil)
	ids := d.List()
	for _, id := range ids {
		if err := d.Stop(id, 30*time.Second); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "could not stop %s: %v\n", id, err)
		}
	}
	if len(ids) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Stopped %d app instance%s; the server starts them again when it starts.\n", len(ids), plural(len(ids)))
	}
}

func newServerInstallCmd() *cobra.Command {
	var t serviceTarget
	var noStart bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the server as a service and start it",
		Long: `Install moor server run as a service — a systemd unit on Linux, a launchd
job on macOS — that starts at boot (as root) or at login (otherwise), and
start it.

The service is set up so that stopping or restarting it leaves the apps
running: the server adopts them when it starts again. It runs this moor
binary from where it is now, with the current PATH.`,
		Example: `  sudo moor server install
  moor server install --config ~/moorage/server.toml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := t.load(); err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if tmp := os.TempDir(); strings.HasPrefix(exe, tmp) || strings.Contains(exe, "go-build") {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s looks temporary; install moor somewhere lasting (such as /usr/local/bin) and run install from there\n", exe)
			}
			spec := service.Spec{Exe: exe, Config: t.path, DataDir: t.cfg.DataDir, System: t.system, Path: os.Getenv("PATH")}
			if err := t.manager.Install(spec, !noStart); err != nil {
				return err
			}
			fmt.Fprintf(out, "Installed %s.\n", t.manager.File())
			if noStart {
				fmt.Fprintln(out, "Start it with: moor server start")
				return nil
			}
			if !t.healthy(20 * time.Second) {
				return fmt.Errorf("the service started but the server does not answer on %s; see %s", t.cfg.Listen.HTTP, t.manager.Logs())
			}
			fmt.Fprintf(out, "The server is running as a service.\n\n  Console:  %s\n  Logs:     %s\n", t.cfg.PublicURL(t.cfg.ConsoleHost), t.manager.Logs())
			if runtime.GOOS == "linux" && !t.system {
				fmt.Fprintln(out, "\nA user service stops when you log out, unless lingering is on: loginctl enable-linger")
			}
			return nil
		},
	}
	t.addFlag(cmd)
	cmd.Flags().BoolVar(&noStart, "no-start", false, "install the service without starting it")
	return cmd
}

func newServerUninstallCmd() *cobra.Command {
	var t serviceTarget
	var keepApps bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the server and remove its service",
		Long: `Stop the server, remove its service, and stop the apps (unless --keep-apps).
The configuration and the data directory are left in place: installing the
service again brings the server, and its apps, back.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := t.load(); err != nil {
				return err
			}
			if err := t.manager.Uninstall(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s.\n", t.manager.File())
			if !keepApps {
				t.stopApps(cmd)
			}
			return nil
		},
	}
	t.addFlag(cmd)
	cmd.Flags().BoolVar(&keepApps, "keep-apps", false, "leave the apps running")
	return cmd
}

func newServerStartCmd() *cobra.Command {
	var t serviceTarget
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the server's service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := t.load(); err != nil {
				return err
			}
			if err := t.manager.Start(); err != nil {
				return err
			}
			if !t.healthy(20 * time.Second) {
				return fmt.Errorf("the server does not answer on %s; see %s", t.cfg.Listen.HTTP, t.manager.Logs())
			}
			fmt.Fprintln(cmd.OutOrStdout(), "The server is running.")
			return nil
		},
	}
	t.addFlag(cmd)
	return cmd
}

func newServerStopCmd() *cobra.Command {
	var t serviceTarget
	var all bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the server's service (the apps keep running)",
		Long: `Stop the server's service. The apps keep running, and the server adopts them
when it starts again; --all stops them too.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := t.load(); err != nil {
				return err
			}
			if err := t.manager.Stop(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "The server is stopped.")
			if all {
				t.stopApps(cmd)
			}
			return nil
		},
	}
	t.addFlag(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "stop the apps too")
	return cmd
}

func newServerRestartCmd() *cobra.Command {
	var t serviceTarget
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the server's service (the apps keep running)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := t.load(); err != nil {
				return err
			}
			if err := t.manager.Restart(); err != nil {
				return err
			}
			if !t.healthy(20 * time.Second) {
				return fmt.Errorf("the server does not answer on %s; see %s", t.cfg.Listen.HTTP, t.manager.Logs())
			}
			fmt.Fprintln(cmd.OutOrStdout(), "The server restarted.")
			return nil
		},
	}
	t.addFlag(cmd)
	return cmd
}

func newServerStatusCmd() *cobra.Command {
	var t serviceTarget
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the server's service runs and answers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := t.load(); err != nil {
				return err
			}
			st, err := t.manager.Status()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !st.Installed {
				fmt.Fprintf(out, "Service:  not installed (moor server install)\n")
			} else {
				state := st.Detail
				if st.PID > 0 {
					state += fmt.Sprintf(", pid %d", st.PID)
				}
				fmt.Fprintf(out, "Service:  %s (%s)\n", state, t.manager.File())
			}
			answer := "not answering"
			if t.healthy(0) {
				answer = "answering"
			}
			fmt.Fprintf(out, "Server:   %s on %s\n", answer, t.cfg.Listen.HTTP)
			fmt.Fprintf(out, "Config:   %s\n", t.path)
			fmt.Fprintf(out, "Logs:     %s\n", t.manager.Logs())
			return nil
		},
	}
	t.addFlag(cmd)
	return cmd
}
