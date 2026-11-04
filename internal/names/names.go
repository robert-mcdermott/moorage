// Package names checks the names of apps and environments, which become
// parts of hostnames (spec/06-ingress.md, "Hostnames").
package names

import (
	"crypto/sha256"
	"encoding/hex"
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

// ValidateProject reports why name cannot be a project's name, or nil.
func ValidateProject(name string) error {
	if !appPattern.MatchString(name) {
		return fmt.Errorf("a project's name must be 3 to 40 lowercase letters, digits and hyphens, starting with a letter")
	}
	return nil
}

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

var envPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidateEnv reports why name cannot be an environment's name, or nil.
func ValidateEnv(name string) error {
	switch {
	case !envPattern.MatchString(name):
		return fmt.Errorf("environment name %q must be up to 40 lowercase letters, digits and hyphens, starting and ending with a letter or digit", name)
	case strings.Contains(name, "--"):
		return fmt.Errorf("environment name %q must not contain \"--\", which separates an app from its environment", name)
	case name == "preview":
		return fmt.Errorf("environment name %q is reserved: [environments.preview] applies to every preview", name)
	}
	return nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:6]
}

// Slug makes an environment's name of a branch's: feature/new-pricing becomes
// feature-new-pricing, and one too long is cut, with a short hash to tell it apart.
func Slug(branch string) string {
	var b strings.Builder
	dash := true // no dash to begin with
	for _, r := range strings.ToLower(branch) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	switch {
	case slug == "", slug == "preview", slug == Production:
		return "branch-" + shortHash(branch)
	case len(slug) > 40:
		return strings.TrimRight(slug[:33], "-") + "-" + shortHash(branch)
	}
	return slug
}

// WithHash is a name made different from another's, for a branch whose slug is taken.
func WithHash(slug, branch string) string {
	if len(slug) > 33 {
		slug = strings.TrimRight(slug[:33], "-")
	}
	return slug + "-" + shortHash(branch)
}

// Label is the hostname's first label of an environment: the app's name for production,
// and <app>--<env> for the others, always one DNS label, so that one wildcard record and
// one wildcard certificate cover them all.
func Label(app, env string) string {
	if env == Production || env == "" {
		return app
	}
	label := app + "--" + env
	if len(label) <= 63 {
		return label
	}
	room := 63 - len(app) - len("--") - len("-") - 6
	cut := env
	if room < len(cut) {
		cut = strings.TrimRight(cut[:room], "-")
	}
	return app + "--" + cut + "-" + shortHash(env)
}
