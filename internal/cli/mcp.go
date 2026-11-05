// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/ignore"
	"github.com/robert-mcdermott/moorage/internal/mcpserver"
)

// agentContext is the context that moor mcp uses, unless told another.
const agentContext = "agent"

// mcpStartLimit is how long moor mcp waits to find out who its token is.
const mcpStartLimit = 15 * time.Second

func newMCPCmd() *cobra.Command {
	var (
		contextName, app, root string
		allowDeploy            bool
	)
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve coding agents over the Model Context Protocol",
		Long: `Serve coding agents over the Model Context Protocol, on standard input and output. A client runs this
for itself: moor mcp setup says how, and makes the token it needs.

It reads apps, deployments, releases and logs. With --allow-deploy, it can also deploy, roll back and restart. Nothing
here runs commands, reads secrets or deletes anything. check_manifest and deploy read files below the directory it started in, or
--root, and only if that is not your home directory or above it.

Its token is what limits it, so it refuses one that can do everything, and does not fall back on what moor login
stored. It takes the context named agent, or another by --context, or $MOOR_SERVER and $MOOR_TOKEN. Logs are written by the
app and whoever can reach it, and an agent should not be trusted further for having read them.`,
		Example: `  moor mcp setup --client claude-code --app shop
  moor mcp --allow-deploy --app shop`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := agentClient(contextName)
			if err != nil {
				return err
			}
			// A client waits on this to start, and gives up without saying why.
			asking, cancel := context.WithTimeout(cmd.Context(), mcpStartLimit)
			defer cancel()
			me, err := c.Me(asking)
			if err != nil {
				return err
			}
			if err := agentMayUse(me, allowDeploy); err != nil {
				return err
			}
			if root == "" {
				if root, err = os.Getwd(); err != nil {
					return err
				}
			} else if root, err = filepath.Abs(root); err != nil {
				return err
			}
			transport := &mcp.IOTransport{Reader: readCloser{cmd.InOrStdin()}, Writer: writeCloser{cmd.OutOrStdout()}}
			return mcpserver.Run(cmd.Context(), mcpserver.Options{
				Client: c, App: firstOf(app, os.Getenv("MOOR_APP")), AllowDeploy: allowDeploy, Root: root, Skip: skipFor, Version: buildinfo.Version,
			}, transport)
		},
	}
	cmd.AddCommand(newMCPSetupCmd())
	f := cmd.Flags()
	f.StringVar(&contextName, "context", agentContext, "the context to use, from moor mcp setup")
	f.StringVar(&app, "app", "", "the app to use when a tool is not told")
	f.BoolVar(&allowDeploy, "allow-deploy", false, "offer deploying, rolling back and restarting; the token has to allow them too")
	f.StringVar(&root, "root", "", "the directory that local files are read from, and only below it; where this started if left out")
	return cmd
}

// agentClient is the one connection that moor mcp will use: never the signed-in context, or the local server's admin token.
func agentClient(name string) (*client.Client, error) {
	server, token := os.Getenv("MOOR_SERVER"), os.Getenv("MOOR_TOKEN")
	if server != "" && token != "" {
		return client.New(server, token, os.Getenv("MOOR_CA_CERT"))
	}
	cfg, err := loadClientConfig()
	if err != nil {
		return nil, err
	}
	ctx, ok := cfg.Contexts[name]
	if !ok {
		return nil, fmt.Errorf("there is no context named %s: run moor mcp setup, which makes a token for an agent", name)
	}
	return client.New(ctx.Server, ctx.Token, firstOf(os.Getenv("MOOR_CA_CERT"), ctx.CACert))
}

// agentMayUse refuses a token that is more than an agent should have. An agent with a shell can go around anything else here, so that this is
// what counts: see spec/14-agents.md.
func agentMayUse(me apitypes.Me, allowDeploy bool) error {
	// No scopes at all is a session, or the local server's own token. A token made by that one has scopes, like any other.
	if slices.Contains(me.Scopes, auth.ScopeAdmin) || len(me.Scopes) == 0 {
		return errors.New("this token can do everything, and an agent should not have one that can: run moor mcp setup, which makes a narrow one")
	}
	can := func(scope string) bool { return slices.Contains(me.Scopes, scope) }
	if !can(auth.ScopeRead) && !can(auth.ScopeDeploy) {
		return errors.New("this token cannot read: an agent needs a token with the read or deploy scope")
	}
	if allowDeploy && !can(auth.ScopeDeploy) {
		return errors.New("--allow-deploy needs a token that can deploy: run moor mcp setup --deploy")
	}
	return nil
}

// skipFor is what an upload of a directory leaves out, as moor deploy does.
func skipFor(dir string) (func(rel string, d fs.DirEntry) bool, error) {
	ignored, err := ignore.ForTree(dir, ".gitignore", ".moorignore")
	if err != nil {
		return nil, err
	}
	return func(rel string, d fs.DirEntry) bool {
		return alwaysLeftOut[d.Name()] || ignored.Match(rel, d.IsDir())
	}, nil
}

// readCloser and writeCloser keep the transport from closing standard input and output, which are not its own.
type readCloser struct{ io.Reader }

func (readCloser) Close() error { return nil }

type writeCloser struct{ io.Writer }

func (writeCloser) Close() error { return nil }
