// Package docs checks the repository's documents: the README, the
// changelog, docs/, spec/ and examples/.
package docs

import (
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

const root = "../.."

var (
	mdLink   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	htmlLink = regexp.MustCompile(`(?:src|srcset|href)="([^"]+)"`)
	heading  = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*#*\s*$`)
	fence    = regexp.MustCompile("(?ms)^```.*?^```")
	inline   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// TestLinks checks that every relative link in the documents leads to a
// file that exists, and every #anchor to a heading of that file.
func TestLinks(t *testing.T) {
	anchors := map[string]map[string]bool{}
	for _, doc := range documents(t) {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		text := fence.ReplaceAllString(string(data), "")
		var targets []string
		for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
			targets = append(targets, m[1])
		}
		for _, m := range htmlLink.FindAllStringSubmatch(text, -1) {
			targets = append(targets, m[1])
		}
		for _, target := range targets {
			if u, err := url.Parse(target); err != nil || u.Scheme != "" {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			path := doc
			if file != "" {
				path = filepath.Join(filepath.Dir(doc), filepath.FromSlash(file))
			}
			rel, _ := filepath.Rel(root, doc)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s links to %s, which does not exist", rel, target)
				continue
			}
			if anchor == "" || !strings.HasSuffix(path, ".md") {
				continue
			}
			if anchors[path] == nil {
				anchors[path] = headings(t, path)
			}
			if !anchors[path][anchor] {
				t.Errorf("%s links to %s, but that file has no heading #%s", rel, target, anchor)
			}
		}
	}
}

// documents lists the Markdown files that are checked.
func documents(t *testing.T) []string {
	docs := []string{filepath.Join(root, "README.md"), filepath.Join(root, "CHANGELOG.md")}
	for _, dir := range []string{"docs", "spec", "examples"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(path, ".md") {
				docs = append(docs, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return docs
}

// headings returns the anchors GitHub gives a file's headings.
func headings(t *testing.T, path string) map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	out := map[string]bool{}
	for _, m := range heading.FindAllStringSubmatch(fence.ReplaceAllString(string(data), ""), -1) {
		slug := slugify(m[1])
		if n := seen[slug]; n > 0 {
			out[slug+"-"+strconv.Itoa(n)] = true
		} else {
			out[slug] = true
		}
		seen[slug]++
	}
	return out
}

// slugify turns a heading into its anchor, as GitHub does: lower case,
// punctuation removed, spaces turned into hyphens.
func slugify(h string) string {
	h = inline.ReplaceAllString(h, "$1") // a link keeps its text
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 && isLetter(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isLetter(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
