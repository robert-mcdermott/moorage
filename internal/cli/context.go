package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/pelletier/go-toml/v2"

	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/spf13/cobra"
)

// clientConfig is the command line's own configuration: the servers it has
// signed in to. It holds API tokens, so it is readable only by its owner.
type clientConfig struct {
	Current  string                   `toml:"current,omitempty"`
	Contexts map[string]clientContext `toml:"contexts,omitempty"`
}

// clientContext is one server and the token for it.
type clientContext struct {
	Server string `toml:"server"`
	Token  string `toml:"token"`
	CACert string `toml:"ca_cert,omitempty"`
	User   string `toml:"user,omitempty"`
}

// clientConfigPath is $MOOR_CONFIG, or moor's place for it on this platform.
func clientConfigPath() (string, error) {
	if p := os.Getenv("MOOR_CONFIG"); p != "" {
		return p, nil
	}
	return platform.ClientConfigFile()
}

func loadClientConfig() (*clientConfig, error) {
	cfg := &clientConfig{}
	path, err := clientConfigPath()
	if err != nil {
		// No home directory is no configuration: the other sources still work.
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *clientConfig) save() error {
	path, err := clientConfigPath()
	if err != nil {
		return err
	}
	data, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// current returns the current context, if any.
func (c *clientConfig) current() *clientContext {
	if ctx, ok := c.Contexts[c.Current]; ok {
		return &ctx
	}
	return nil
}

// forServer returns the context of a server, if any.
func (c *clientConfig) forServer(serverURL string) *clientContext {
	serverURL = strings.TrimSuffix(serverURL, "/")
	if ctx := c.current(); ctx != nil && ctx.Server == serverURL {
		return ctx
	}
	for _, name := range c.names() {
		if ctx := c.Contexts[name]; ctx.Server == serverURL {
			return &ctx
		}
	}
	return nil
}

func (c *clientConfig) names() []string {
	names := make([]string, 0, len(c.Contexts))
	for name := range c.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// contextName names a server's context after its hostname.
func contextName(serverURL string) string {
	if u, err := url.Parse(serverURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return serverURL
}

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "List the servers you have signed in to",
		Long: `A context is a server and the token for it, saved by moor login. Commands use
the current one, unless --server or $MOOR_SERVER says otherwise.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadClientConfig()
			if err != nil {
				return err
			}
			if len(cfg.Contexts) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No contexts yet. Sign in to a server with: moor login URL")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "\tNAME\tSERVER\tUSER")
			for _, name := range cfg.names() {
				mark := ""
				if name == cfg.Current {
					mark = "*"
				}
				ctx := cfg.Contexts[name]
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, name, ctx.Server, ctx.User)
			}
			return w.Flush()
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "use NAME",
		Short: "Make a context the current one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadClientConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Contexts[args[0]]; !ok {
				return fmt.Errorf("there is no context %q: moor context lists them", args[0])
			}
			cfg.Current = args[0]
			if err := cfg.save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Now using %s (%s).\n", args[0], cfg.Contexts[args[0]].Server)
			return nil
		},
	}, &cobra.Command{
		Use:   "rm NAME",
		Short: "Forget a context, without revoking its token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadClientConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Contexts[args[0]]; !ok {
				return fmt.Errorf("there is no context %q: moor context lists them", args[0])
			}
			delete(cfg.Contexts, args[0])
			if cfg.Current == args[0] {
				cfg.Current = ""
			}
			if err := cfg.save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Forgot %s. Its token still works until it is revoked (moor tokens revoke).\n", args[0])
			return nil
		},
	})
	return cmd
}
