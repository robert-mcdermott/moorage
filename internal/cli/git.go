package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/names"
)

// defaultRemote is the name moor git remote gives the app's remote.
const defaultRemote = "moorage"

func newGitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git",
		Short: "Deploy with git push",
		Long: `Push to an app's repository on the server to deploy it: the production branch
(main, unless the first push named another) deploys to production.

  moor git remote            add the "moorage" remote to this repository
  git push moorage main      deploy; the build is shown as it happens`,
	}
	cmd.AddCommand(newGitRemoteCmd(), newGitCredentialCmd())
	return cmd
}

func newGitRemoteCmd() *cobra.Command {
	var (
		conn      connection
		app, name string
		overHTTPS bool
	)
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Add the app's git remote to this repository",
		Long: `Add a git remote for the app to the repository in the current directory, so
that git push deploys it. The remote uses SSH, with a key registered with
moor keys add; with --http, it uses HTTPS and the API token, which
moor git credential gives git.

The app is --app, $MOOR_APP, [app].name in moorage.toml, or the directory's
name. It need not exist yet: the first push creates it.`,
		Example: `  moor git remote
  moor git remote --http
  git push moorage main`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := exec.LookPath("git"); err != nil {
				return errors.New("git is not installed or not on PATH")
			}
			if out, err := exec.Command("git", "rev-parse", "--git-dir").CombinedOutput(); err != nil {
				return fmt.Errorf("the current directory is not in a git repository: %s", strings.TrimSpace(string(out)))
			}
			appName, _, err := resolveApp(app)
			if err != nil {
				appName, err = dirApp()
				if err != nil {
					return err
				}
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			info, err := c.ServerInfo(cmd.Context())
			if err != nil {
				return err
			}
			prefix := info.Git.SSH
			if overHTTPS || prefix == "" {
				prefix = info.Git.HTTP
			}
			remote := prefix + "/" + appName + ".git"
			verb := "Added"
			if exec.Command("git", "remote", "get-url", name).Run() == nil {
				verb = "Changed"
				err = runGit("remote", "set-url", name, remote)
			} else {
				err = runGit("remote", "add", name, remote)
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s the remote %s: %s\n", verb, name, remote)
			if strings.HasPrefix(remote, "ssh:") {
				if keys, err := c.SSHKeys(cmd.Context()); err == nil && len(keys) == 0 {
					fmt.Fprintln(out, "No SSH key may push yet: register yours with moor keys add (or use --http).")
				}
			} else {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				// The empty helper first clears the helpers configured
				// elsewhere, such as a keychain, so that none of them answers
				// with an old token, or stores this one.
				key := "credential." + info.Git.HTTP + ".helper"
				runGit("config", "--local", "--unset-all", key) // there may be none
				for _, helper := range []string{"", fmt.Sprintf("!%q git credential", filepath.ToSlash(exe))} {
					if err := runGit("config", "--local", "--add", key, helper); err != nil {
						return err
					}
				}
				fmt.Fprintln(out, "git gets the token from moor git credential.")
			}
			fmt.Fprintf(out, "Deploy with: git push %s %s\n", name, currentBranch())
			return nil
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "app (default: $MOOR_APP, [app].name in moorage.toml, or the directory's name)")
	f.StringVar(&name, "name", defaultRemote, "the remote's name")
	f.BoolVar(&overHTTPS, "http", false, "push over HTTPS with the API token instead of SSH")
	return cmd
}

func newGitCredentialCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "credential [get|store|erase]",
		Short: "Give git the API token for HTTPS pushes",
		Long: `A git credential helper: git asks it for the password of the server's git
URL, and it answers with the API token. moor git remote --http sets it up
for the repository; it answers only for the Moorage server's own git URL.`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"get", "store", "erase"},
		Hidden:    true,
		RunE: func(cmd *cobra.Command, args []string) error {
			attrs := readCredential(cmd.InOrStdin())
			if len(args) == 0 || args[0] != "get" {
				return nil // nothing is stored, so nothing is erased
			}
			serverURL, token, err := conn.resolve()
			if err != nil || token == "" {
				return nil // git asks the next helper, or the user
			}
			c, err := conn.client()
			if err != nil {
				return nil
			}
			info, err := c.ServerInfo(context.WithoutCancel(cmd.Context()))
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "moor git credential: %s: %v\n", serverURL, err)
				return nil
			}
			git, err := url.Parse(info.Git.HTTP)
			if err != nil || attrs["host"] != git.Host || (attrs["protocol"] != "" && attrs["protocol"] != git.Scheme) {
				return nil // not this server's git URL: the token is not given away
			}
			fmt.Fprintf(cmd.OutOrStdout(), "username=moor\npassword=%s\n", token)
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}

// readCredential reads git's key=value lines, up to a blank line.
func readCredential(r io.Reader) map[string]string {
	attrs := map[string]string{}
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	return attrs
}

// remoteApp returns the app of the repository's git remote named name, or
// "" if there is none (or no git).
func remoteApp(name string) string {
	out, err := exec.Command("git", "remote", "get-url", name).Output()
	if err != nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	app := strings.TrimSuffix(filepath.Base(u.Path), ".git")
	if names.ValidateApp(app) != nil {
		return ""
	}
	return app
}

// dirApp names the app after the current directory, when that is a valid
// app name.
func dirApp() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	app := strings.ToLower(filepath.Base(wd))
	if err := names.ValidateApp(app); err != nil {
		return "", fmt.Errorf("which app? %q is not a valid app name: pass --app NAME", app)
	}
	return app, nil
}

func currentBranch() string {
	out, err := exec.Command("git", "symbolic-ref", "--short", "HEAD").Output()
	if b := strings.TrimSpace(string(out)); err == nil && b != "" {
		return b
	}
	return "main"
}

func runGit(args ...string) error {
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
