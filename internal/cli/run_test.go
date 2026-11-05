package cli

import (
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// What a server runs has to be what was typed, word for word.
func TestCommandFrom(t *testing.T) {
	for _, words := range [][]string{
		{"python", "manage.py", "migrate"},
		{"sh", "-c", "echo a; echo b >&2"},
		{"python", "-c", "print('hi there')"},
		{"echo", "$HOME", "*", "a\nb", ""},
		{"./seed.sh", `say "that"`, `back\slash`},
	} {
		line := commandFrom(words)
		file, err := syntax.NewParser().Parse(strings.NewReader(line), "")
		if err != nil {
			t.Errorf("%q: %v", line, err)
			continue
		}
		var got []string
		syntax.Walk(file, func(n syntax.Node) bool {
			if call, ok := n.(*syntax.CallExpr); ok {
				fields, err := expand.Fields(&expand.Config{}, call.Args...)
				if err != nil {
					t.Fatal(err)
				}
				got = fields
				return false
			}
			return true
		})
		if strings.Join(got, "\x00") != strings.Join(words, "\x00") {
			t.Errorf("%q reads as %q, and not %q", line, got, words)
		}
	}
	// One word is a command line, and may be a whole script.
	if got := commandFrom([]string{"echo a && echo b"}); got != "echo a && echo b" {
		t.Errorf("%q", got)
	}
}
