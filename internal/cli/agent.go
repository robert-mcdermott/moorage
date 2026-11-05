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
			// Nothing is written unless all of it can be.
			paths := []string{"AGENTS.md"}
			for _, d := range agentSkillDirs {
				paths = append(paths, filepath.Join(d, "SKILL.md"))
			}
			for _, p := range paths {
				if err := notThroughALink(dir, p); err != nil {
					return err
				}
			}
			path := filepath.Join(dir, "AGENTS.md")
			agents, err := agentsWithSection(path, app)
			if err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
			if err != nil {
				return err
			}
			f.Close()
			out := cmd.OutOrStdout()
			for _, d := range agentSkillDirs {
				path := filepath.Join(dir, d, "SKILL.md")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				// A checkout on Windows can change the line endings of what is embedded, and a skill's header wants these.
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(agentSkill, "\r\n", "\n")), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(out, "Wrote %s\n", path)
			}
			if err := os.WriteFile(path, []byte(agents), 0o644); err != nil {
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

// notThroughALink refuses a path in a repository with a link along it, as a repository can hold one, and this would write through it to wherever it leads.
func notThroughALink(dir, rel string) error {
	path := dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		fi, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a link, and this writes nothing through one", path)
		}
	}
	return nil
}

// agentsWithSection is AGENTS.md with the section in it, between markers so that it can be brought up to date, and nothing else changed.
func agentsWithSection(path, app string) (string, error) {
	line := "Its app is named in `moorage.toml`."
	if app != "" {
		line = "Its app is `" + app + "`."
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	text := string(existing)
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	body := strings.ReplaceAll(strings.ReplaceAll(agentsSection, "\r\n", "\n"), "APP_LINE", line)
	section := strings.ReplaceAll(agentsStart+"\n"+strings.TrimRight(body, "\n")+"\n"+agentsEnd+"\n", "\n", eol)
	start, end, found, err := agentsMarkers(text)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %w: put them right, or take them out, and run this again", path, err)
	case found:
		return text[:start] + section + text[end:], nil
	case strings.TrimSpace(text) == "":
		return "# Instructions for coding agents" + eol + eol + section, nil
	}
	return strings.TrimRight(text, "\r\n") + eol + eol + section, nil
}

// agentsMarkers finds the section: from the start of the line with the one marker to the end of the line with the other. Each has to be a whole line, outside a code
// block, where a document that says how this works may show them. Anything else that looks like a section is left for a person, as this would otherwise replace what is between.
func agentsMarkers(text string) (start, end int, found bool, err error) {
	var fence string
	starts, ends := 0, 0
	for offset := 0; offset < len(text); {
		next := strings.IndexByte(text[offset:], '\n')
		lineEnd := len(text)
		if next >= 0 {
			lineEnd = offset + next + 1
		}
		line := strings.TrimRight(text[offset:lineEnd], "\r\n")
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case fence != "":
			if len(line)-len(trimmed) <= 3 && strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]+" \t") == "" {
				fence = ""
			}
		case len(line)-len(trimmed) <= 3 && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")):
			run := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
			fence = trimmed[:run]
		case strings.TrimSpace(line) == agentsStart:
			if starts++; starts == 1 {
				start = offset
			}
			if ends > 0 {
				return 0, 0, false, errors.New("its Moorage section ends before it starts")
			}
		case strings.TrimSpace(line) == agentsEnd:
			if ends++; ends == 1 {
				end = lineEnd
			}
			if starts == 0 {
				return 0, 0, false, errors.New("its Moorage section ends before it starts")
			}
		}
		offset = lineEnd
	}
	switch {
	case starts == 0 && ends == 0:
		return 0, 0, false, nil
	case starts != 1 || ends != 1:
		return 0, 0, false, fmt.Errorf("it has %d lines that start a Moorage section and %d that end one", starts, ends)
	}
	return start, end, true, nil
}
