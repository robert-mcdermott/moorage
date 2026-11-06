package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/driver/native"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/server"
	"github.com/robert-mcdermott/moorage/internal/service"
	"github.com/robert-mcdermott/moorage/internal/truststore"
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
	m, err := serviceFor(t.system)
	if errors.Is(err, service.ErrUnsupported) {
		return fmt.Errorf("%w; on %s, run moor server run under the service manager of your choice (the Windows service arrives in a later release)", err, runtime.GOOS)
	}
	if err != nil {
		return err
	}
	t.manager, t.path, t.cfg = m, path, cfg
	return nil
}

// isThisServers refuses a service that does not run this server. A service has one name for all of a user's servers, whatever the
// configuration or the home directory, so that going by the name would reach a real one from a server that someone is only trying out.
func (t *serviceTarget) isThisServers() error {
	if runsServer(t.manager, t.path) {
		return nil
	}
	if runsAnother(t.manager, t.path) {
		return fmt.Errorf("the service in %s runs another server than %s, and this leaves it alone: use the configuration that it runs, or moor server install to change it", t.manager.File(), t.path)
	}
	return fmt.Errorf("no service runs the server of %s: moor server install makes one, and moor server run runs it without", t.path)
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

// engineAnswerLimit is how long an engine has to answer, to stop what is in it. A rootless Podman that has just started can take several seconds.
const engineAnswerLimit = 15 * time.Second

// stopApps stops every app instance of the server, as a server shutting down for good would: those it runs natively, and those in containers. Then it stops
// its services, which keep what they hold in their volumes, after the apps that may be writing to them.
func stopApps(cmd *cobra.Command, cfg config.Config) {
	type stopper interface {
		List() []string
		Stop(id string, stopTimeout time.Duration) error
	}
	drivers := []stopper{native.New(filepath.Join(cfg.DataDir, "run", "shims"), nil, nil)}
	// Any of them may be what a server used, so that only those that do not answer are left out, and that is only worth saying if none did.
	var engines []*container.Driver
	var silent []string
	for _, engine := range container.FindAll(cfg.Containers.Engine) {
		asking, cancel := context.WithTimeout(cmd.Context(), engineAnswerLimit)
		_, err := engine.Check(asking)
		cancel()
		if err != nil {
			silent = append(silent, filepath.Base(engine.Bin))
			continue
		}
		d := container.New(engine, server.ContainerLabel(cfg), slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)))
		engines = append(engines, d)
		drivers = append(drivers, d)
	}
	if len(engines) == 0 && len(silent) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Not stopped: anything in containers, as %s did not answer.\n", strings.Join(silent, " and "))
	}
	var wg sync.WaitGroup
	var stopped atomic.Int32
	for _, d := range drivers {
		for _, id := range d.List() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := d.Stop(id, 30*time.Second); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "could not stop %s: %v\n", id, err)
					return
				}
				stopped.Add(1)
			}()
		}
	}
	wg.Wait()
	if n := int(stopped.Load()); n > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Stopped %d app instance%s; the server starts them again when it starts.\n", n, plural(n))
	}
	var services atomic.Int32
	for _, d := range engines {
		names, err := d.ServiceContainers(cmd.Context())
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "could not list what services there are: %v\n", err)
			continue
		}
		for _, name := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := d.StopService(name, container.ServiceStopLimit); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "could not stop %s: %v\n", name, err)
					return
				}
				services.Add(1)
			}()
		}
	}
	wg.Wait()
	if n := int(services.Load()); n > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Stopped %d service%s, which keep what they hold; the server starts them again when it starts.\n", n, plural(n))
	}
}

// serviceFor finds this machine's service; tests keep theirs out of it.
var serviceFor = service.For

// runsServer reports whether a service runs the server configured by this file:
// a server tried out with a configuration of its own must not stop the real one.
func runsServer(m service.Manager, configFile string) bool {
	unit, err := m.Definition()
	return err == nil && sameFile(service.ConfigIn(unit), configFile)
}

// runsAnother says that it certainly does not: one that cannot be read may be this one.
func runsAnother(m service.Manager, configFile string) bool {
	unit, err := m.Definition()
	return err == nil && !sameFile(service.ConfigIn(unit), configFile)
}

// sameFile says whether two paths are one file, however they are spelled: through a link, as /tmp is on macOS, or in another case where that makes no difference.
func sameFile(a, b string) bool {
	if a == b {
		return a != ""
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// stopForeground stops a server that is not running under the service manager,
// such as one started with moor server run. The data directory's lock says
// whether one runs, and holds its process ID. It reports whether it stopped one.
func stopForeground(cfg config.Config) (bool, error) {
	file := server.NewLayout(cfg.DataDir).LockFile
	if _, err := os.Stat(file); err != nil {
		return false, nil
	}
	held := func() bool {
		lock, err := platform.LockFile(file)
		if err != nil {
			return errors.Is(err, platform.ErrLocked)
		}
		lock.Unlock()
		return false
	}
	// Read before the check, which leaves its own ID in the file if the lock is free.
	data, readErr := os.ReadFile(file)
	if errors.Is(readErr, os.ErrPermission) {
		return false, fmt.Errorf("this user may not use the server's files in %s: use sudo", cfg.DataDir)
	}
	if !held() {
		return false, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if readErr != nil || err != nil || pid <= 0 {
		return false, errors.New("a server is running, and this system does not say which: stop it where it runs (Ctrl-C)")
	}
	p, err := os.FindProcess(pid)
	if err == nil {
		err = p.Signal(syscall.SIGTERM)
	}
	if err != nil {
		return false, fmt.Errorf("a server is running as process %d, and it cannot be stopped from here (%v): stop it where it runs (Ctrl-C), or use sudo", pid, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for held() {
		if time.Now().After(deadline) {
			return false, fmt.Errorf("the server, process %d, has not stopped", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true, nil
}

func newServerInstallCmd() *cobra.Command {
	var t serviceTarget
	var noStart bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the server as a service and start it",
		Long: `Install moor server run as a service — a systemd unit on Linux, a launchd
job on macOS, a Windows service — that starts at boot (as root or an
administrator) or at login (otherwise, on Linux and macOS), and start it.

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
		Long: `Stop the server, remove its service, and stop the apps and the services they
use (unless --keep-apps). The configuration, the data directory and the volumes of
services are left in place: installing the service again brings the server, and
its apps, back.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := t.load(); err != nil {
				return err
			}
			switch {
			case runsServer(t.manager, t.path):
				if err := t.manager.Uninstall(); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed %s.\n", t.manager.File())
			case runsAnother(t.manager, t.path):
				return t.isThisServers()
			default:
				// The server may be running without one, and this should not stop what it is looking after under it.
				fmt.Fprintln(cmd.OutOrStdout(), "No service runs this server, so that there is nothing to remove. moor server stop --all stops it, and its apps.")
				return nil
			}
			if !keepApps {
				stopApps(cmd, t.cfg)
			}
			return nil
		},
	}
	t.addFlag(cmd)
	cmd.Flags().BoolVar(&keepApps, "keep-apps", false, "leave the apps and services running")
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
			if err := t.isThisServers(); err != nil {
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
		Short: "Stop the server (the apps keep running)",
		Long: `Stop the server: its service, or one started with moor server run. The apps and
services keep running, and the server adopts them when it starts again; --all stops
them too, whether or not a server is running. Services keep what they hold.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, cfg, err := loadServerConfig(t.cfgFlag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			stopped := false
			if m, err := serviceFor(platform.IsPrivileged()); err == nil && runsServer(m, cfg.Path) {
				if st, err := m.Status(); err == nil && st.Installed {
					if err := m.Stop(); err != nil {
						return err
					}
					// So that starting it straight afterwards works: launchd is only told to stop it, and ignores a start until it has.
					for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
						if st, err := m.Status(); err != nil || !st.Running {
							break
						}
					}
					stopped = true
				}
			}
			foreground, err := stopForeground(cfg)
			if err != nil {
				return err
			}
			if stopped || foreground {
				fmt.Fprintln(out, "The server is stopped.")
			} else {
				fmt.Fprintln(out, "The server is not running.")
			}
			if all {
				stopApps(cmd, cfg)
			}
			return nil
		},
	}
	t.addFlag(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "stop the apps and services too")
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
			if err := t.isThisServers(); err != nil {
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
			switch {
			case !st.Installed:
				fmt.Fprintf(out, "Service:  not installed (moor server install)\n")
			case runsAnother(t.manager, t.path):
				// The service on this machine may be for another server: what it is doing says nothing about this one.
				fmt.Fprintf(out, "Service:  none for this configuration (the one here runs another: %s)\n", t.manager.File())
			default:
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
			if fp := caFingerprint(t.cfg); fp != "" {
				fmt.Fprintf(out, "CA:       SHA-256 %s (moor trust)\n", fp)
			}
			if data, err := os.ReadFile(server.NewLayout(t.cfg.DataDir).ClaimCode); err == nil {
				fmt.Fprintf(out, "Claim:    no accounts yet: create the first at %s\n", claimURL(t.cfg, strings.TrimSpace(string(data))))
			}
			return nil
		},
	}
	t.addFlag(cmd)
	return cmd
}

// caFingerprint is the fingerprint of the server's internal CA, if it has one
// and this user can read it.
func caFingerprint(cfg config.Config) string {
	if cfg.TLS.Mode != "internal" {
		return ""
	}
	file := cfg.TLS.CACert
	if file == "" {
		file = filepath.Join(server.NewLayout(cfg.DataDir).Certs, "ca.crt")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	cert, err := parseCA(data, file)
	if err != nil {
		return ""
	}
	return truststore.Fingerprint(cert)
}
