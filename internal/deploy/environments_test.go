package deploy

import (
	"testing"

	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func TestBranchMatches(t *testing.T) {
	for _, c := range []struct {
		globs  []string
		branch string
		want   bool
	}{
		{nil, "anything", true},
		{[]string{"*"}, "topic", true},
		{[]string{"feature/*"}, "feature/a", true},
		{[]string{"feature/*"}, "topic", false},
		{[]string{"*", "!dependabot/*"}, "topic", true},
		{[]string{"*", "!dependabot/*", "dependabot/*"}, "dependabot/up", false},
		{[]string{"!wip"}, "wip", false},
		{[]string{"!wip"}, "other", true},
		{[]string{"*"}, "feature/a/b", true},
		{[]string{"feature/*"}, "feature/a/b", true},
		{[]string{"v?"}, "v1", true},
		{[]string{"v?"}, "v12", false},
		{[]string{"*-wip"}, "topic-wip", true},
		{[]string{"*-wip"}, "topic-wip-2", false},
		{[]string{"a*b*c"}, "axbxbxc", true},
	} {
		if got := branchMatches(c.globs, c.branch); got != c.want {
			t.Errorf("%v of %q: %v", c.globs, c.branch, got)
		}
	}
}

func TestBranchTarget(t *testing.T) {
	off := false
	declared := &manifest.Manifest{Environments: map[string]manifest.Environment{"staging": {Branch: "develop"}, "preview": {Branch: "not-this"}}}
	if env, kind, skip := branchTarget(declared, "develop"); env != "staging" || kind != store.KindNamed || skip != "" {
		t.Errorf("a declared branch: %q %q %q", env, kind, skip)
	}
	if _, kind, skip := branchTarget(declared, "not-this"); kind != store.KindPreview || skip != "" {
		t.Errorf("[environments.preview] gives no branch: %q %q", kind, skip)
	}
	if _, kind, skip := branchTarget(nil, "topic"); kind != store.KindPreview || skip != "" {
		t.Errorf("no manifest: %q %q", kind, skip)
	}
	if _, _, skip := branchTarget(&manifest.Manifest{Previews: &manifest.Previews{Enabled: &off}}, "topic"); skip == "" {
		t.Errorf("previews turned off")
	}
	if _, _, skip := branchTarget(&manifest.Manifest{Previews: &manifest.Previews{Branches: []string{"feature/*"}}}, "topic"); skip == "" {
		t.Errorf("a branch left out")
	}
}
