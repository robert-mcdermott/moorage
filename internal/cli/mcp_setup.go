// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

// agentTokenPrefix starts the name of each token that moor mcp setup makes.
const agentTokenPrefix = "moor-mcp-"

// agentClientSetup says how a coding agent is told of a server. Each is as its own documentation had it on 5 November 2026.
type agentClientSetup struct {
	name string
	// command, if set, is the client's own way of adding a server, with the server's command line at the end.
	command func(moor []string) []string
	// file and format say where else it goes.
	file, format string
	// printOnWindows is for a command that is a batch file there, which would not get the quotes of what it is given.
	printOnWindows bool
}

// mcpServersEntry is the usual shape of a config file's entry.
func mcpServersEntry(moor []string) string {
	data, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"moorage": map[string]any{"command": moor[0], "args": moor[1:]}}}, "", "  ")
	return string(data)
}

var agentClients = map[string]agentClientSetup{
	"claude-code": {name: "Claude Code", command: func(moor []string) []string {
		return append([]string{"claude", "mcp", "add", "--transport", "stdio", "--scope", "user", "moorage", "--"}, moor...)
	}},
	"codex": {name: "Codex CLI", command: func(moor []string) []string {
		return append([]string{"codex", "mcp", "add", "moorage", "--"}, moor...)
	}},
	"vscode": {name: "VS Code", command: func(moor []string) []string {
		data, _ := json.Marshal(map[string]any{"name": "moorage", "type": "stdio", "command": moor[0], "args": moor[1:]})
		return []string{"code", "--add-mcp", string(data)}
	}, printOnWindows: true},
	"gemini": {name: "Gemini CLI", command: func(moor []string) []string {
		// It takes the command and its words after the name, and the server's own flags after --.
		out := []string{"gemini", "mcp", "add", "-s", "user", "moorage"}
		for i, word := range moor {
			if strings.HasPrefix(word, "-") {
				return append(append(append(out, moor[:i]...), "--"), moor[i:]...)
			}
		}
		return append(out, moor...)
	}},
	"cline": {name: "Cline", command: func(moor []string) []string {
		return append([]string{"cline", "mcp", "add", "moorage", "--yes", "--"}, moor...)
	}},
	"cursor":      {name: "Cursor", file: "~/.cursor/mcp.json", format: "mcpServers"},
	"antigravity": {name: "Antigravity CLI", file: "~/.gemini/config/mcp_config.json", format: "mcpServers"},
	"devin":       {name: "Devin Desktop, formerly Windsurf", file: "~/.config/devin/mcp_config.json", format: "mcpServers"},
	"junie":       {name: "Junie", file: "~/.junie/mcp/mcp.json", format: "mcpServers"},
	// The IDE and the CLI read the one file. The CLI's own command takes the server as a single string, and does not say how it splits one with flags in it.
	"kiro":           {name: "Kiro, IDE and CLI", file: "~/.kiro/settings/mcp.json", format: "mcpServers"},
	"jetbrains":      {name: "JetBrains AI Assistant", file: "Settings, Tools, AI Assistant, Model Context Protocol (MCP), Add", format: "mcpServers"},
	"claude-desktop": {name: "Claude Desktop", file: "claude_desktop_config.json", format: "mcpServers"},
	"zed":            {name: "Zed", file: "its settings, under context_servers", format: "zed"},
	"goose":          {name: "Goose", file: "~/.config/goose/config.yaml, under extensions", format: "goose"},
}

// agentClientAliases are other names that people will look for a client by.
var agentClientAliases = map[string]string{"kiro-cli": "kiro", "kiro-ide": "kiro"}

func agentClientNames() []string {
	names := make([]string, 0, len(agentClients))
	for name := range agentClients {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func newMCPSetupCmd() *cobra.Command {
	var (
		conn                                          connection
		clientName, app, envs, expires, ctxName, root string
		deploy, printOnly                             bool
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Make a token for a coding agent, and tell its client of this server",
		Long: `Make a narrow, expiring token for a coding agent, keep it in a context of its own, and tell its client how to run
moor mcp. The token stays in moor's own file, which only you can read, and goes in no client's configuration or repository.

By default the token can only read. With --deploy it can also deploy, roll back and restart, in previews that take nothing of
production's, whose names start with agent-, and it can reach nothing else: --envs all lets it reach every environment, production included.

Running this again replaces the token. With --client, it runs that client's own command where it has one, or says what to
put where. Without, it lists them all.

The clients are: ` + strings.Join(agentClientNames(), ", ") + `.

Code an agent deploys runs on your server. On Linux, an app's environments share a user, so a preview's code can read production's data. To keep
what an agent makes from production altogether, give it an app of its own with --app.`,
		Example: `  moor mcp setup --client claude-code --app shop
  moor mcp setup --client codex --app shop-dev --deploy`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var setup agentClientSetup
			if clientName != "" {
				if name, ok := agentClientAliases[clientName]; ok {
					clientName = name
				}
				var ok bool
				if setup, ok = agentClients[clientName]; !ok {
					return usageError{fmt.Errorf("no client named %s: they are %s", clientName, strings.Join(agentClientNames(), ", "))}
				}
			}
			switch envs {
			case "":
				if deploy {
					envs = auth.EnvsPreviews
				}
			case "all":
				envs = ""
			case auth.EnvsPreviews:
			default:
				return usageError{errors.New("--envs is previews or all")}
			}
			var lifetime manifest.Duration
			if err := lifetime.UnmarshalText([]byte(expires)); err != nil {
				return usageError{fmt.Errorf("--expires: %w", err)}
			}
			serverURL, token, caCert, err := conn.resolve()
			if err != nil {
				return err
			}
			if root != "" {
				if root, err = filepath.Abs(root); err != nil {
					return err
				}
			}
			cfg, err := loadClientConfig()
			if err != nil {
				return err
			}
			// What this replaces has to be its own: moor login's context holds a token that is not for this.
			before, had := cfg.Contexts[ctxName]
			if had && !before.Agent {
				return fmt.Errorf("the context %s is not one that this made, and it would be replaced: name another with --context", ctxName)
			}
			for name, other := range cfg.Contexts {
				if other.Agent && other.Token == token {
					return fmt.Errorf("the context in use, %s, is a coding agent's, and cannot make tokens: moor context use another", name)
				}
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			// The new one first, so that failing here leaves an agent with what it had.
			name, secret, err := makeAgentToken(cmd.Context(), c, ctxName, app, envs, deploy, lifetime)
			if err != nil {
				return err
			}
			if cfg.Contexts == nil {
				cfg.Contexts = map[string]clientContext{}
			}
			serverURL = strings.TrimSuffix(serverURL, "/")
			cfg.Contexts[ctxName] = clientContext{Server: serverURL, Token: secret, CACert: caCert, User: "a coding agent", Agent: true, TokenName: name}
			if err := cfg.save(); err != nil {
				if _, undo := c.RevokeToken(cmd.Context(), name); undo != nil {
					return fmt.Errorf("%w, and the token %s that was made for it could not be revoked: %v", err, name, undo)
				}
				return err
			}
			// Only the one this context held. Another with a name like it may be another machine's.
			if had && before.Server == serverURL && before.TokenName != "" {
				if _, err := c.RevokeToken(cmd.Context(), before.TokenName); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "The token %s it replaces could not be revoked, and works until it expires: %v\n", before.TokenName, err)
				}
			}
			if had && before.Server != serverURL {
				fmt.Fprintf(cmd.ErrOrStderr(), "The context %s held a token for %s, which is not reached from here, and it works until it expires. moor tokens revoke removes it there.\n", ctxName, before.Server)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Made a token for an agent, and kept it in the context %s. %s It expires in %s.\n", ctxName, describeAgent(app, envs, deploy), expires)
			moor := agentCommandLine(ctxName, app, root, deploy)
			if clientName == "" {
				fmt.Fprintln(out, "\nTell your client of it. For each of them:")
				for _, name := range agentClientNames() {
					printAgentSetup(out, agentClients[name], moor)
				}
				return nil
			}
			return applyAgentSetup(cmd, setup, moor, printOnly)
		},
	}
	conn.addFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&clientName, "client", "", "the client to set up")
	f.StringVar(&app, "app", "", "limit the token to one app")
	f.BoolVar(&deploy, "deploy", false, "let it deploy, roll back and restart")
	f.StringVar(&envs, "envs", "", "previews or all; with --deploy, previews unless told otherwise")
	f.StringVar(&expires, "expires", "90d", "how long the token lasts")
	f.StringVar(&ctxName, "context", agentContext, "the context to keep it in")
	f.BoolVar(&printOnly, "print", false, "say what to do, and run nothing of the client's")
	f.StringVar(&root, "root", "", "the directory that moor mcp reads local files from; where the client starts it if left out")
	return cmd
}

// makeAgentToken makes a token for a context, under a name of its own: an older one goes only once this one is kept.
func makeAgentToken(ctx context.Context, c *client.Client, ctxName, app, envs string, deploy bool, lifetime manifest.Duration) (name, secret string, err error) {
	scope := auth.ScopeRead
	if deploy {
		scope = auth.ScopeDeploy
	}
	name = fmt.Sprintf("%s%s.%d", agentTokenPrefix, ctxName, time.Now().UnixMilli())
	made, err := c.CreateToken(ctx, apitypes.NewToken{Name: name, Scopes: []string{scope}, App: app, Envs: envs, ExpiresAt: time.Now().Add(lifetime.Std()).UTC()})
	if err != nil {
		return "", "", err
	}
	return name, made.Secret, nil
}

func describeAgent(app, envs string, deploy bool) string {
	what := "It can read"
	if deploy {
		what = "It can read and deploy"
	}
	if envs == auth.EnvsPreviews {
		what += ", in previews named agent-something"
	}
	if app != "" {
		return what + ", for " + app + "."
	}
	return what + "."
}

// agentCommandLine is what a client runs. A desktop app does not start from a shell, so that it needs the whole path.
func agentCommandLine(ctxName, app, root string, deploy bool) []string {
	path, err := os.Executable()
	if err != nil {
		path = "moor"
	}
	line := []string{path, "mcp"}
	if ctxName != agentContext {
		line = append(line, "--context", ctxName)
	}
	if app != "" {
		line = append(line, "--app", app)
	}
	if root != "" {
		line = append(line, "--root", root)
	}
	if deploy {
		line = append(line, "--allow-deploy")
	}
	return line
}

func printAgentSetup(out io.Writer, s agentClientSetup, moor []string) {
	fmt.Fprintf(out, "\n%s:\n", s.name)
	switch {
	case s.command != nil:
		fmt.Fprintf(out, "  %s\n", strings.Join(quotedWords(s.command(moor)), " "))
	case s.format == "zed":
		data, _ := json.MarshalIndent(map[string]any{"context_servers": map[string]any{"moorage": map[string]any{"command": moor[0], "args": moor[1:]}}}, "  ", "  ")
		fmt.Fprintf(out, "  in %s:\n  %s\n", s.file, data)
	case s.format == "goose":
		fmt.Fprintf(out, "  in %s:\n  moorage:\n    type: stdio\n    enabled: true\n    cmd: %s\n    args: [%s]\n", s.file, moor[0], strings.Join(moor[1:], ", "))
	default:
		fmt.Fprintf(out, "  in %s:\n", s.file)
		for _, line := range strings.Split(mcpServersEntry(moor), "\n") {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
}

// applyAgentSetup runs a client's own command, if it has one and it is installed. What it cannot do, it says.
func applyAgentSetup(cmd *cobra.Command, s agentClientSetup, moor []string, printOnly bool) error {
	out := cmd.OutOrStdout()
	if s.command == nil || printOnly || s.printOnWindows && runtime.GOOS == "windows" {
		printAgentSetup(out, s, moor)
		return nil
	}
	line := s.command(moor)
	if _, err := exec.LookPath(line[0]); err != nil {
		fmt.Fprintf(out, "\n%s is not on this PATH, so run this once it is:\n", line[0])
		printAgentSetup(out, s, moor)
		return nil
	}
	run := exec.CommandContext(cmd.Context(), line[0], line[1:]...)
	run.Stdout, run.Stderr = out, cmd.ErrOrStderr()
	if err := run.Run(); err != nil {
		fmt.Fprintf(out, "\nThat did not work, and this is what it tried:\n")
		printAgentSetup(out, s, moor)
		return err
	}
	fmt.Fprintf(out, "%s now knows of this server.\n", s.name)
	return nil
}

// quotedWords quotes what a shell would otherwise take apart.
func quotedWords(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = w
		if strings.ContainsAny(w, " \"'{}") {
			out[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
	}
	return out
}
