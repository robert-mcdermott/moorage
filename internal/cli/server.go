package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/server"
)

func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Set up and run a Moorage server",
	}
	cmd.AddCommand(newServerUsersCmd(), newServerInitCmd(), newServerRunCmd(), newServerInstallCmd(), newServerUninstallCmd(),
		newServerStartCmd(), newServerStopCmd(), newServerRestartCmd(), newServerStatusCmd(), newServerDoctorCmd(),
		newServerProxyConfigCmd(), newServerResolverCmd())
	return cmd
}

// defaults returns where server.toml and the data directory go when neither
// a flag nor MOORAGE_CONFIG / MOORAGE_DATA_DIR says otherwise: system-wide
// locations when running as root (or an elevated Administrator), per-user
// locations otherwise.
func defaults() (platform.Dirs, error) {
	return platform.DefaultDirs(platform.IsPrivileged())
}

func configPath(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	if env := os.Getenv("MOORAGE_CONFIG"); env != "" {
		return filepath.Abs(env)
	}
	d, err := defaults()
	return d.ConfigFile, err
}

func newServerInitCmd() *cobra.Command {
	var (
		preset, domain, dataDir, cfgFlag string
		behindProxy, force               bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write server.toml for a new server",
		Long: `Write server.toml, the bootstrap configuration of a Moorage server.

Presets:
  lan     a server on your network: apps at https://<app>.<domain> with
          certificates from Moorage's own CA (ports 80, 443 and 2222), or plain
          HTTP on 127.0.0.1:8080 behind your TLS proxy with --behind-proxy
  local   this computer only: apps at http://<app>.localhost:8080

The file is written to --config, $MOORAGE_CONFIG, or a default location:
system-wide when run as root (Administrator on Windows), per-user otherwise.`,
		Example: `  moor server init --preset local
  sudo moor server init --domain apps.example.com
  sudo moor server init --domain apps.example.com --behind-proxy`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := configPath(cfgFlag)
			if err != nil {
				return err
			}
			if dataDir == "" {
				dataDir = os.Getenv("MOORAGE_DATA_DIR")
			}
			if dataDir == "" {
				d, err := defaults()
				if err != nil {
					return err
				}
				dataDir = d.DataDir
			}
			if dataDir, err = filepath.Abs(dataDir); err != nil {
				return err
			}
			cfg, err := config.Preset(preset, config.PresetOptions{Domain: domain, DataDir: dataDir, BehindProxy: behindProxy})
			if err != nil {
				return err
			}
			if err := config.Write(path, cfg, preset, force); err != nil {
				return err
			}
			printInitSummary(cmd, path, cfgFlag != "" || os.Getenv("MOORAGE_CONFIG") != "", preset, cfg)
			// A data directory with a database has a server already: it may be
			// claimed, and it makes or removes its own code when it starts.
			layout := server.NewLayout(cfg.DataDir)
			if _, err := os.Stat(layout.Database); os.IsNotExist(err) {
				code, err := auth.EnsureClaimCode(layout.ClaimCode)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "\nOnce it runs, claim it by creating the first account at:\n\n  %s\n", claimURL(cfg, code))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&preset, "preset", "lan", "starting point: "+strings.Join(config.Presets, " or "))
	f.StringVar(&domain, "domain", "", "base domain; apps are served at <app>.<domain> (required for lan)")
	f.BoolVar(&behindProxy, "behind-proxy", false, "serve plain HTTP on 127.0.0.1:8080 behind a TLS-terminating proxy such as nginx (lan)")
	f.StringVar(&dataDir, "data-dir", "", "data directory (default: $MOORAGE_DATA_DIR or a per-platform location)")
	f.StringVar(&cfgFlag, "config", "", "where to write server.toml (default: $MOORAGE_CONFIG or a per-platform location)")
	f.BoolVar(&force, "force", false, "replace an existing server.toml")
	return cmd
}

func printInitSummary(cmd *cobra.Command, path string, explicitPath bool, preset string, c config.Config) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Wrote %s (preset %s).\n\n", path, preset)
	fmt.Fprintf(out, "  Console:  %s\n", c.PublicURL(c.ConsoleHost))
	fmt.Fprintf(out, "  Apps:     %s\n", c.PublicURL("<app>."+c.Domain))
	fmt.Fprintf(out, "  Data:     %s\n\n", c.DataDir)
	switch {
	case len(c.Proxy.Trusted) > 0:
		fmt.Fprintf(out, "Point your TLS proxy at http://%s for %s and *.%s. It must keep the\n", c.Listen.HTTP, c.ConsoleHost, c.Domain)
		fmt.Fprintf(out, "Host header, pass WebSocket upgrades, not buffer responses, and allow long\n")
		fmt.Fprintf(out, "requests and large uploads. moor server proxy-config nginx prints a\n")
		fmt.Fprintf(out, "configuration for nginx.\n\n")
	case c.Domain != "localhost":
		fmt.Fprintf(out, "Point a wildcard DNS record, *.%s, at this server.\n\n", c.Domain)
	}
	run, install := "moor server run", "moor server install"
	if explicitPath {
		run += " --config " + quoteArg(path)
		install += " --config " + quoteArg(path)
	}
	if platform.IsPrivileged() {
		install = "sudo " + install
	}
	fmt.Fprintf(out, "Start the server with:\n\n  %s\n\nor install it as a service that starts with the computer:\n\n  %s\n\nmoor server doctor checks the setup at any time.\n", run, install)
}

// claimURL is the first-run setup link.
func claimURL(c config.Config, code string) string {
	return c.PublicURL(c.ConsoleHost) + "/setup?code=" + code
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"'") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func newServerRunCmd() *cobra.Command {
	var cfgFlag, logLevel, logFormat string
	var dev bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the server in the foreground",
		Long: `Run the Moorage server in the foreground until interrupted.

The configuration is read from --config, $MOORAGE_CONFIG, or the default
location used by 'moor server init'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := configPath(cfgFlag)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("no server configuration at %s: create one with 'moor server init' (or pass --config)", path)
			}
			cfg, err := config.Load(path, os.Getenv)
			if err != nil {
				return err
			}
			if logLevel != "" {
				cfg.Log.Level = logLevel
			}
			if logFormat != "" {
				cfg.Log.Format = logFormat
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			logFile, err := logs.Open(filepath.Join(cfg.DataDir, "logs", "server", "moorage.log"), logs.DefaultMaxSize, logs.DefaultKeep)
			if err != nil {
				return fmt.Errorf("open the server log: %w", err)
			}
			defer logFile.Close()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			opts := server.Options{}
			if dev {
				opts.ConsoleDir = filepath.Join("internal", "console")
				if _, err := os.Stat(filepath.Join(opts.ConsoleDir, "templates")); err != nil {
					return fmt.Errorf("--dev reads the console from the source tree: run it at the top of a checkout (%w)", err)
				}
			}
			return server.Run(ctx, cfg, newLogger(cmd.ErrOrStderr(), cfg.Log, logFile), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfgFlag, "config", "", "server.toml to read (default: $MOORAGE_CONFIG or a per-platform location)")
	f.StringVar(&logLevel, "log-level", "", "debug, info, warn or error (overrides server.toml)")
	f.StringVar(&logFormat, "log-format", "", "text or json (overrides server.toml)")
	f.BoolVar(&dev, "dev", false, "read the console's templates and assets from the source tree, for working on them")
	return cmd
}
