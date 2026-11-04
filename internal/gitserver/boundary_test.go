package gitserver

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyThisPackageUsesTheGitLibrary keeps the git library, whose major
// version is in beta, behind this package (spec/12-decisions.md, D4).
func TestOnlyThisPackageUsesTheGitLibrary(t *testing.T) {
	here, _ := filepath.Abs(".")
	for _, dir := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if abs, _ := filepath.Abs(filepath.Dir(path)); abs == here {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(p, "github.com/go-git/") {
					t.Errorf("%s imports %s: use internal/gitserver instead", path, p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
