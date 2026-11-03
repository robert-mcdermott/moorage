package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// MaxVarValue bounds the size of a variable's value.
const MaxVarValue = 64 << 10

var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateVarName checks the name of a variable set on the server: a valid
// environment variable name that Moorage does not set itself.
func ValidateVarName(name string) error {
	switch {
	case !varName.MatchString(name):
		return fmt.Errorf("%q is not a valid variable name (letters, digits and _, not starting with a digit)", name)
	case name == "PORT" || name == "HOST" || strings.HasPrefix(name, "MOORAGE_"):
		return fmt.Errorf("%s is set by Moorage and cannot be changed", name)
	}
	return nil
}

// VarValue is a variable to set, its value in the clear.
type VarValue struct {
	Name   string
	Value  string
	Secret bool
	Build  bool
}

// ConfigChange is a change to an environment's variables: those of the
// environment, or with AllEnvs, those of every environment of the app.
type ConfigChange struct {
	Set     []VarValue
	Unset   []string
	AllEnvs bool
	// Restart releases the change at once, when the environment has a
	// release; otherwise the next deployment picks it up.
	Restart bool
	By      string
}

// InvalidConfigError reports a change that cannot be applied.
type InvalidConfigError struct{ Err error }

func (e *InvalidConfigError) Error() string { return e.Err.Error() }
func (e *InvalidConfigError) Unwrap() error { return e.Err }

// ConfigResult says what a configuration change did.
type ConfigResult struct {
	// AppCreated reports an app created by the change (variables can be set
	// before the first deploy).
	AppCreated bool
	// NotSet lists the names to unset that were not set.
	NotSet []string
	// Release and Deployment are the release made with the change, if any.
	Release    *store.Release
	Deployment store.Deployment
}

// Configure changes an environment's variables. Every change that is
// released makes a new release of the current build, rolled out without
// downtime like a deploy; the release command does not run again.
func (d *Deployer) Configure(ctx context.Context, app, env string, ch ConfigChange) (ConfigResult, error) {
	var res ConfigResult
	if len(ch.Set) == 0 && len(ch.Unset) == 0 {
		return res, &InvalidConfigError{errors.New("nothing to change: give variables to set or unset")}
	}
	seen := map[string]bool{}
	for _, v := range ch.Set {
		if err := ValidateVarName(v.Name); err != nil {
			return res, &InvalidConfigError{err}
		}
		if len(v.Value) > MaxVarValue {
			return res, &InvalidConfigError{fmt.Errorf("the value of %s is longer than %d KiB", v.Name, MaxVarValue>>10)}
		}
		if seen[v.Name] {
			return res, &InvalidConfigError{fmt.Errorf("%s is given twice", v.Name)}
		}
		seen[v.Name] = true
	}
	for _, name := range ch.Unset {
		if seen[name] {
			return res, &InvalidConfigError{fmt.Errorf("%s is both set and unset", name)}
		}
	}
	_, created, err := d.db.EnsureApp(ctx, app)
	if err != nil {
		return res, err
	}
	res.AppCreated = created
	appID, envID, err := d.db.Scope(ctx, app, env)
	if err != nil {
		return res, err
	}
	scope := envID
	if ch.AllEnvs {
		scope = ""
	}
	change := store.VarChange{Unset: ch.Unset, AllEnvs: ch.AllEnvs, By: ch.By}
	for _, v := range ch.Set {
		sv := store.Var{Name: v.Name, Secret: v.Secret, Build: v.Build}
		if v.Secret {
			if d.opts.Secrets == nil {
				return res, errors.New("this server cannot encrypt secrets")
			}
			if sv.Sealed, err = d.opts.Secrets.Seal(ctx, appID, scope, v.Name, v.Value); err != nil {
				return res, err
			}
		} else {
			sv.Value = v.Value
		}
		change.Set = append(change.Set, sv)
	}
	if res.NotSet, err = d.db.ChangeVars(ctx, app, env, change); err != nil {
		return res, err
	}
	d.log.Info("variables changed", "app", app, "env", env, "set", varNames(ch.Set), "unset", ch.Unset, "all_envs", ch.AllEnvs)
	if !ch.Restart {
		return res, nil
	}
	_, current, err := d.db.CurrentRelease(ctx, app, env)
	if errors.Is(err, store.ErrNotFound) {
		return res, nil // the first deployment will run with them
	}
	if err != nil {
		return res, err
	}
	var plan manifest.Plan
	if err := json.Unmarshal(current.Plan, &plan); err != nil {
		return res, err
	}
	rel, dep, err := d.Publish(ctx, PublishRequest{
		App: app, Env: env, By: ch.By, Source: "config", Reason: "config", Build: current, Plan: plan,
	})
	res.Deployment = dep
	if err != nil {
		return res, err
	}
	res.Release = &rel
	return res, nil
}

func varNames(vs []VarValue) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Name
	}
	return out
}
