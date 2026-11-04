package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/doctor"
	"github.com/robert-mcdermott/moorage/internal/names"
)

func loadServerConfig(flag string) (string, config.Config, error) {
	path, err := configPath(flag)
	if err != nil {
		return "", config.Config{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return path, config.Config{}, fmt.Errorf("no server is configured at %s: write one with moor server init", path)
	}
	cfg, err := config.Load(path, os.Getenv)
	return path, cfg, err
}

func newServerDoctorCmd() *cobra.Command {
	var (
		cfgFlag, publicURL, caCert string
		timeouts, asJSON           bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the server and what it depends on",
		Long: `Check a server on this machine, whether it is running or not, and say how to fix
what is wrong: its listeners, DNS, certificates, the container engine, disk space,
the data directory, the clock, kernel features, and apps that can be reached
without the router.

--public-url also checks the whole path from outside to the server, through any
TLS proxy in front of it: the headers that arrive, an app's hostname, WebSockets,
streaming and large uploads. --timeouts adds a request that stays silent for over
a minute. The exit status is 1 if anything failed.`,
		Example: `  moor server doctor
  moor server doctor --public-url https://moorage.apps.example.com --timeouts`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, cfg, err := loadServerConfig(cfgFlag)
			if err != nil {
				return err
			}
			checks := []doctor.Check{{Name: "config", Status: doctor.OK, Detail: path}}
			checks = append(checks, doctor.Run(cmd.Context(), doctor.Options{
				Config: cfg, PublicURL: publicURL, CACert: caCert, Timeouts: timeouts,
				InstancePorts: instancePorts,
			})...)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(checks); err != nil {
					return err
				}
			} else {
				printChecks(cmd.OutOrStdout(), checks)
			}
			if doctor.Worst(checks) == doctor.Fail {
				return errors.New("the server has problems: see above")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfgFlag, "config", "", "server.toml (default: $MOORAGE_CONFIG or the per-platform location)")
	f.StringVar(&publicURL, "public-url", "", "the server's address from outside, such as https://moorage.apps.example.com")
	f.StringVar(&caCert, "ca-cert", "", "PEM file of a CA to trust for --public-url, besides the system's")
	f.BoolVar(&timeouts, "timeouts", false, "with --public-url, also check that a request may stay silent for over a minute")
	f.BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func printChecks(w io.Writer, checks []doctor.Check) {
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	marks := map[string]string{doctor.OK: "✔", doctor.Info: "·", doctor.Warn: "!", doctor.Fail: "✘"}
	counts := map[string]int{}
	for _, c := range checks {
		counts[c.Status]++
		fmt.Fprintf(w, "%s %-*s  %s\n", marks[c.Status], width, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "  %-*s  → %s\n", width, "", c.Fix)
		}
	}
	switch {
	case counts[doctor.Fail] > 0 || counts[doctor.Warn] > 0:
		var parts []string
		if n := counts[doctor.Fail]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d problem%s", n, plural(n)))
		}
		if n := counts[doctor.Warn]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d warning%s", n, plural(n)))
		}
		fmt.Fprintf(w, "\n%s.\n", strings.Join(parts, " and "))
	default:
		fmt.Fprintln(w, "\nNo problems.")
	}
}

// instancePorts lists the ports of the instances of the server on this machine.
func instancePorts(ctx context.Context) ([]int, error) {
	url, token, err := localServer()
	if err != nil {
		return nil, err
	}
	c, err := client.New(url, token, "")
	if err != nil {
		return nil, err
	}
	apps, err := c.Apps(ctx)
	if err != nil {
		return nil, err
	}
	var ports []int
	for _, app := range apps {
		instances, err := c.Instances(ctx, app.Name, names.Production)
		if err != nil {
			continue
		}
		for _, in := range instances {
			if in.Port != 0 {
				ports = append(ports, in.Port)
			}
		}
	}
	return ports, nil
}

func newServerProxyConfigCmd() *cobra.Command {
	var (
		cfgFlag string
		opts    config.NginxOptions
	)
	cmd := &cobra.Command{
		Use:   "proxy-config nginx",
		Short: "Print a configuration for running behind a TLS-terminating nginx",
		Long: `Print an nginx configuration for this server: its hostnames, the certificate,
WebSockets, streaming, long requests and large uploads. Any other proxy must do
the same; see the documentation. Check the result with moor server doctor
--public-url.`,
		Example: `  moor server proxy-config nginx --cert /etc/nginx/tls/wildcard.crt --key /etc/nginx/tls/wildcard.key
  moor server proxy-config nginx > /etc/nginx/conf.d/moorage.conf`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "nginx" {
				return usageError{fmt.Errorf("moor knows how to configure nginx, not %q; the documentation says what any proxy must do", args[0])}
			}
			_, cfg, err := loadServerConfig(cfgFlag)
			if err != nil {
				return err
			}
			for _, problem := range cfg.ProxyProblems() {
				fmt.Fprintf(cmd.ErrOrStderr(), "This server is not set up to run behind a proxy: %s (or start from moor server init --behind-proxy).\n", problem)
			}
			_, err = io.WriteString(cmd.OutOrStdout(), config.Nginx(cfg, opts))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfgFlag, "config", "", "server.toml (default: $MOORAGE_CONFIG or the per-platform location)")
	f.StringVar(&opts.Cert, "cert", "", "the proxy's certificate, for the console's and the apps' hostnames")
	f.StringVar(&opts.Key, "key", "", "its private key")
	f.IntVar(&opts.HTTPPort, "http-port", 80, "the port nginx redirects to HTTPS from")
	f.IntVar(&opts.HTTPSPort, "https-port", 443, "the port nginx serves HTTPS on")
	return cmd
}
