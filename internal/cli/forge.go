package cli

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

func newForgeCmd() *cobra.Command {
	var f appFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "forge",
		Short: "Deploy from a repository on GitHub, GitLab or Gitea",
		Long: `Connect an app to a repository on a forge. A push to its default branch deploys to
production, a push to a branch that an environment names deploys to that environment,
and each pull request gets a preview of its own, which reports back to the pull request
if the server has a token to do it with.

A pull request from a fork, or from someone the forge does not say may push there, is
not built until someone approves that commit: building it would run their code here.`,
		Example: `  moor forge connect https://github.com/acme/shop.git --token-stdin
  moor forge pulls
  moor forge approve 42 1f3a9c07
  moor forge sync`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			conn, err := c.Forge(cmd.Context(), app)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), conn)
			}
			printForge(cmd, conn)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(newForgeConnectCmd(), newForgeDisconnectCmd(), newForgePullsCmd(), newForgeApproveCmd(), newForgeSyncCmd())
	return cmd
}

func printForge(cmd *cobra.Command, conn apitypes.ForgeConnection) {
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Repository\t%s\n", conn.URL)
	fmt.Fprintf(w, "Kind\t%s\n", conn.Kind)
	fmt.Fprintf(w, "Default branch\t%s\n", conn.DefaultBranch)
	how := "the forge tells it"
	if conn.Poll {
		how = "it looks every minute"
	}
	fmt.Fprintf(w, "Changes\t%s\n", how)
	if !conn.Poll {
		fmt.Fprintf(w, "Hook\t%s\n", conn.HookURL)
	}
	fmt.Fprintf(w, "Previews\t%s\n", onOff(conn.Previews))
	fmt.Fprintf(w, "Token\t%s\n", yesNo(conn.HasToken))
	if !conn.LastEventAt.IsZero() {
		fmt.Fprintf(w, "Last heard\t%s\n", ago(conn.LastEventAt))
	}
	if conn.LastError != "" {
		fmt.Fprintf(w, "Problem\t%s\n", conn.LastError)
	}
	w.Flush()
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func yesNo(yes bool) string {
	if yes {
		return "yes"
	}
	return "no"
}

func newForgeConnectCmd() *cobra.Command {
	var f appFlags
	var in apitypes.ForgeConnect
	var tokenStdin bool
	cmd := &cobra.Command{
		Use:   "connect URL",
		Short: "Connect an app to a repository",
		Long: `Connect an app to a repository, by its https address. It answers with an address and
a secret for the forge's webhook settings: the secret is shown this once.

A token reads a private repository, and sets the checks and comments on pull requests.
Give it on standard input, or be asked for it, so that it stays out of your history.
A server that a forge cannot reach can look for itself, every minute, with --poll:
it then sees branches, and no pull requests.

Connecting deploys nothing. The next push does, or: moor forge sync`,
		Example: `  moor forge connect https://github.com/acme/shop.git --token-stdin < token
  moor forge connect https://git.example.com/acme/shop.git --kind gitea
  moor forge connect https://github.com/acme/shop.git --poll`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			in.URL = args[0]
			if tokenStdin {
				if in.Token, err = readSecret(cmd, "the token"); err != nil {
					return err
				}
			}
			conn, err := c.ConnectForge(cmd.Context(), app, in)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s deploys from %s, whose default branch is %s.\n", app, conn.URL, conn.DefaultBranch)
			if conn.LastError != "" {
				fmt.Fprintf(out, "\nBut: %s\n", conn.LastError)
			}
			switch {
			case conn.Poll:
				fmt.Fprintln(out, "\nIt looks every minute, and deploys what has changed.")
			case conn.Secret != "":
				fmt.Fprintf(out, "\nGive your forge a webhook, for pushes and pull requests, sending JSON:\n\n  Address  %s\n  Secret   %s\n\nThe secret is not shown again.\n", conn.HookURL, conn.Secret)
			default:
				fmt.Fprintf(out, "\nIts webhook stays as it was: %s\n", conn.HookURL)
			}
			if !conn.HasToken {
				fmt.Fprintln(out, "\nWith no token, it cannot read a private repository, or report back to pull requests.")
			}
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&in.Kind, "kind", "", "github, gitlab, gitea or generic (github.com and gitlab.com need not say)")
	cmd.Flags().StringVar(&in.APIURL, "api-url", "", "where the forge's API is, if not where it usually is")
	cmd.Flags().StringVar(&in.DefaultBranch, "branch", "", "its production branch (default: what the repository says)")
	cmd.Flags().BoolVar(&in.NoPreviews, "no-previews", false, "leave pull requests alone")
	cmd.Flags().BoolVar(&in.Poll, "poll", false, "look every minute, for a server that the forge cannot reach")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read a token from standard input, or ask for it")
	return cmd
}

func newForgeDisconnectCmd() *cobra.Command {
	var f appFlags
	cmd := &cobra.Command{
		Use:   "disconnect",
		Short: "Stop deploying from a repository",
		Long:  "Let go of a repository, its secret and its token. What it deployed stays, previews too.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if err := c.DisconnectForge(cmd.Context(), app); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s no longer deploys from a repository. Its webhook can go, too.\n", app)
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func newForgePullsCmd() *cobra.Command {
	var f appFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "pulls",
		Short: "List pull requests and their previews",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			pulls, err := c.ForgePulls(cmd.Context(), app)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), pulls)
			}
			if len(pulls) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s has no open pull requests that it has heard of.\n", app)
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PULL\tSTATE\tCOMMIT\tAUTHOR\tTITLE\tPREVIEW")
			for _, p := range pulls {
				commit := p.Commit
				if len(commit) > 8 {
					commit = commit[:8]
				}
				where := p.URL
				if where == "" {
					where = p.Error
				}
				author := p.Author
				if p.FromFork {
					author += " (fork)"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", p.Number, p.State, commit, author, p.Title, where)
			}
			return w.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newForgeApproveCmd() *cobra.Command {
	var f appFlags
	cmd := &cobra.Command{
		Use:   "approve NUMBER COMMIT",
		Short: "Build a pull request's code",
		Long: `Have a pull request built, at the commit that you looked at, as moor forge pulls shows it. Building it
runs its code on this server, and its preview has the app's preview variables, so look at it first.
This is for that one commit: if it has moved on, nothing is built, and another commit needs
approving too. It also builds again one that failed.`,
		Example: "  moor forge approve 42 1f3a9c07",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("%q is not the number of a pull request", args[0])
			}
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if _, err := c.ApproveForgePull(cmd.Context(), app, number, args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Building %s of pull request %d. See how it goes with: moor forge pulls\n", args[1], number)
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func newForgeSyncCmd() *cobra.Command {
	var f appFlags
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Look at the repository now",
		Long: `Have the server look at the repository now, and deploy what has changed. This is how
what is there already gets its first deployment, and how one that failed gets another try.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if err := c.SyncForge(cmd.Context(), app); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "It is looking. See how it goes with: moor deployments --app %s\n", app)
			return nil
		},
	}
	f.add(cmd)
	return cmd
}
