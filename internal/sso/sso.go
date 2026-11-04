// Package sso signs people in through an OpenID Connect provider (spec/08-security.md): with an
// account made the first time if the server says so, and roles from their groups.
package sso

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// CallbackPath is where a provider sends people back to, which it has to be told.
const CallbackPath = "/login/sso/callback"

// CookieName is what a browser holds while it is away at the provider.
const CookieName = "moorage_sso"

// flowLifetime is how long someone has at the provider.
const flowLifetime = 10 * time.Minute

// Keyring seals the client's secret.
type Keyring interface {
	SealSSO(value string) []byte
	OpenSSO(sealed []byte) (string, error)
}

// RefusedError says why someone may not sign in, in words for them.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// InvalidError says what is wrong with a configuration.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Service signs people in through a provider.
type Service struct {
	DB         *store.DB
	Auth       *auth.Service
	Keyring    Keyring
	Log        *slog.Logger
	ConsoleURL string
	// StateKey signs what a browser holds while it is away.
	StateKey []byte

	mu       sync.Mutex
	provider *oidc.Provider
	issuer   string
}

// RedirectURL is where the provider sends people back to.
func (s *Service) RedirectURL() string { return s.ConsoleURL + CallbackPath }

func (s *Service) client() *http.Client { return &http.Client{Timeout: 15 * time.Second} }

func (s *Service) providerFor(ctx context.Context, issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil && s.issuer == issuer {
		return s.provider, nil
	}
	// It outlives this request: what it fetches of the provider's keys is kept.
	found, err := oidc.NewProvider(oidc.ClientContext(context.WithoutCancel(ctx), s.client()), issuer)
	if err != nil {
		return nil, err
	}
	s.provider, s.issuer = found, issuer
	return found, nil
}

// Button says what the sign-in page offers, if anything, and whether it offers passwords as well.
func (s *Service) Button(ctx context.Context) (label string, passwords bool) {
	cfg, err := s.DB.SSOConfig(ctx)
	if err != nil || !cfg.Configured() {
		return "", true
	}
	label = cfg.Label
	if label == "" {
		label = "single sign-on"
	}
	return label, !cfg.PasswordsOff
}

// PasswordsOff says that signing in is left to the provider.
func (s *Service) PasswordsOff(ctx context.Context) bool {
	cfg, err := s.DB.SSOConfig(ctx)
	return err == nil && cfg.Configured() && cfg.PasswordsOff
}

type flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

func (s *Service) seal(f flow) (string, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.StateKey)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Service) open(value string) (flow, bool) {
	var f flow
	encoded, sum, ok := strings.Cut(value, ".")
	if !ok || len(s.StateKey) == 0 {
		return f, false
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return f, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sum)
	if err != nil {
		return f, false
	}
	mac := hmac.New(sha256.New, s.StateKey)
	mac.Write(data)
	if !hmac.Equal(got, mac.Sum(nil)) || json.Unmarshal(data, &f) != nil || time.Now().Unix() > f.Expires {
		return f, false
	}
	return f, true
}

func random() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Service) oauth(ctx context.Context, cfg store.SSOConfig) (*oidc.Provider, *oauth2.Config, error) {
	provider, err := s.providerFor(ctx, cfg.Issuer)
	if err != nil {
		return nil, nil, err
	}
	secret := ""
	if len(cfg.SealedSecret) > 0 {
		if secret, err = s.Keyring.OpenSSO(cfg.SealedSecret); err != nil {
			return nil, nil, err
		}
	}
	scopes := []string{oidc.ScopeOpenID, "email", "profile"}
	scopes = append(scopes, strings.Fields(cfg.Scopes)...)
	return provider, &oauth2.Config{ClientID: cfg.ClientID, ClientSecret: secret, Endpoint: provider.Endpoint(), RedirectURL: s.RedirectURL(), Scopes: scopes}, nil
}

// Begin says where to send a browser, and what it holds while it is there.
func (s *Service) Begin(ctx context.Context, next string) (string, string, error) {
	cfg, err := s.DB.SSOConfig(ctx)
	if err != nil {
		return "", "", err
	}
	if !cfg.Configured() {
		return "", "", &RefusedError{"Single sign-on is not set up on this server."}
	}
	_, conf, err := s.oauth(ctx, cfg)
	if err != nil {
		return "", "", fmt.Errorf("reach the provider: %w", err)
	}
	f := flow{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Next: next, Expires: time.Now().Add(flowLifetime).Unix()}
	held, err := s.seal(f)
	if err != nil {
		return "", "", err
	}
	return conf.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), held, nil
}

type claims struct {
	Email    string `json:"email"`
	Verified *bool  `json:"email_verified"`
	Name     string `json:"name"`
}

// Finish takes what the provider sent a browser back with, and says who they are and where they were
// going. held is what Begin gave the browser.
func (s *Service) Finish(ctx context.Context, query url.Values, held, ip string) (store.User, string, error) {
	f, ok := s.open(held)
	if !ok {
		return store.User{}, "", &RefusedError{"That took too long, or it did not start here. Try signing in again."}
	}
	if !hmac.Equal([]byte(query.Get("state")), []byte(f.State)) {
		return store.User{}, "", &RefusedError{"That sign-in did not start in this browser. Try again."}
	}
	if problem := query.Get("error"); problem != "" {
		s.Auth.Security("a provider refused a sign-in", "ip", ip, "error", problem)
		return store.User{}, f.Next, &RefusedError{"Your provider did not sign you in: " + problem + "."}
	}
	cfg, err := s.DB.SSOConfig(ctx)
	if err != nil {
		return store.User{}, f.Next, err
	}
	if !cfg.Configured() {
		return store.User{}, f.Next, &RefusedError{"Single sign-on is not set up on this server."}
	}
	provider, conf, err := s.oauth(ctx, cfg)
	if err != nil {
		return store.User{}, f.Next, fmt.Errorf("reach the provider: %w", err)
	}
	exchangeCtx := oidc.ClientContext(ctx, s.client())
	token, err := conf.Exchange(exchangeCtx, query.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		s.Auth.Security("a provider's code could not be exchanged", "ip", ip, "err", err)
		return store.User{}, f.Next, &RefusedError{"Your provider did not confirm that sign-in. Try again."}
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return store.User{}, f.Next, &RefusedError{"Your provider did not say who you are."}
	}
	id, err := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}).Verify(exchangeCtx, raw)
	if err != nil {
		s.Auth.Security("a provider's token was refused", "ip", ip, "err", err)
		return store.User{}, f.Next, &RefusedError{"What your provider sent could not be trusted. Try again."}
	}
	if !hmac.Equal([]byte(id.Nonce), []byte(f.Nonce)) {
		s.Auth.Security("a provider's token was for another sign-in", "ip", ip)
		return store.User{}, f.Next, &RefusedError{"What your provider sent was for another sign-in. Try again."}
	}
	var who claims
	if err := id.Claims(&who); err != nil {
		return store.User{}, f.Next, err
	}
	var all map[string]any
	if err := id.Claims(&all); err != nil {
		return store.User{}, f.Next, err
	}
	u, err := s.account(ctx, cfg, id.Issuer, id.Subject, who, ip)
	if err != nil {
		s.Auth.Audit(ctx, store.AuditEntry{Actor: who.Email, IP: ip, Action: "signin.sso", Result: "failed"})
		return store.User{}, f.Next, err
	}
	if err := s.applyGroups(ctx, cfg, u, groupsOf(all, cfg.GroupClaim)); err != nil {
		s.Log.Warn("apply someone's groups", "email", u.Email, "err", err)
	}
	if u, err = s.DB.UserByID(ctx, u.ID); err != nil {
		return store.User{}, f.Next, err
	}
	s.Auth.Audit(ctx, store.AuditEntry{Actor: u.Email, IP: ip, Action: "signin.sso", Result: "ok"})
	return u, f.Next, nil
}

// account finds who someone is, making them an account if the server says so. A provider's subject is
// the one thing that stays the same. An address joins someone to an account once, and only if the
// provider vouches for it: otherwise, whoever can have a provider say an address could be anyone.
func (s *Service) account(ctx context.Context, cfg store.SSOConfig, issuer, subject string, who claims, ip string) (store.User, error) {
	u, err := s.DB.UserByIdentity(ctx, issuer, subject)
	if err == nil {
		if u.Disabled() {
			return u, &RefusedError{"This account is disabled."}
		}
		return u, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return u, err
	}
	email, err := auth.NormalizeEmail(who.Email)
	if err != nil {
		return store.User{}, &RefusedError{"Your provider did not give an email address. It may need the email scope."}
	}
	if who.Verified != nil && !*who.Verified {
		return store.User{}, &RefusedError{"Your provider has not verified your email address."}
	}
	if !domainAllowed(cfg.EmailDomains, email) {
		s.Auth.Security("a sign-in from an address that is not allowed", "ip", ip, "email", email)
		return store.User{}, &RefusedError{"Your address is not one that may use this server."}
	}
	u, err = s.DB.UserByEmail(ctx, email)
	switch {
	case err == nil:
		if u.Disabled() {
			return u, &RefusedError{"This account is disabled."}
		}
	case !errors.Is(err, store.ErrNotFound):
		return u, err
	case !cfg.Provision:
		return store.User{}, &RefusedError{"You have no account on this server. Ask an administrator for one."}
	default:
		if u, err = s.Auth.NewUser(ctx, email, who.Name, store.RoleMember, ""); err != nil {
			return u, err
		}
		if err := s.DB.CreateUser(ctx, u); err != nil {
			return u, err
		}
		s.Auth.Audit(ctx, store.AuditEntry{Actor: email, IP: ip, Action: "user.provision", Target: u.ID, Result: "ok"})
	}
	if err := s.DB.LinkIdentity(ctx, issuer, subject, u.ID); err != nil {
		return u, err
	}
	return u, nil
}

func domainAllowed(domains, email string) bool {
	list := strings.Fields(strings.ToLower(domains))
	if len(list) == 0 {
		return true
	}
	_, domain, ok := strings.Cut(email, "@")
	return ok && slices.Contains(list, strings.ToLower(domain))
}

func groupsOf(all map[string]any, claim string) []string {
	if claim == "" {
		claim = "groups"
	}
	switch v := all[claim].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, g := range v {
			if s, ok := g.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{v}
	}
	return nil
}

// Rule says what being in a group means.
type Rule struct {
	Group string
	// Admin makes someone an administrator of the server. Otherwise Role is their role in Project.
	Admin         bool
	Project, Role string
}

// ParseRules reads rules, one per line: "ops = admin", or "web-team = project web maintainer".
func ParseRules(text string) ([]Rule, error) {
	var rules []Rule
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		group, meaning, ok := strings.Cut(line, "=")
		fields := strings.Fields(meaning)
		group = strings.TrimSpace(group)
		switch {
		case ok && group != "" && len(fields) == 1 && fields[0] == "admin":
			rules = append(rules, Rule{Group: group, Admin: true})
		case ok && group != "" && len(fields) == 3 && fields[0] == "project" && store.ValidRole(fields[2]):
			rules = append(rules, Rule{Group: group, Project: fields[1], Role: fields[2]})
		default:
			return nil, &InvalidError{fmt.Sprintf(`Line %d of the group rules should be "GROUP = admin" or "GROUP = project NAME viewer", deployer or maintainer.`, i+1)}
		}
	}
	return rules, nil
}

var roleRank = map[string]int{store.RoleViewer: 1, store.RoleDeployer: 2, store.RoleMaintainer: 3}

// applyGroups gives someone what their groups say, every time they sign in, and takes away what they
// no longer say. What the rules do not mention is left as it is, for an administrator to set by hand.
func (s *Service) applyGroups(ctx context.Context, cfg store.SSOConfig, u store.User, groups []string) error {
	rules, err := ParseRules(cfg.GroupRules)
	if err != nil || len(rules) == 0 {
		return err
	}
	adminMentioned, admin := false, false
	wanted := map[string]string{}
	for _, r := range rules {
		has := slices.Contains(groups, r.Group)
		if r.Admin {
			adminMentioned = true
			admin = admin || has
			continue
		}
		if _, ok := wanted[r.Project]; !ok {
			wanted[r.Project] = ""
		}
		if has && roleRank[r.Role] > roleRank[wanted[r.Project]] {
			wanted[r.Project] = r.Role
		}
	}
	var failed error
	if adminMentioned {
		role := store.RoleMember
		if admin {
			role = store.RoleAdmin
		}
		if u.Role != role {
			if err := s.DB.SetUserRole(ctx, u.ID, role); err != nil && !errors.Is(err, store.ErrLastAdmin) {
				failed = errors.Join(failed, err)
			}
		}
	}
	for project, role := range wanted {
		var err error
		if role == "" {
			err = s.DB.RemoveMember(ctx, project, u.ID)
		} else {
			err = s.DB.SetMember(ctx, project, u.ID, role)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			failed = errors.Join(failed, fmt.Errorf("project %s: %w", project, err))
		}
	}
	return failed
}

// Form is what someone asks of single sign-on. An empty secret keeps the one there is.
type Form struct {
	Issuer, ClientID, ClientSecret, Label, Scopes, EmailDomains string
	Provision                                                   bool
	GroupClaim, GroupRules                                      string
	PasswordsOff                                                bool
}

// issuerAllowed says whether a provider's address can be trusted with signing people in.
func issuerAllowed(issuer string) bool {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return u.Scheme == "http" && ip.IsLoopback()
	}
	return u.Scheme == "http" && (host == "localhost" || strings.HasSuffix(host, ".localhost"))
}

// Configure keeps what someone asks, once the provider has been found at the address it gives.
func (s *Service) Configure(ctx context.Context, form Form, by string) error {
	form.Issuer = strings.TrimRight(strings.TrimSpace(form.Issuer), "/")
	form.ClientID = strings.TrimSpace(form.ClientID)
	if form.Issuer == "" || form.ClientID == "" {
		return &InvalidError{"It takes the provider's address and a client ID."}
	}
	if !issuerAllowed(form.Issuer) {
		return &InvalidError{"The provider's address has to be https, as signing in depends on it."}
	}
	if _, err := ParseRules(form.GroupRules); err != nil {
		return err
	}
	old, err := s.DB.SSOConfig(ctx)
	if err != nil {
		return err
	}
	if _, err := s.freshProvider(ctx, form.Issuer); err != nil {
		return &InvalidError{"The provider cannot be reached at that address: " + err.Error()}
	}
	cfg := store.SSOConfig{Issuer: form.Issuer, ClientID: form.ClientID, SealedSecret: old.SealedSecret, Label: strings.TrimSpace(form.Label), Scopes: strings.TrimSpace(form.Scopes),
		EmailDomains: strings.TrimSpace(form.EmailDomains), Provision: form.Provision, GroupClaim: strings.TrimSpace(form.GroupClaim), GroupRules: form.GroupRules, PasswordsOff: form.PasswordsOff, UpdatedBy: by, UpdatedAt: time.Now().UTC()}
	if form.ClientSecret != "" {
		cfg.SealedSecret = s.Keyring.SealSSO(form.ClientSecret)
	}
	return s.DB.SetSSOConfig(ctx, cfg)
}

func (s *Service) freshProvider(ctx context.Context, issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	s.provider, s.issuer = nil, ""
	s.mu.Unlock()
	return s.providerFor(ctx, issuer)
}

// TurnOff ends single sign-on, and gives passwords back.
func (s *Service) TurnOff(ctx context.Context) error {
	s.mu.Lock()
	s.provider, s.issuer = nil, ""
	s.mu.Unlock()
	return s.DB.DeleteSSOConfig(ctx)
}
