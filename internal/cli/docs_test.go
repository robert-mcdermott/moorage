package cli

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	inlineCommand = regexp.MustCompile("`(moor [^`]+)`")
	shellBlock    = regexp.MustCompile("(?s)```bash\n(.*?)```")
	shellCommand  = regexp.MustCompile(`(?m)^\s*(?:sudo )?(moor [^\n]+)$`)
	// A quoted word is one word, and an option in brackets may not be there.
	quoted   = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	optional = regexp.MustCompile(`\[[^\]]*\]`)
)

// TestDocumentedCommands checks every moor command the documentation shows: that
// it exists, and takes the flags shown.
func TestDocumentedCommands(t *testing.T) {
	root := filepath.Join("..", "..")
	var files []string
	for _, pattern := range []string{"README.md", "docs/*.md", "examples/*.md", "examples/*/README.md"} {
		found, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, found...)
	}
	if len(files) < 15 {
		t.Fatalf("only %d files", len(files))
	}
	seen := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		var commands []string
		for _, m := range inlineCommand.FindAllStringSubmatch(text, -1) {
			commands = append(commands, m[1])
		}
		for _, block := range shellBlock.FindAllStringSubmatch(text, -1) {
			for _, m := range shellCommand.FindAllStringSubmatch(block[1], -1) {
				commands = append(commands, m[1])
			}
		}
		for _, command := range commands {
			seen++
			if err := checkCommand(command); err != "" {
				t.Errorf("%s: %q: %s", filepath.Base(file), command, err)
			}
		}
	}
	if seen < 50 {
		t.Errorf("only %d commands found: the patterns may have stopped matching", seen)
	}
}

func checkCommand(line string) string {
	line = quoted.ReplaceAllString(line, "word")
	line = optional.ReplaceAllString(line, "")
	var args []string
	for _, f := range strings.Fields(line)[1:] {
		// What a shell does with the rest is its own affair.
		if f == "|" || f == "&&" || f == ";" || f == "#" || strings.HasPrefix(f, ">") || strings.HasPrefix(f, "<") {
			break
		}
		args = append(args, f)
	}
	cmd, rest, err := newRoot(io.Discard, io.Discard).Find(args)
	if err != nil {
		return err.Error()
	}
	if err := cmd.ParseFlags(rest); err != nil {
		// Prose names a flag without its value.
		if strings.Contains(err.Error(), "flag needs an argument") {
			return ""
		}
		return cmd.CommandPath() + ": " + err.Error()
	}
	positional := cmd.Flags().Args()
	if !cmd.Runnable() && len(positional) > 0 {
		return cmd.CommandPath() + " has no subcommand " + positional[0]
	}
	// Prose may leave out a command's words, but not add to them.
	if err := cmd.ValidateArgs(positional); err != nil && len(positional) > 0 {
		return cmd.CommandPath() + ": " + err.Error()
	}
	return ""
}
