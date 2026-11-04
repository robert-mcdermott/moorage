// Package dotenv reads and writes NAME=value lines, as `.env` files have them.
package dotenv

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Var is a line's name and value.
type Var struct{ Name, Value string }

// Parse reads NAME=value lines, sorted by name. Single-quoted values are taken as they
// are, double-quoted ones with Go's escapes, and bare ones up to a " #" comment. The last
// of a name wins, as in a shell.
func Parse(r io.Reader) ([]Var, error) {
	var out []Var
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("line %d is not NAME=value", n)
		}
		value, err := readValue(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}
		if i, dup := seen[name]; dup {
			out[i].Value = value
			continue
		}
		seen[name] = len(out)
		out = append(out, Var{name, value})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readValue reads the value part of a line: quoted (and followed by nothing but a
// comment), or bare up to a " #" comment.
func readValue(v string) (string, error) {
	if v == "" || (v[0] != '\'' && v[0] != '"') {
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		return v, nil
	}
	end := -1
	if v[0] == '\'' {
		end = strings.IndexByte(v[1:], '\'') + 1
	} else {
		for i := 1; i < len(v); i++ {
			if v[i] == '\\' {
				i++
			} else if v[i] == '"' {
				end = i
				break
			}
		}
	}
	if end <= 0 {
		return "", fmt.Errorf("the quote is not closed")
	}
	if rest := strings.TrimSpace(v[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("unexpected %q after the closing quote", rest)
	}
	if v[0] == '\'' {
		return v[1:end], nil
	}
	return strconv.Unquote(v[:end+1])
}

// Quote writes a value so that Parse reads it back.
func Quote(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\r\n\"'#\\$`") {
		return v
	}
	return strconv.Quote(v)
}
