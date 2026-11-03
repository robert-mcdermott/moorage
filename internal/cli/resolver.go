package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/config"
)

// Where macOS looks for a resolver rule per domain, and which system this is;
// tests change them.
var (
	resolverDir  = "/etc/resolver"
	resolverGOOS = runtime.GOOS
)

// defaultDNSAddr needs no privileges to listen on.
const defaultDNSAddr = "127.0.0.1:15353"

func newServerResolverCmd() *cobra.Command {
	var cfgFlag string
	cmd := &cobra.Command{
		Use:   "resolver",
		Short: "Show whether this machine resolves the addresses of a local server's apps",
		Long: `Show whether this machine resolves <app>.localhost, the addresses of a server on
your own computer. Chromium- and Firefox-based browsers manage that on their own,
as does systemd-resolved on Linux. macOS does not, for Safari and command-line
tools: moor server resolver install adds a rule for it, pointing at a small DNS
responder of the server's, which answers only on loopback and only for that domain.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, cfg, err := loadServerConfig(cfgFlag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			probe := "moorage-resolver-check." + cfg.Domain
			if addrs, err := net.DefaultResolver.LookupHost(ctx, probe); err == nil && len(addrs) > 0 {
				fmt.Fprintf(out, "*.%s resolves on this machine.\n", cfg.Domain)
			} else {
				fmt.Fprintf(out, "*.%s does not resolve for every program on this machine.\n", cfg.Domain)
			}
			if resolverGOOS == "darwin" {
				rule := filepath.Join(resolverDir, cfg.Domain)
				if _, err := os.Stat(rule); err == nil {
					fmt.Fprintf(out, "The rule %s is in place; moor server resolver remove takes it out.\n", rule)
				} else {
					fmt.Fprintln(out, "No rule is in place; moor server resolver install adds one.")
				}
			}
			if cfg.Listen.DNS != "" {
				fmt.Fprintf(out, "The server answers DNS queries on %s.\n", cfg.Listen.DNS)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "server.toml (default: $MOORAGE_CONFIG or the per-platform location)")
	cmd.AddCommand(newResolverInstallCmd(), newResolverRemoveCmd())
	return cmd
}

func newResolverInstallCmd() *cobra.Command {
	var cfgFlag string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Make this machine resolve the addresses of a local server's apps (macOS)",
		Long: `Make this machine resolve <app>.localhost for every program, by adding a rule to
/etc/resolver, for which it uses sudo, and turning on the server's DNS responder
(listen.dns in server.toml). The server needs restarting to take that up.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, cfg, err := loadServerConfig(cfgFlag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch resolverGOOS {
			case "darwin":
			case "linux":
				fmt.Fprintln(out, "Nothing to install on Linux: systemd-resolved and nss-myhostname resolve *.localhost. On a system with neither, list your apps in /etc/hosts.")
				return nil
			default:
				return fmt.Errorf("not available on %s yet: browsers based on Chromium or Firefox resolve *.localhost on their own, and other programs can send the name as the Host header to 127.0.0.1", resolverGOOS)
			}
			if cfg.Domain != "localhost" {
				return fmt.Errorf("this server's domain is %s: that takes a real DNS record, *.%s, not a rule on one machine", cfg.Domain, cfg.Domain)
			}
			addr := cfg.Listen.DNS
			restart := false
			if addr == "" {
				addr = defaultDNSAddr
				if err := config.SetListenDNS(path, addr); err != nil {
					return err
				}
				if _, err := config.Load(path, os.Getenv); err != nil {
					return fmt.Errorf("after changing %s: %w", path, err)
				}
				restart = true
				fmt.Fprintf(out, "Set listen.dns = %q in %s.\n", addr, path)
			}
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return err
			}
			rule := filepath.Join(resolverDir, cfg.Domain)
			content := fmt.Sprintf("# Written by moor server resolver install; moor server resolver remove takes it out.\nnameserver %s\nport %s\n", host, port)
			if err := writeSystemFile(rule, []byte(content)); err != nil {
				return fmt.Errorf("add %s: %w", rule, err)
			}
			fmt.Fprintf(out, "Added %s.\n", rule)
			if restart {
				fmt.Fprintln(out, "\nRestart the server to turn on its DNS responder (moor server restart, or Ctrl-C and moor server run).")
			}
			fmt.Fprintln(out, "moor server doctor checks the result.")
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "server.toml (default: $MOORAGE_CONFIG or the per-platform location)")
	return cmd
}

func newResolverRemoveCmd() *cobra.Command {
	var cfgFlag string
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Undo moor server resolver install",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, cfg, err := loadServerConfig(cfgFlag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			rule := filepath.Join(resolverDir, cfg.Domain)
			if resolverGOOS == "darwin" {
				if err := removeSystemFile(rule); err != nil {
					return fmt.Errorf("remove %s: %w", rule, err)
				}
				fmt.Fprintf(out, "Removed %s.\n", rule)
			}
			if cfg.Listen.DNS != "" {
				if err := config.SetListenDNS(path, ""); err != nil {
					return err
				}
				fmt.Fprintf(out, "Removed listen.dns from %s; the server stops answering when it restarts.\n", path)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "server.toml (default: $MOORAGE_CONFIG or the per-platform location)")
	return cmd
}

// writeSystemFile writes a file only root may, through sudo if it must.
func writeSystemFile(path string, data []byte) error {
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = os.WriteFile(path, data, 0o644)
	}
	if !errors.Is(err, os.ErrPermission) {
		return err
	}
	tmp, err := os.CreateTemp("", "moorage-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	tmp.Close()
	if err != nil {
		return err
	}
	if err := sudo("mkdir", "-p", filepath.Dir(path)); err != nil {
		return err
	}
	return sudo("install", "-m", "0644", tmp.Name(), path)
}

func removeSystemFile(path string) error {
	err := os.Remove(path)
	switch {
	case err == nil, errors.Is(err, os.ErrNotExist):
		return nil
	case errors.Is(err, os.ErrPermission):
		return sudo("rm", "-f", path)
	}
	return err
}

func sudo(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
