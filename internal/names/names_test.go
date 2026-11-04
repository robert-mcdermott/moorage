package names

import (
	"strings"
	"testing"
)

func TestValidateApp(t *testing.T) {
	for _, ok := range []string{"docs", "acme-portal", "a1b", "xyz", "abcdefghijklmnopqrstuvwxyz0123456789-abc"} {
		if err := ValidateApp(ok); err != nil {
			t.Errorf("ValidateApp(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", "Docs", "1docs", "docs-", "-docs", "my--app", "my_app", "my.app", "moorage", "api",
		"abcdefghijklmnopqrstuvwxyz0123456789-abcd"} {
		if err := ValidateApp(bad); err == nil {
			t.Errorf("ValidateApp(%q) accepted", bad)
		}
	}
}

func TestValidateEnv(t *testing.T) {
	for _, good := range []string{"staging", "pr-12", "a", "feature-new-pricing"} {
		if err := ValidateEnv(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{"", "Staging", "-a", "a-", "a--b", "preview", "a_b", "a.b", strings.Repeat("a", 41)} {
		if ValidateEnv(bad) == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestSlug(t *testing.T) {
	for branch, want := range map[string]string{
		"feature/new-pricing": "feature-new-pricing",
		"Fix_The--Bug":        "fix-the-bug",
		"-odd-":               "odd",
	} {
		if got := Slug(branch); got != want {
			t.Errorf("%q: %q, want %q", branch, got, want)
		}
	}
	long := strings.Repeat("a", 60)
	if got := Slug(long); len(got) > 40 || got == Slug(long+"b") || ValidateEnv(got) != nil {
		t.Errorf("a long branch: %q", got)
	}
	for _, reserved := range []string{"preview", "production", "///"} {
		if got := Slug(reserved); ValidateEnv(got) != nil || got == reserved {
			t.Errorf("%q: %q", reserved, got)
		}
	}
	if got := WithHash("feature", "feature/x"); ValidateEnv(got) != nil || got == "feature" {
		t.Errorf("with a hash: %q", got)
	}
}

func TestLabel(t *testing.T) {
	if got := Label("shop", Production); got != "shop" {
		t.Errorf("production: %q", got)
	}
	if got := Label("shop", "staging"); got != "shop--staging" {
		t.Errorf("staging: %q", got)
	}
	app, env := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if got := Label(app, env); len(got) > 63 || !strings.HasPrefix(got, app+"--") || got == Label(app, strings.Repeat("b", 39)+"c") {
		t.Errorf("a long one: %q (%d)", got, len(got))
	}
}

func TestValidHostLabel(t *testing.T) {
	for label, want := range map[string]bool{
		"docs": true, "docs--staging": true, "docs--production--v12": true, "docs--pr-7--v1": true,
		"a--b--c--d": false, "docs--production--v01": false, "docs--production--x": false, "--v1": false,
		"docs--": false, "": false, "Docs": false, "docs--ENV": false,
	} {
		if got := ValidHostLabel(label); got != want {
			t.Errorf("%q: %v", label, got)
		}
	}
}
