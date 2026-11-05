// SPDX-License-Identifier: Apache-2.0

package cli

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/manifest"
)

//go:embed agentfiles/SKILL.md
var agentSkill string

//go:embed agentfiles/AGENTS-section.md
var agentsSection string

const (
	agentsStart = "<!-- moorage:start -->"
	agentsEnd   = "<!-- moorage:end -->"
)

// agentSkillDirs are where clients look for a repository's skills. Codex, Gemini CLI, VS Code and Cursor read the first, and Claude Code the second.
var agentSkillDirs = []string{filepath.Join(".agents", "skills", "moorage"), filepath.Join(".claude", "skills", "moorage")}

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Tell coding agents how to use Moorage",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newAgentInitCmd())
	return cmd
}

func newAgentInitCmd() *cobra.Command {
	var app string
	cmd := &cobra.Command{
		Use:   "init [DIR]",
		Short: "Add instructions for coding agents to a repository",
		Long: `Add instructions for coding agents to the repository in DIR (default: the current directory): a section of AGENTS.md, and
an Agent Skill in .agents/skills and .claude/skills. They teach an agent to deploy and look at this app with moor or moor mcp, and what to
be careful of. They hold no token, so they can be committed. Running this again brings them up to date, and leaves the rest of AGENTS.md as it was.`,
		Example: `  moor agent init --app shop`,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := firstOf(firstArg(args), ".")
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			if app == "" {
				m, err := manifest.Load(dir)
				if err != nil {
					return err
				}
				if m != nil {
					app = m.App.Name
				}
			}
			out := cmd.OutOrStdout()
			for _, d := range agentSkillDirs {
				path := filepath.Join(dir, d, "SKILL.md")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte(agentSkill), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(out, "Wrote %s\n", path)
			}
			path := filepath.Join(dir, "AGENTS.md")
			if err := writeAgentsSection(path, app); err != nil {
				return err
			}
			fmt.Fprintf(out, "Wrote the Moorage section of %s\n", path)
			if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err == nil {
				fmt.Fprintln(out, "\nThis repository has a CLAUDE.md, so Claude Code will not read AGENTS.md by itself. It will find the skill, or you can add @AGENTS.md to CLAUDE.md.")
			}
			fmt.Fprintln(out, "\nThese hold no token. To let an agent reach the server: moor mcp setup --client NAME")
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "the app; moorage.toml's name if left out")
	return cmd
}

// writeAgentsSection puts the section in AGENTS.md, between markers so that it can be brought up to date, and touches nothing else.
func writeAgentsSection(path, app string) error {
	line := "Its app is named in `moorage.toml`."
	if app != "" {
		line = "Its app is `" + app + "`."
	}
	section := agentsStart + "\n" + strings.TrimRight(strings.ReplaceAll(agentsSection, "APP_LINE", line), "\n") + "\n" + agentsEnd + "\n"
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := strings.ReplaceAll(string(existing), "\r\n", "\n")
	start, end := strings.Index(text, agentsStart), strings.Index(text, agentsEnd)
	switch {
	case start >= 0 && end > start:
		text = text[:start] + section + strings.TrimPrefix(text[end+len(agentsEnd):], "\n")
	case text == "":
		text = "# Instructions for coding agents\n\n" + section
	default:
		text = strings.TrimRight(text, "\n") + "\n\n" + section
	}
	return os.WriteFile(path, []byte(text), 0o644)
}
