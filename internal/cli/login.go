package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/client"
)

func newLoginCmd() *cobra.Command {
	var (
		caCert, name         string
		noBrowser, withToken bool
	)
	cmd := &cobra.Command{
		Use:   "login [URL]",
		Short: "Sign in to a server",
		Long: `Sign in to a server, and save it as a context for later commands. moor login
opens the server's approval page in your browser, and receives a token when
you approve it; no password is typed into the terminal.

On a machine without a browser, open the address it prints on any other
machine, or pass an API token on standard input with --with-token.

URL is the server's console, such as https://moorage.apps.example.com. It
defaults to $MOOR_SERVER, the current context's server, or the server on this
machine.`,
		Example: `  moor login https://moorage.apps.example.com
  moor login https://moorage.apps.example.com --no-browser
  moor login https://moorage.apps.example.com --with-token < token.txt`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			conn := connection{caCert: caCert}
			serverURL := ""
			if len(args) == 1 {
				serverURL = args[0]
			} else {
				var err error
				if serverURL, _, _, err = conn.resolve(); err != nil {
					return err
				}
			}
			serverURL = strings.TrimSuffix(serverURL, "/")
			caCert = firstOf(caCert, os.Getenv("MOOR_CA_CERT"))
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			var token string
			var err error
			if withToken {
				token, err = readSecret(cmd, "the token")
				token = strings.TrimSpace(token)
			} else {
				token, err = deviceLogin(ctx, cmd, serverURL, caCert, noBrowser)
			}
			if err != nil {
				return err
			}
			c, err := client.New(serverURL, token, caCert)
			if err != nil {
				return err
			}
			me, err := c.Me(ctx)
			if err != nil {
				return fmt.Errorf("the server refused the token: %w", err)
			}
			user := firstOf(me.Email, me.Name)
			cfg, err := loadClientConfig()
			if err != nil {
				return err
			}
			if name == "" {
				name = contextName(serverURL)
			}
			if cfg.Contexts == nil {
				cfg.Contexts = map[string]clientContext{}
			}
			cfg.Contexts[name] = clientContext{Server: serverURL, Token: token, CACert: caCert, User: user}
			cfg.Current = name
			if err := cfg.save(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Signed in to %s as %s (context %s).\n", serverURL, user, name)
			if info, err := c.ServerInfo(ctx); err == nil && info.Version != buildinfo.Version {
				fmt.Fprintf(out, "The server runs Moorage %s, and this command line is %s: some commands may not match.\n", info.Version, buildinfo.Version)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&caCert, "ca-cert", "", "PEM file of an extra CA to trust, such as the server's internal CA (default: $MOOR_CA_CERT)")
	f.StringVar(&name, "name", "", "the context's name (default: the server's hostname)")
	f.BoolVar(&noBrowser, "no-browser", false, "print the approval page's address without opening a browser")
	f.BoolVar(&withToken, "with-token", false, "read an API token from standard input instead of opening a browser")
	return cmd
}

// deviceLogin asks the server for a sign-in, has the user approve it in a
// browser, and returns the token.
func deviceLogin(ctx context.Context, cmd *cobra.Command, serverURL, caCert string, noBrowser bool) (string, error) {
	c, err := client.New(serverURL, "", caCert)
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "this computer"
	}
	dev, err := c.StartDevice(ctx, "moor on "+host)
	if err != nil {
		return "", err
	}
	out := cmd.OutOrStdout()
	if noBrowser || openBrowser(dev.VerificationURLComplete) != nil {
		fmt.Fprintf(out, "To sign in, open this address in a browser:\n\n  %s\n\n", dev.VerificationURLComplete)
	} else {
		fmt.Fprintf(out, "Your browser is opening the server's approval page. If it does not, open:\n\n  %s\n\n", dev.VerificationURLComplete)
	}
	fmt.Fprintf(out, "Check that it shows this code, then approve it:\n\n  %s\n\n", dev.UserCode)
	if f, ok := out.(interface{ Sync() error }); ok {
		f.Sync()
	}
	interval := time.Duration(dev.Interval) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", errors.New("stopped waiting for approval")
		case <-time.After(interval):
		}
		got, err := c.DeviceToken(ctx, dev.DeviceCode)
		if err == nil {
			return got.Token, nil
		}
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) {
			return "", err
		}
		switch apiErr.Code {
		case "authorization_pending":
		case "slow_down":
			interval += time.Second
		case "access_denied":
			return "", errors.New("the sign-in was denied")
		case "expired_token":
			return "", errors.New("the sign-in expired: run moor login again")
		default:
			return "", err
		}
	}
	return "", errors.New("the sign-in expired: run moor login again")
}

// openBrowser asks the system to open an address.
func openBrowser(address string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", address).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", address).Start()
	}
	return exec.Command("xdg-open", address).Start()
}

func newLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Sign out of the current server, revoking the token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadClientConfig()
			if err != nil {
				return err
			}
			ctx := cfg.current()
			if ctx == nil {
				return errors.New("no context is in use: moor context lists them")
			}
			out := cmd.OutOrStdout()
			c, err := client.New(ctx.Server, ctx.Token, ctx.CACert)
			if err == nil {
				err = c.Logout(cmd.Context())
			}
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "The token could not be revoked (%v): it still works. Revoke it on the server with moor tokens revoke.\n", err)
			}
			name := cfg.Current
			delete(cfg.Contexts, name)
			cfg.Current = ""
			if err := cfg.save(); err != nil {
				return err
			}
			fmt.Fprintf(out, "Signed out of %s.\n", ctx.Server)
			return nil
		},
	}
	return cmd
}

func newWhoamiCmd() *cobra.Command {
	var (
		conn   connection
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show who you are signed in as, and on which server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			me, err := c.Me(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), me)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s on %s\n", whoLine(me), c.Server())
			if len(me.Scopes) > 0 {
				fmt.Fprintf(out, "Token scopes: %s\n", strings.Join(me.Scopes, ", "))
			}
			if me.App != "" {
				fmt.Fprintf(out, "Token restricted to the app %s\n", me.App)
			}
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func whoLine(me apitypes.Me) string {
	if me.Local {
		return "The server's local administrator"
	}
	return fmt.Sprintf("%s (%s)", me.Email, me.Role)
}
