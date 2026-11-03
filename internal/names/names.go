// Package names checks the names of apps and environments, which become
// parts of hostnames (docs/06-ingress.md, "Hostnames").
package names

import (
	"fmt"
	"regexp"
	"strings"
)

var appPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)

// reserved are names an app cannot take, because they would collide with
// the console's hostname or platform paths.
var reserved = map[string]bool{
	"moorage": true, "console": true, "api": true, "git": true, "www": true,
	"admin": true, "status": true, "assets": true, "login": true, "auth": true,
}

// Production is the environment every app has.
const Production = "production"

// ValidateApp reports why name cannot be an app name, or nil.
func ValidateApp(name string) error {
	switch {
	case !appPattern.MatchString(name):
		return fmt.Errorf("app name %q must be 3 to 40 lowercase letters, digits and hyphens, starting with a letter and ending with a letter or digit", name)
	case strings.Contains(name, "--"):
		return fmt.Errorf("app name %q must not contain \"--\", which separates an app from its environment in hostnames", name)
	case reserved[name]:
		return fmt.Errorf("app name %q is reserved", name)
	}
	return nil
}
