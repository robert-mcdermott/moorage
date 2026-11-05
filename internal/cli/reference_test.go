package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestCommandReference keeps docs/cli.md the same as the commands themselves. UPDATE_CLI_DOC=1 writes it again.
func TestCommandReference(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "cli.md")
	want := commandReference(newRoot(io.Discard, io.Discard))
	if os.Getenv("UPDATE_CLI_DOC") != "" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != want {
		t.Error("docs/cli.md is not what the commands say: run UPDATE_CLI_DOC=1 go test ./internal/cli -run TestCommandReference")
	}
}

// commonFlags are those of every command that talks to a server, said once at the top.
var commonFlags = map[string]bool{"server": true, "token": true, "ca-cert": true, "help": true}

func commandReference(root *cobra.Command) string {
	var b strings.Builder
	b.WriteString(`# Command line

Every command of ` + "`moor`" + `, as its ` + "`--help`" + ` says it. This page is made from the commands themselves, and a test keeps
it the same as they are. [The documentation](README.md) lists the common ones.

An app is chosen with ` + "`--app`" + `, ` + "`$MOOR_APP`" + `, ` + "`[app].name`" + ` in the ` + "`moorage.toml`" + ` of the current directory, or the repository's
` + "`moorage`" + ` git remote.

Commands that talk to a server also take these, which are left out below:

| Flag | What it says |
|---|---|
| ` + "`--server URL`" + ` | The server. By default ` + "`$MOOR_SERVER`" + `, or the server configured on this machine |
| ` + "`--token TOKEN`" + ` | An API token. By default ` + "`$MOOR_TOKEN`" + `, or the admin token of the server on this machine |
| ` + "`--ca-cert FILE`" + ` | A PEM file of an extra CA to trust, such as the server's internal CA. By default ` + "`$MOOR_CA_CERT`" + ` |

`)
	for _, c := range visible(root) {
		writeCommand(&b, c, 2)
	}
	return b.String()
}

// visible lists a command's subcommands that are there for people, by name.
func visible(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range c.Commands() {
		if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		out = append(out, sub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func writeCommand(b *strings.Builder, c *cobra.Command, level int) {
	fmt.Fprintf(b, "%s `%s`\n\n", strings.Repeat("#", min(level, 4)), c.CommandPath())
	text := c.Long
	if text == "" {
		text = c.Short
	}
	writeProse(b, text)
	if c.Runnable() {
		fmt.Fprintf(b, "```text\n%s\n```\n\n", usageLine(c))
	}
	if c.Example != "" {
		fmt.Fprintf(b, "```text\n%s\n```\n\n", exampleBlock(c.Example))
	}
	writeFlags(b, c)
	for _, sub := range visible(c) {
		writeCommand(b, sub, level+1)
	}
}

func usageLine(c *cobra.Command) string {
	return strings.TrimSuffix(strings.TrimSpace(c.UseLine()), " [flags]")
}

func exampleBlock(example string) string {
	lines := strings.Split(strings.Trim(example, "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "\n")
}

var mdSpecial = regexp.MustCompile(`([\\*_<>\[\]|` + "`" + `#])`)

// plainText makes help text safe as Markdown: it means what it says, and nothing in it is a link or emphasis.
func plainText(s string) string { return mdSpecial.ReplaceAllString(s, `\$1`) }

// writeProse writes a command's long help: paragraphs as paragraphs, and what is laid out by hand as it is.
func writeProse(b *strings.Builder, text string) {
	for _, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		lines := strings.Split(para, "\n")
		laidOut := false
		for _, line := range lines {
			if strings.HasPrefix(line, " ") {
				laidOut = true
			}
		}
		if laidOut {
			fmt.Fprintf(b, "```text\n%s\n```\n\n", para)
			continue
		}
		fmt.Fprintf(b, "%s\n\n", plainText(strings.Join(lines, " ")))
	}
}

func writeFlags(b *strings.Builder, c *cobra.Command) {
	var flags []*pflag.Flag
	add := func(f *pflag.Flag) {
		if !f.Hidden && !commonFlags[f.Name] {
			flags = append(flags, f)
		}
	}
	c.NonInheritedFlags().VisitAll(add)
	c.InheritedFlags().VisitAll(add)
	if len(flags) == 0 {
		return
	}
	sort.Slice(flags, func(i, j int) bool { return flags[i].Name < flags[j].Name })
	b.WriteString("| Flag | Default | What it does |\n|---|---|---|\n")
	for _, f := range flags {
		kind, usage := pflag.UnquoteUsage(f)
		name := "--" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", " + name
		}
		if kind != "" {
			name += " " + kind
		}
		fmt.Fprintf(b, "| `%s` | %s | %s |\n", name, flagDefault(f), plainText(usage))
	}
	b.WriteString("\n")
}

func flagDefault(f *pflag.Flag) string {
	switch f.DefValue {
	case "", "false", "[]", "0":
		return ""
	case runtime.GOOS:
		return "this system"
	}
	return "`" + f.DefValue + "`"
}
