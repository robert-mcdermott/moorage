package dotenv

import (
	"fmt"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	vars, err := Parse(strings.NewReader("B=2\nA = 1 # one\nexport C='x y'\nD=\"q\\\"t\"\nB=3\n# comment\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Var{{"A", "1"}, {"B", "3"}, {"C", "x y"}, {"D", `q"t`}}
	if fmt.Sprint(vars) != fmt.Sprint(want) {
		t.Errorf("parsed %v, want %v", vars, want)
	}
	for _, bad := range []string{"NOEQUALS\n", "A=\"open\n", "=1\n", "A='x' y\n"} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
	if _, err := Parse(strings.NewReader("A=1\n\nB\n")); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("the line of a mistake: %v", err)
	}
	for _, v := range []string{"plain", "with space", `quo"te`, "$dollar", "multi\nline", ""} {
		got, err := Parse(strings.NewReader("X=" + Quote(v) + "\n"))
		if err != nil || len(got) != 1 || got[0].Value != v {
			t.Errorf("round trip of %q: %v %v", v, got, err)
		}
	}
}
