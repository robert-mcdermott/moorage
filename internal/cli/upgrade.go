package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/server"
	"github.com/robert-mcdermott/moorage/internal/service"
	"github.com/robert-mcdermott/moorage/internal/upgrade"
)

// runningService is the service manager's view of the server, in the terms an upgrade needs.
type runningService struct{ service.Manager }

func (s runningService) Running() bool {
	status, err := s.Status()
	return err == nil && status.Running
}

func newServerUpgradeCmd() *cobra.Command {
	var (
		cfgFlag, version, from string
		check, rollback, force bool
		wait                   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the server to a newer release, or go back from one",
		Long: `Upgrade downloads a release, checks its signature and checksum, keeps a copy of the
database and the binary, puts the new binary in place and restarts the server. Apps keep running. If
the server does not come up on the new version, everything goes back by itself.

With --rollback it goes back later on. The database only goes back if the new version changed it,
since what has happened since is then lost from it.`,
		Example: `  moor server upgrade --check
  sudo moor server upgrade
  sudo moor server upgrade --version 0.6.1
  sudo moor server upgrade --from /mnt/releases/0.6.1
  sudo moor server upgrade --rollback`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if rollback && (check || version != "" || from != "") {
				return errors.New("--rollback goes back to what was there, and takes neither --check, --version nor --from")
			}
			if force && !rollback {
				return errors.New("--force only goes with --rollback")
			}
			path, err := configPath(cfgFlag)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("no server is configured at %s: on a machine that only uses a server, run the installer again to upgrade moor", path)
			}
			cfg, err := config.Load(path, os.Getenv)
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(exe)
			if err != nil {
				return err
			}
			keys, err := upgrade.ReleaseKeys()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			layout := server.NewLayout(cfg.DataDir)
			o := upgrade.Options{
				Executable: resolved, Current: buildinfo.Version, DataDir: cfg.DataDir, Database: layout.Database,
				Source: upgrade.Source{Base: from, Flat: from != ""}, Version: strings.TrimPrefix(version, "v"), Keys: keys,
				Check: check, Force: force, Wait: wait,
				Say:      func(format string, args ...any) { fmt.Fprintf(out, "  "+format+"\n", args...) },
				ServerUp: func() bool { return serverRunning(layout.LockFile) },
			}
			target := serviceTarget{cfg: cfg}
			o.Healthy = target.healthy
			manager, err := service.For(platform.IsPrivileged())
			if err != nil && !errors.Is(err, service.ErrUnsupported) {
				return err
			}
			if err == nil {
				if status, err := manager.Status(); err == nil && status.Installed {
					// The binary that matters is the one the service runs.
					if err := runsThis(manager, exe, resolved); err != nil {
						return err
					}
					o.Service = runningService{manager}
				}
			}
			if rollback {
				res, err := upgrade.Rollback(cmd.Context(), o)
				if err != nil {
					return err
				}
				if res.Restored {
					fmt.Fprintf(out, "The database went back to how it was before the upgrade. As it was is in %s.\n", res.Aside)
				}
				fmt.Fprintf(out, "Back on %s.\n", res.To)
				if res.Stopped {
					fmt.Fprintln(out, "The server is not running: start it, and it uses this version.")
				}
				return nil
			}
			res, err := upgrade.Upgrade(cmd.Context(), o)
			if err != nil {
				return err
			}
			switch {
			case res.Current:
				fmt.Fprintf(out, "This is %s already.\n", res.From)
			case res.Available:
				fmt.Fprintf(out, "This is %s, and %s is out. To upgrade: moor server upgrade\n", res.From, res.To)
			default:
				if !res.Signed {
					fmt.Fprintln(out, "This binary knows no signing key, so that the release was only checked against its checksum.")
				}
				if res.Restarted {
					fmt.Fprintf(out, "The server now runs %s, and the apps kept running.\n", res.To)
				} else if res.Stopped {
					fmt.Fprintf(out, "Upgraded to %s. The server is not running: start it, and it uses this version.\n", res.To)
				} else {
					fmt.Fprintf(out, "Upgraded to %s. The server goes on with %s until you restart it, where it runs.\n", res.To, res.From)
				}
				fmt.Fprintln(out, "To go back: moor server upgrade --rollback")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "server.toml (default: $MOORAGE_CONFIG or the per-platform location)")
	cmd.Flags().StringVar(&version, "version", "", "the version to upgrade to (default: the newest)")
	cmd.Flags().StringVar(&from, "from", "", "a directory or address holding the release's files, for a mirror or a machine with no way out")
	cmd.Flags().DurationVar(&wait, "wait", 2*time.Minute, "how long the new version has to come up before going back")
	cmd.Flags().BoolVar(&check, "check", false, "only say whether there is a newer version")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "go back to the version before the last upgrade")
	cmd.Flags().BoolVar(&force, "force", false, "with --rollback, go back even if that loses what has happened since")
	return cmd
}

// runsThis refuses to replace a binary that the server's service does not run, which would change nothing.
func runsThis(manager service.Manager, paths ...string) error {
	definition, err := os.ReadFile(manager.File())
	if err != nil {
		return err
	}
	for _, path := range paths {
		if strings.Contains(string(definition), path) {
			return nil
		}
	}
	return fmt.Errorf("the service in %s runs another moor than %s: run that one's upgrade, or install the service again from this one", manager.File(), paths[0])
}

// serverRunning says whether a server holds its lock.
func serverRunning(file string) bool {
	if _, err := os.Stat(file); err != nil {
		return false
	}
	lock, err := platform.LockFile(file)
	if err != nil {
		return errors.Is(err, platform.ErrLocked)
	}
	lock.Unlock()
	return false
}
