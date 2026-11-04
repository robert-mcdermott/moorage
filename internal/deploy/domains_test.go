package deploy

import (
	"testing"
)

func TestParseDomain(t *testing.T) {
	for address, want := range map[string][2]string{
		"portal.example.com":                 {"portal.example.com", ""},
		"Portal.Example.COM":                 {"portal.example.com", ""},
		"https://portal.example.com/":        {"portal.example.com", ""},
		"tools.example.com/reports":          {"tools.example.com", "/reports"},
		"tools.example.com/reports/":         {"tools.example.com", "/reports"},
		"tools.example.com/a/b-c_d.e":        {"tools.example.com", "/a/b-c_d.e"},
		" http://tools.example.com/reports ": {"tools.example.com", "/reports"},
	} {
		host, prefix, err := ParseDomain(address)
		if err != nil || host != want[0] || prefix != want[1] {
			t.Errorf("%q: %q %q %v", address, host, prefix, err)
		}
	}
	for _, bad := range []string{
		"", "localhost", "example", "-bad.example.com", "a b.example.com", "*.example.com", "1.2.3.4", "tools.example.com/..", "tools.example.com/a/../b",
		"tools.example.com/.moorage", "tools.example.com/.hidden", "tools.example.com/a b", "tools.example.com/a?b", "tools.example.com/a%2fb", "user@example.com",
	} {
		if host, prefix, err := ParseDomain(bad); err == nil {
			t.Errorf("%q was accepted, as %q %q", bad, host, prefix)
		}
	}
}
