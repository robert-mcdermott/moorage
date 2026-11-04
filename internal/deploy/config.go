package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
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
	// ProductionOnly keeps previews from inheriting it.
	ProductionOnly bool
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

// ListVars lists an environment's variables: those set on the server and the manifest's
// [env] of the current release, each marked overridden when a later source sets the same
// name. The values of secrets are never in it.
func ListVars(ctx context.Context, db *store.DB, app, env string) ([]apitypes.Var, error) {
	set, err := db.Vars(ctx, app, env)
	if err != nil {
		return nil, err
	}
	out := []apitypes.Var{}
	envNames := map[string]bool{}
	serverNames := map[string]bool{}
	for _, v := range set {
		if !v.AllEnvs {
			envNames[v.Name] = true
		}
		serverNames[v.Name] = true
	}
	for _, v := range set {
		doc := apitypes.Var{Name: v.Name, Secret: v.Secret, Build: v.Build, Source: "env", UpdatedBy: v.UpdatedBy, UpdatedAt: v.UpdatedAt}
		if !v.Secret {
			doc.Value = v.Value
		}
		if v.AllEnvs {
			doc.Source = "app"
			doc.Overridden = envNames[v.Name]
		}
		out = append(out, doc)
	}
	kind, err := db.EnvKind(ctx, app, env)
	if err != nil {
		return nil, err
	}
	if kind == store.KindPreview {
		inherited, err := db.Vars(ctx, app, names.Production)
		if err != nil {
			return nil, err
		}
		for _, v := range inherited {
			if v.AllEnvs || v.ProductionOnly {
				continue
			}
			doc := apitypes.Var{Name: v.Name, Secret: v.Secret, Build: v.Build, Source: "production", UpdatedBy: v.UpdatedBy, UpdatedAt: v.UpdatedAt, Overridden: envNames[v.Name]}
			if !v.Secret {
				doc.Value = v.Value
			}
			out = append(out, doc)
			serverNames[v.Name] = true
		}
	}
	if _, build, err := db.CurrentRelease(ctx, app, env); err == nil {
		var plan manifest.Plan
		if json.Unmarshal(build.Plan, &plan) == nil {
			running := plan.ForEnv(env, kind).Env
			for name, value := range running {
				out = append(out, apitypes.Var{Name: name, Value: value, Source: "manifest", Overridden: serverNames[name]})
			}
			for name, v := range plan.Vars {
				_, inRepo := running[name]
				if serverNames[name] || inRepo {
					for i := range out {
						if out[i].Name == name {
							out[i].Declared, out[i].Description = true, v.Description
						}
					}
					continue
				}
				out = append(out, apitypes.Var{Name: name, Source: "declared", Secret: v.Secret, Declared: true, Description: v.Description, Missing: v.Required})
			}
		}
	}
	order := map[string]int{"env": 0, "production": 1, "app": 2, "manifest": 3, "declared": 4}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return order[out[i].Source] < order[out[j].Source]
	})
	return out, nil
}

// UnreleasedVars says how an environment's variables differ from those its current
// release runs with. Nothing differs for an environment with no release.
func UnreleasedVars(ctx context.Context, db *store.DB, app, env string) ([]apitypes.VarDiff, error) {
	rel, _, err := db.CurrentRelease(ctx, app, env)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now, err := db.Config(ctx, app, env)
	if err != nil {
		return nil, err
	}
	return diffVars(rel.Config, now), nil
}

// ReleaseVars makes a release of the current build with the variables as they are now, for
// changes saved without being released.
func (d *Deployer) ReleaseVars(ctx context.Context, app, env, by string) (store.Release, store.Deployment, error) {
	_, current, err := d.db.CurrentRelease(ctx, app, env)
	if err != nil {
		return store.Release{}, store.Deployment{}, err
	}
	var plan manifest.Plan
	if err := json.Unmarshal(current.Plan, &plan); err != nil {
		return store.Release{}, store.Deployment{}, err
	}
	return d.Publish(ctx, PublishRequest{App: app, Env: env, By: by, Source: "config", Reason: "config", Build: current, Plan: plan})
}

// NotSecretError says that a variable has nothing sealed to reveal.
type NotSecretError struct{ Name string }

func (e *NotSecretError) Error() string {
	return e.Name + " is not a secret: its value is already shown."
}

// RevealVar reads the value of a secret set for an environment, or for all of them.
func (d *Deployer) RevealVar(ctx context.Context, app, env, name string) (string, error) {
	vars, err := d.db.Config(ctx, app, env)
	if err != nil {
		return "", err
	}
	for _, v := range vars {
		if v.Name != name {
			continue
		}
		if !v.Secret {
			return "", &NotSecretError{name}
		}
		appID, _, err := d.db.Scope(ctx, app, env)
		if err != nil {
			return "", err
		}
		if d.opts.Secrets == nil {
			return "", errors.New("this server cannot open secrets")
		}
		return d.opts.Secrets.Open(ctx, appID, v.Scope, v.Name, v.Sealed)
	}
	return "", fmt.Errorf("no variable %s: %w", name, store.ErrNotFound)
}

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
		sv := store.Var{Name: v.Name, Secret: v.Secret, Build: v.Build, ProductionOnly: v.ProductionOnly}
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
	d.opts.Events.Publish(ctx, store.Event{Type: events.ConfigChanged, App: app, Env: env, Actor: ch.By,
		Data: map[string]any{"set": varNames(ch.Set), "unset": ch.Unset}}) // names only: never their values
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
