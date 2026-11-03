package names

import "testing"

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
