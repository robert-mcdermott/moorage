package deploy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Limits of share links and of a gate's password.
const (
	DefaultShareLife = 7 * 24 * time.Hour
	MaxShareLife     = 365 * 24 * time.Hour
	MinGatePassword  = 8
	MaxGatePassword  = 200
)

// InvalidAccessError says what is wrong with a change of who may reach an environment.
type InvalidAccessError struct{ Msg string }

func (e *InvalidAccessError) Error() string { return e.Msg }

// AccessState says who may reach an environment, and why.
type AccessState struct {
	// Mode is the gate it has; Source says where that comes from: setting, manifest or server.
	Mode, Source string
	// Set is what has been set for it, and Asked what its manifest asks.
	Set   store.EnvAccess
	Asked string
}

// AskedInVain says that its manifest asks for no gate, which only a maintainer can allow.
func (a AccessState) AskedInVain() bool {
	return a.Asked == ingress.GatePublic && a.Mode != ingress.GatePublic && a.Source == "server"
}

// PasswordMissing says that its gate asks for a password nobody has set, so that nobody gets in.
func (a AccessState) PasswordMissing() bool {
	return a.Mode == ingress.GatePassword && a.Set.PasswordHash == ""
}

// Access says who may reach an environment.
func (d *Deployer) Access(ctx context.Context, app, env string) (AccessState, error) {
	var state AccessState
	e, err := d.db.Environment(ctx, app, env)
	if err != nil {
		return state, err
	}
	if state.Set, err = d.db.EnvironmentAccess(ctx, app, env); err != nil {
		return state, err
	}
	if _, b, err := d.db.CurrentRelease(ctx, app, env); err == nil {
		var plan manifest.Plan
		if json.Unmarshal(b.Plan, &plan) == nil {
			state.Asked = plan.AccessFor(env, e.Kind)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return state, err
	}
	state.Mode, state.Source = d.opts.Config.GateFor(state.Set.Mode, state.Asked, e.Kind)
	return state, nil
}

// AccessChange changes who may reach an environment. An empty Mode leaves it to the manifest
// and the server; an empty Password leaves the password as it was, and a nil Allow the list.
type AccessChange struct {
	Mode, Password string
	Allow          *[]string
	AnyUser        bool
}

// SetAccess changes who may reach an environment. It takes effect at once.
func (d *Deployer) SetAccess(ctx context.Context, app, env, by string, change AccessChange) (AccessState, error) {
	if change.Mode != "" && !slices.Contains([]string{ingress.GatePublic, ingress.GatePassword, ingress.GateLogin, ingress.GateShare}, change.Mode) {
		return AccessState{}, &InvalidAccessError{fmt.Sprintf("%q is not a gate: it can be public, password, login or share", change.Mode)}
	}
	before, err := d.db.EnvironmentAccess(ctx, app, env)
	if err != nil {
		return AccessState{}, err
	}
	next := store.EnvAccess{Mode: change.Mode, PasswordHash: before.PasswordHash, AnyUser: change.AnyUser, Allow: before.Allow}
	if change.Allow != nil {
		next.Allow = nil
		for _, entry := range *change.Allow {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			prefix, err := ingress.ParseAllow(entry)
			if err != nil {
				return AccessState{}, &InvalidAccessError{fmt.Sprintf("%q is %v", entry, err)}
			}
			next.Allow = append(next.Allow, prefix.String())
		}
	}
	if change.Password != "" {
		if n := len(change.Password); n < MinGatePassword || n > MaxGatePassword {
			return AccessState{}, &InvalidAccessError{fmt.Sprintf("a password here must be %d to %d characters long", MinGatePassword, MaxGatePassword)}
		}
		if next.PasswordHash, err = auth.HashPassword(ctx, change.Password); err != nil {
			return AccessState{}, err
		}
	}
	if change.Mode == ingress.GatePassword && next.PasswordHash == "" {
		return AccessState{}, &InvalidAccessError{"the password gate needs a password"}
	}
	if err := d.db.SetEnvironmentAccess(ctx, app, env, next); err != nil {
		return AccessState{}, err
	}
	if err := d.opts.Reload(ctx); err != nil {
		return AccessState{}, err
	}
	state, err := d.Access(ctx, app, env)
	if err != nil {
		return state, err
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.AccessChanged, App: app, Env: env, Actor: by, Data: map[string]any{"mode": state.Mode}})
	return state, nil
}

// CreateShareLink makes a link to an environment, and returns it with its address, which is
// shown only now: only the hash of its token is kept.
func (d *Deployer) CreateShareLink(ctx context.Context, app, env, by string, life time.Duration, maxUses int) (store.ShareLink, string, error) {
	if life == 0 {
		life = DefaultShareLife
	}
	if life < 0 || life > MaxShareLife {
		return store.ShareLink{}, "", &InvalidAccessError{"a share link can last up to a year"}
	}
	if maxUses < 0 {
		return store.ShareLink{}, "", &InvalidAccessError{"a share link's number of uses cannot be negative"}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return store.ShareLink{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	l := store.ShareLink{ID: ids.New(ids.ShareLink), App: app, Env: env, CreatedBy: by, CreatedAt: now, ExpiresAt: now.Add(life), MaxUses: maxUses}
	if err := d.db.CreateShareLink(ctx, app, env, l, ingress.ShareTokenHash(token)); err != nil {
		return l, "", err
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.ShareCreated, App: app, Env: env, Actor: by, Data: map[string]any{"link": l.ID}})
	return l, d.opts.URL(app, env) + "/.moorage/share/" + token, nil
}

// RevokeShareLink ends a link, and within a minute everyone it let in.
func (d *Deployer) RevokeShareLink(ctx context.Context, app, env, id, by string) error {
	if err := d.db.RevokeShareLink(ctx, app, env, id); err != nil {
		return err
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.ShareRevoked, App: app, Env: env, Actor: by, Data: map[string]any{"link": id}})
	return nil
}
