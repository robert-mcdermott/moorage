// Package serverconfig writes out what a server keeps of its own settings, and brings it in again, on that
// server or another. It adds and changes, and removes nothing. It holds no secrets: where one belongs, it
// names a variable of the environment.
package serverconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/sso"
	"github.com/robert-mcdermott/moorage/internal/store"
	"github.com/robert-mcdermott/moorage/internal/telemetry"
)

// Format is the version of what is written.
const Format = 1

// InvalidError says that a configuration cannot be brought in at all.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Keys opens and seals the secrets that settings hold.
type Keys interface {
	SealRegistry(host, password string) []byte
	OpenRegistry(host string, sealed []byte) (string, error)
	OpenBackup(sealed []byte) (string, error)
	OpenSSO(sealed []byte) (string, error)
}

// Service reads and changes a server's settings.
type Service struct {
	DB       *store.DB
	Auth     *auth.Service
	Keyring  Keys
	Notifier *notify.Service
	Sinks    *telemetry.Service
	Backups  *backup.Service
	SSO      *sso.Service
}

var placeholder = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// variable names the variable of the environment that a secret is kept in.
func variable(parts ...string) string {
	var b strings.Builder
	b.WriteString("MOORAGE")
	for _, part := range parts {
		b.WriteByte('_')
		for _, r := range strings.ToUpper(part) {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
	}
	return "${" + b.String() + "}"
}

// actual says what a secret's value is, if it is not just the name of a variable that was not set.
func actual(value string) (string, bool) {
	if value == "" || placeholder.MatchString(value) {
		return "", false
	}
	return value, true
}

func missing(value string) string {
	if m := placeholder.FindStringSubmatch(value); m != nil {
		return "set " + m[1] + " and bring this in again"
	}
	return "it has no secret here, and needs one"
}

// Expand fills in the secrets that the environment holds. Those it does not hold stay as they are, which the
// server takes to mean that it should keep what it has. Only variables named for this count: a file from
// someone else could otherwise ask for any secret in the environment, and send it wherever it likes.
func Expand(f *apitypes.ServerConfig, getenv func(string) string) {
	fill := func(value *string) {
		if m := placeholder.FindStringSubmatch(*value); m != nil && strings.HasPrefix(m[1], "MOORAGE_") {
			if got := getenv(m[1]); got != "" {
				*value = got
			}
		}
	}
	for i := range f.Registries {
		fill(&f.Registries[i].Password)
	}
	for i := range f.Sinks {
		for name, value := range f.Sinks[i].Headers {
			fill(&value)
			f.Sinks[i].Headers[name] = value
		}
	}
	for i := range f.Channels {
		fill(&f.Channels[i].URL)
		fill(&f.Channels[i].Secret)
		fill(&f.Channels[i].Password)
	}
	if f.Backups != nil {
		fill(&f.Backups.SecretKey)
	}
	if f.SignIn != nil {
		fill(&f.SignIn.ClientSecret)
	}
}

// Parse reads the file that Encode writes, and refuses a key that it does not know: a mistyped setting that
// was quietly ignored would be worse.
func Parse(data []byte) (apitypes.ServerConfig, error) {
	var f apitypes.ServerConfig
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return f, fmt.Errorf("this is not a server's configuration:\n%s", strict.String())
		}
		var syntax *toml.DecodeError
		if errors.As(err, &syntax) {
			return f, fmt.Errorf("this is not a server's configuration:\n%s", syntax.String())
		}
		return f, err
	}
	return f, nil
}

// Encode writes a configuration to keep or to read.
func Encode(f apitypes.ServerConfig, about string) ([]byte, error) {
	body, err := toml.Marshal(f)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Moorage server configuration%s.\n", about)
	out.WriteString(`#
# Bring this in with: moor server config import THIS-FILE --dry-run
# That adds and changes, and removes nothing. It holds no secrets: ${NAME} is read from the environment of the
# command that brings it in, and if that is not set, a server keeps the secret that it has. People's passwords
# and second factors do not go with them: they get a link from moor server users invite, or sign in with your
# provider. server.toml is apart from this: it is a file already, and is for you to copy.

`)
	out.Write(body)
	return out.Bytes(), nil
}

// Export reads what a server has.
func (s *Service) Export(ctx context.Context) (apitypes.ServerConfig, error) {
	f := apitypes.ServerConfig{Format: Format}
	for after := ""; ; {
		users, err := s.DB.Users(ctx, after, 500)
		if err != nil {
			return f, err
		}
		if len(users) == 0 {
			break
		}
		for _, u := range users {
			f.Users = append(f.Users, apitypes.ConfigUser{Email: u.Email, Name: u.Name, Role: u.Role, Disabled: u.Disabled()})
			after = u.Email
		}
	}
	sort.Slice(f.Users, func(i, j int) bool { return f.Users[i].Email < f.Users[j].Email })

	projects, err := s.DB.Projects(ctx)
	if err != nil {
		return f, err
	}
	for _, p := range projects {
		out := apitypes.ConfigProject{Name: p.Name, EveryoneRole: &p.EveryoneRole, ViewersSeeLogs: &p.ViewersSeeLogs}
		members, err := s.DB.Members(ctx, p.Name)
		if err != nil {
			return f, err
		}
		for _, m := range members {
			if out.Members == nil {
				out.Members = map[string]string{}
			}
			out.Members[m.Email] = m.Role
		}
		f.Projects = append(f.Projects, out)
	}

	registries, err := s.DB.Registries(ctx)
	if err != nil {
		return f, err
	}
	for _, r := range registries {
		f.Registries = append(f.Registries, apitypes.ConfigRegistry{Host: r.Host, Username: r.Username, Password: variable("registry", r.Host, "password")})
	}

	channels, err := s.DB.NotificationChannels(ctx)
	if err != nil {
		return f, err
	}
	for _, c := range channels {
		cfg, err := s.Notifier.ChannelConfig(c)
		if err != nil {
			return f, fmt.Errorf("channel %s: %w", c.Name, err)
		}
		out := apitypes.ConfigChannel{Name: c.Name, Kind: c.Kind, Host: cfg.Host, Port: cfg.Port, Username: cfg.Username, From: cfg.From, To: cfg.To}
		if cfg.URL != "" {
			out.URL = variable("channel", c.Name, "url")
		}
		if cfg.Secret != "" {
			out.Secret = variable("channel", c.Name, "secret")
		}
		if cfg.Password != "" {
			out.Password = variable("channel", c.Name, "password")
		}
		f.Channels = append(f.Channels, out)
	}
	sinks, err := s.DB.TelemetrySinks(ctx)
	if err != nil {
		return f, err
	}
	for _, k := range sinks {
		out := apitypes.ConfigSink{Name: k.Name, Kind: k.Kind, Address: k.Address, Logs: k.Logs, Metrics: k.Metrics, App: k.App}
		if s.Sinks != nil {
			headers, err := s.Sinks.Headers(k)
			if err != nil {
				return f, fmt.Errorf("sink %s: %w", k.Name, err)
			}
			for name := range headers {
				if out.Headers == nil {
					out.Headers = map[string]string{}
				}
				out.Headers[name] = variable("sink", k.Name, name)
			}
		}
		f.Sinks = append(f.Sinks, out)
	}
	rules, err := s.DB.NotificationRules(ctx)
	if err != nil {
		return f, err
	}
	for _, r := range rules {
		f.Rules = append(f.Rules, apitypes.ConfigRule{Channel: r.ChannelName, Project: r.Project, App: r.App, Events: r.Events})
	}

	backups, err := s.DB.BackupConfig(ctx)
	if err != nil {
		return f, err
	}
	if !backups.UpdatedAt.IsZero() {
		f.Backups = &apitypes.ConfigBackups{Destination: &backups.Destination, Endpoint: &backups.Endpoint, Region: &backups.Region, AccessKey: &backups.AccessKey, Schedule: &backups.Schedule, Keep: &backups.Keep}
		if len(backups.SealedSecret) > 0 {
			f.Backups.SecretKey = variable("backup", "secret", "key")
		}
	}
	signIn, err := s.DB.SSOConfig(ctx)
	if err != nil {
		return f, err
	}
	if signIn.Issuer != "" {
		f.SignIn = &apitypes.ConfigSignIn{Issuer: signIn.Issuer, ClientID: signIn.ClientID, Label: &signIn.Label, Scopes: &signIn.Scopes, EmailDomains: &signIn.EmailDomains,
			Provision: &signIn.Provision, TrustEmails: &signIn.TrustEmails, GroupClaim: &signIn.GroupClaim, GroupRules: &signIn.GroupRules, PasswordsOff: &signIn.PasswordsOff}
		if len(signIn.SealedSecret) > 0 {
			f.SignIn.ClientSecret = variable("sso", "client", "secret")
		}
	}
	return f, nil
}

// run is one bringing in.
type run struct {
	ctx    context.Context
	dry    bool
	by     string
	report apitypes.ConfigReport
	// Those that are there now or, in a dry run, would be.
	users    map[string]string
	projects map[string]bool
	channels map[string]bool
}

func (r *run) add(what, name, action, note string) {
	r.report.Items = append(r.report.Items, apitypes.ConfigItem{What: what, Name: name, Action: action, Note: note})
}

func (r *run) failed(what, name string, err error) {
	r.add(what, name, apitypes.ConfigFailed, err.Error())
}

// Import makes a server match a configuration, in what it mentions. Something that cannot be done is said in
// the report and does not stop the rest. It goes in an order in which each thing has what it depends on, and
// with signing in last, so that nothing can get in the way of the rest.
func (s *Service) Import(ctx context.Context, f apitypes.ServerConfig, dry bool, by, ip string) (apitypes.ConfigReport, error) {
	if f.Format != Format {
		return apitypes.ConfigReport{}, &InvalidError{fmt.Sprintf("This is format %d of a server's configuration, and this server reads format %d.", f.Format, Format)}
	}
	r := &run{ctx: ctx, dry: dry, by: by, users: map[string]string{}, projects: map[string]bool{}, channels: map[string]bool{}}
	r.report.DryRun = dry
	for _, step := range []func(*run, apitypes.ServerConfig) error{s.importUsers, s.importProjects, s.importRegistries, s.importChannels, s.importRules, s.importSinks, s.importBackups, s.importSignIn} {
		if err := step(r, f); err != nil {
			return r.report, err
		}
	}
	if !dry {
		s.Auth.Audit(ctx, store.AuditEntry{Actor: by, IP: ip, Action: "server.config.import", Result: "ok"})
	}
	return r.report, nil
}

func (s *Service) importUsers(r *run, f apitypes.ServerConfig) error {
	for _, want := range f.Users {
		email, err := auth.NormalizeEmail(want.Email)
		if err != nil {
			r.failed("user", want.Email, err)
			continue
		}
		role := want.Role
		if role != "" && role != store.RoleAdmin && role != store.RoleMember {
			r.failed("user", email, fmt.Errorf("a user's role is admin or member, and not %q", role))
			continue
		}
		u, err := s.DB.UserByEmail(r.ctx, email)
		if errors.Is(err, store.ErrNotFound) {
			r.users[email] = "" // there will be one, whose ID is not known in a dry run
			if role == "" {
				role = store.RoleMember
			}
			if !r.dry {
				u, err := s.Auth.NewUser(r.ctx, email, want.Name, role, "")
				if err == nil {
					err = s.DB.CreateUser(r.ctx, u)
				}
				if err == nil && want.Disabled {
					err = s.DB.SetUserDisabled(r.ctx, u.ID, true)
				}
				if err != nil {
					delete(r.users, email)
					r.failed("user", email, err)
					continue
				}
				r.users[email] = u.ID
			}
			r.add("user", email, apitypes.ConfigCreate, "they need a link to choose a password, or single sign-on: moor server users invite "+email)
			continue
		}
		if err != nil {
			return err
		}
		r.users[email] = u.ID
		if role == "" {
			role = u.Role
		}
		var changes []string
		if u.Role != role {
			changes = append(changes, "role "+u.Role+" to "+role)
		}
		disable := want.Disabled && !u.Disabled()
		if disable {
			changes = append(changes, "disabled")
		}
		if len(changes) == 0 {
			r.add("user", email, apitypes.ConfigSame, "")
			continue
		}
		if !r.dry {
			if u.Role != role {
				if err := s.DB.SetUserRole(r.ctx, u.ID, role); err != nil {
					r.failed("user", email, err)
					continue
				}
			}
			if disable {
				if err := s.DB.SetUserDisabled(r.ctx, u.ID, true); err != nil {
					r.failed("user", email, err)
					continue
				}
			}
		}
		r.add("user", email, apitypes.ConfigChange, strings.Join(changes, ", "))
	}
	return nil
}

func (s *Service) importProjects(r *run, f apitypes.ServerConfig) error {
	existing, err := s.DB.Projects(r.ctx)
	if err != nil {
		return err
	}
	for _, p := range existing {
		r.projects[p.Name] = true
	}
	for _, want := range f.Projects {
		var have *store.Project
		for i := range existing {
			if existing[i].Name == want.Name {
				have = &existing[i]
			}
		}
		// What the file leaves out stays as it is, or is as a new project has it.
		everyone, seeLogs := "", true
		if have != nil {
			everyone, seeLogs = have.EveryoneRole, have.ViewersSeeLogs
		}
		if want.EveryoneRole != nil {
			everyone = *want.EveryoneRole
		}
		if want.ViewersSeeLogs != nil {
			seeLogs = *want.ViewersSeeLogs
		}
		if everyone != "" && !store.ValidRole(everyone) {
			r.failed("project", want.Name, fmt.Errorf("a role is viewer, deployer or maintainer, and not %q", everyone))
			continue
		}
		action := apitypes.ConfigSame
		if have == nil {
			action = apitypes.ConfigCreate
			if err := names.ValidateProject(want.Name); err != nil {
				r.failed("project", want.Name, err)
				continue
			}
			if !r.dry {
				if _, err := s.DB.CreateProject(r.ctx, want.Name); err != nil {
					r.failed("project", want.Name, err)
					continue
				}
			}
			r.projects[want.Name] = true
		}
		if have == nil || have.EveryoneRole != everyone || have.ViewersSeeLogs != seeLogs {
			if action == apitypes.ConfigSame {
				action = apitypes.ConfigChange
			}
			if !r.dry {
				if err := s.DB.SetProject(r.ctx, want.Name, everyone, seeLogs); err != nil {
					r.failed("project", want.Name, err)
					continue
				}
			}
		}
		// Who is in it.
		var had map[string]string
		if have != nil {
			members, err := s.DB.Members(r.ctx, want.Name)
			if err != nil {
				return err
			}
			had = map[string]string{}
			for _, m := range members {
				had[m.Email] = m.Role
			}
		}
		emails := make([]string, 0, len(want.Members))
		for email := range want.Members {
			emails = append(emails, email)
		}
		sort.Strings(emails)
		var notes []string
		for _, given := range emails {
			role := want.Members[given]
			email, err := auth.NormalizeEmail(given)
			if err != nil {
				notes = append(notes, given+" is not an address")
				continue
			}
			if !store.ValidRole(role) {
				notes = append(notes, fmt.Sprintf("%s cannot have the role %q", email, role))
				continue
			}
			if had[email] == role {
				continue
			}
			id, known := r.users[email]
			if !known {
				u, err := s.DB.UserByEmail(r.ctx, email)
				if errors.Is(err, store.ErrNotFound) {
					notes = append(notes, email+" has no account")
					continue
				}
				if err != nil {
					return err
				}
				id = u.ID
			}
			if action == apitypes.ConfigSame {
				action = apitypes.ConfigChange
			}
			if !r.dry {
				if err := s.DB.SetMember(r.ctx, want.Name, id, role); err != nil {
					notes = append(notes, fmt.Sprintf("%s: %v", email, err))
				}
			}
		}
		if len(notes) > 0 {
			r.add("project", want.Name, apitypes.ConfigFailed, strings.Join(notes, "; "))
			continue
		}
		r.add("project", want.Name, action, "")
	}
	return nil
}

func (s *Service) importRegistries(r *run, f apitypes.ServerConfig) error {
	existing, err := s.DB.Registries(r.ctx)
	if err != nil {
		return err
	}
	for _, want := range f.Registries {
		host := strings.ToLower(want.Host)
		if !names.ValidRegistryHost(host) {
			r.failed("registry", want.Host, errors.New("a registry is named by its host, such as ghcr.io"))
			continue
		}
		var have *store.Registry
		for i := range existing {
			if existing[i].Host == host {
				have = &existing[i]
			}
		}
		password, given := actual(want.Password)
		switch {
		case have == nil && !given:
			r.add("registry", host, apitypes.ConfigSkip, missing(want.Password))
			continue
		case want.Username == "":
			r.failed("registry", host, errors.New("a registry needs a username"))
			continue
		}
		action := apitypes.ConfigCreate
		if have != nil {
			action = apitypes.ConfigSame
			current, err := s.Keyring.OpenRegistry(host, have.Sealed)
			if err != nil {
				return err
			}
			if !given {
				password = current
			}
			if have.Username != want.Username || password != current {
				action = apitypes.ConfigChange
			}
		}
		if action != apitypes.ConfigSame && !r.dry {
			reg := store.Registry{Host: host, Username: want.Username, Sealed: s.Keyring.SealRegistry(host, password), CreatedAt: time.Now().UTC()}
			if err := s.DB.SetRegistry(r.ctx, reg); err != nil {
				r.failed("registry", host, err)
				continue
			}
		}
		r.add("registry", host, action, "")
	}
	return nil
}

func (s *Service) importChannels(r *run, f apitypes.ServerConfig) error {
	existing, err := s.DB.NotificationChannels(r.ctx)
	if err != nil {
		return err
	}
	for _, c := range existing {
		r.channels[c.Name] = true
	}
	for _, want := range f.Channels {
		var have *store.NotificationChannel
		for i := range existing {
			if existing[i].Name == want.Name {
				have = &existing[i]
			}
		}
		cfg := notify.Config{Host: want.Host, Port: want.Port, Username: want.Username, From: want.From, To: want.To}
		cfg.URL, _ = actual(want.URL)
		cfg.Secret, _ = actual(want.Secret)
		cfg.Password, _ = actual(want.Password)

		if have == nil {
			if want.Kind != notify.KindEmail && cfg.URL == "" {
				r.add("channel", want.Name, apitypes.ConfigSkip, missing(want.URL))
				continue
			}
			if want.Kind == notify.KindEmail && cfg.Password == "" && want.Password != "" {
				r.add("channel", want.Name, apitypes.ConfigSkip, missing(want.Password))
				continue
			}
			if err := cfg.Check(want.Kind); err != nil {
				r.failed("channel", want.Name, err)
				continue
			}
			note := ""
			if !r.dry {
				_, secret, err := s.Notifier.AddChannel(r.ctx, want.Name, want.Kind, cfg, r.by)
				if err != nil {
					r.failed("channel", want.Name, err)
					continue
				}
				if want.Kind == notify.KindWebhook && secret != "" && !given(want.Secret) {
					note = "it signs with " + secret + ", which is not shown again"
				}
			} else if want.Kind == notify.KindWebhook && !given(want.Secret) {
				note = "it would have a new secret to sign with"
			}
			r.channels[want.Name] = true
			r.add("channel", want.Name, apitypes.ConfigCreate, note)
			continue
		}

		current, err := s.Notifier.ChannelConfig(*have)
		if err != nil {
			return fmt.Errorf("channel %s: %w", want.Name, err)
		}
		// What is not given is kept.
		if cfg.URL == "" {
			cfg.URL = current.URL
		}
		if cfg.Secret == "" {
			cfg.Secret = current.Secret
		}
		if cfg.Password == "" {
			cfg.Password = current.Password
		}
		if cfg.Host == "" {
			cfg.Host = current.Host
		}
		if cfg.Port == 0 {
			cfg.Port = current.Port
		}
		if cfg.Username == "" {
			cfg.Username = current.Username
		}
		if cfg.From == "" {
			cfg.From = current.From
		}
		if len(cfg.To) == 0 {
			cfg.To = current.To
		}
		if want.Kind == "" {
			want.Kind = have.Kind
		}
		if want.Kind != have.Kind {
			r.failed("channel", want.Name, fmt.Errorf("there is a %s channel of this name, and it cannot become a %s one", have.Kind, want.Kind))
			continue
		}
		if err := cfg.Check(want.Kind); err != nil {
			r.failed("channel", want.Name, err)
			continue
		}
		if sameChannel(cfg, current) {
			r.add("channel", want.Name, apitypes.ConfigSame, "")
			continue
		}
		if !r.dry {
			if err := s.Notifier.SetChannelConfig(r.ctx, *have, cfg); err != nil {
				r.failed("channel", want.Name, err)
				continue
			}
		}
		r.add("channel", want.Name, apitypes.ConfigChange, "")
	}
	return nil
}

// importSinks brings in where things are sent. What a file leaves out stays, as anywhere else.
func (s *Service) importSinks(r *run, f apitypes.ServerConfig) error {
	if s.Sinks == nil {
		return nil
	}
	existing, err := s.DB.TelemetrySinks(r.ctx)
	if err != nil {
		return err
	}
	for _, want := range f.Sinks {
		var have *store.TelemetrySink
		for i := range existing {
			if existing[i].Name == want.Name {
				have = &existing[i]
			}
		}
		var headers map[string]string
		waiting := ""
		for name, value := range want.Headers {
			if plain, ok := actual(value); ok {
				if headers == nil {
					headers = map[string]string{}
				}
				headers[name] = plain
			} else {
				waiting = missing(value)
			}
		}
		if waiting != "" && (have == nil || have.Address != strings.TrimRight(want.Address, "/")) {
			r.add("sink", want.Name, apitypes.ConfigSkip, waiting)
			continue
		}
		if waiting != "" {
			headers = nil // not some of them: what it has stays, as it would with none of them there
		}
		appID := ""
		if want.App != "" {
			if appID, err = s.DB.AppID(r.ctx, want.App); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					return err
				}
				r.add("sink", want.Name, apitypes.ConfigSkip, "there is no app "+want.App+" here yet")
				continue
			}
		}
		form := telemetry.Form{Name: want.Name, Kind: want.Kind, Address: want.Address, Headers: headers, Logs: want.Logs, Metrics: want.Metrics, AppID: appID}
		if err := form.Check(); err != nil {
			r.failed("sink", want.Name, err)
			continue
		}
		action := apitypes.ConfigCreate
		if have != nil {
			action = apitypes.ConfigChange
			if have.Kind == want.Kind && have.Address == strings.TrimRight(want.Address, "/") && have.Logs == want.Logs && have.Metrics == want.Metrics && have.App == want.App && headers == nil {
				action = apitypes.ConfigSame
			}
		}
		if action != apitypes.ConfigSame && !r.dry {
			if _, err := s.Sinks.Set(r.ctx, form, r.by); err != nil {
				r.failed("sink", want.Name, err)
				continue
			}
		}
		r.add("sink", want.Name, action, "")
	}
	return nil
}

func given(value string) bool {
	_, ok := actual(value)
	return ok
}

func sameChannel(a, b notify.Config) bool {
	return a.URL == b.URL && a.Secret == b.Secret && a.Host == b.Host && a.Port == b.Port && a.Username == b.Username &&
		a.Password == b.Password && a.From == b.From && slices.Equal(a.To, b.To)
}

func ruleKey(channel, project, app string, events []string) string {
	sorted := slices.Clone(events)
	sort.Strings(sorted)
	return strings.Join([]string{channel, project, app, strings.Join(sorted, ",")}, "\x00")
}

func ruleName(want apitypes.ConfigRule) string {
	switch {
	case want.App != "":
		return want.Channel + ", for app " + want.App
	case want.Project != "":
		return want.Channel + ", for project " + want.Project
	}
	return want.Channel + ", for the server"
}

func (s *Service) importRules(r *run, f apitypes.ServerConfig) error {
	existing, err := s.DB.NotificationRules(r.ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, rule := range existing {
		have[ruleKey(rule.ChannelName, rule.Project, rule.App, rule.Events)] = true
	}
	for _, want := range f.Rules {
		name := ruleName(want)
		if !r.channels[want.Channel] {
			r.add("rule", name, apitypes.ConfigSkip, "its channel is not here")
			continue
		}
		key := ruleKey(want.Channel, want.Project, want.App, want.Events)
		if have[key] {
			r.add("rule", name, apitypes.ConfigSame, "")
			continue
		}
		have[key] = true
		if err := s.checkRule(r, want); err != nil {
			r.failed("rule", name, err)
			continue
		}
		if !r.dry {
			if _, err := s.Notifier.AddRule(r.ctx, want.Channel, want.Project, want.App, want.Events, r.by); err != nil {
				r.failed("rule", name, err)
				continue
			}
		}
		r.add("rule", name, apitypes.ConfigCreate, "")
	}
	return nil
}

// checkRule says what would stop a rule, so that a dry run says it too.
func (s *Service) checkRule(r *run, want apitypes.ConfigRule) error {
	if err := notify.CheckRule(want.Project, want.App, want.Events); err != nil {
		return err
	}
	if want.Project != "" && !r.projects[want.Project] {
		return fmt.Errorf("there is no project %s here", want.Project)
	}
	if want.App != "" {
		if _, err := s.DB.AppProject(r.ctx, want.App); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("there is no app %s here yet", want.App)
		}
	}
	return nil
}

func (s *Service) importBackups(r *run, f apitypes.ServerConfig) error {
	want := f.Backups
	if want == nil {
		return nil
	}
	current, err := s.DB.BackupConfig(r.ctx)
	if err != nil {
		return err
	}
	form := backup.Form{
		Destination: value(want.Destination, current.Destination), Endpoint: value(want.Endpoint, current.Endpoint), Region: value(want.Region, current.Region),
		AccessKey: value(want.AccessKey, current.AccessKey), Schedule: value(want.Schedule, current.Schedule), Keep: value(want.Keep, current.Keep),
	}
	secret, hasSecret := actual(want.SecretKey)
	form.SecretKey = secret
	same := !current.UpdatedAt.IsZero() && current.Destination == form.Destination && current.Endpoint == form.Endpoint && current.Region == form.Region &&
		current.AccessKey == form.AccessKey && current.Schedule == form.Schedule && current.Keep == form.Keep
	if same && hasSecret {
		if len(current.SealedSecret) == 0 {
			same = false
		} else {
			old, err := s.Keyring.OpenBackup(current.SealedSecret)
			if err != nil {
				return err
			}
			same = old == secret
		}
	}
	if same {
		r.add("backups", form.Destination, apitypes.ConfigSame, "")
		return nil
	}
	if strings.HasPrefix(form.Destination, "s3://") && !hasSecret && len(current.SealedSecret) == 0 {
		r.add("backups", form.Destination, apitypes.ConfigSkip, missing(want.SecretKey))
		return nil
	}
	action := apitypes.ConfigChange
	if current.UpdatedAt.IsZero() {
		action = apitypes.ConfigCreate
	}
	if r.dry {
		err = s.Backups.CheckForm(r.ctx, form)
	} else {
		err = s.Backups.Configure(r.ctx, form, r.by)
	}
	if err != nil {
		r.failed("backups", form.Destination, err)
		return nil
	}
	r.add("backups", form.Destination, action, "")
	return nil
}

// value is what a file says, or otherwise what is there.
func value[T any](said *T, there T) T {
	if said != nil {
		return *said
	}
	return there
}

func (s *Service) importSignIn(r *run, f apitypes.ServerConfig) error {
	want := f.SignIn
	if want == nil {
		return nil
	}
	current, err := s.DB.SSOConfig(r.ctx)
	if err != nil {
		return err
	}
	form := sso.Form{
		Issuer: want.Issuer, ClientID: want.ClientID, Label: value(want.Label, current.Label), Scopes: value(want.Scopes, current.Scopes),
		EmailDomains: value(want.EmailDomains, current.EmailDomains), Provision: value(want.Provision, current.Provision), TrustEmails: value(want.TrustEmails, current.TrustEmails),
		GroupClaim: value(want.GroupClaim, current.GroupClaim), GroupRules: value(want.GroupRules, current.GroupRules), PasswordsOff: value(want.PasswordsOff, current.PasswordsOff),
	}
	if form.Issuer == "" {
		form.Issuer = current.Issuer
	}
	if form.ClientID == "" {
		form.ClientID = current.ClientID
	}
	secret, hasSecret := actual(want.ClientSecret)
	form.ClientSecret = secret
	same := current.Issuer == form.Issuer && current.ClientID == form.ClientID && current.Label == form.Label && current.Scopes == form.Scopes &&
		current.EmailDomains == form.EmailDomains && current.Provision == form.Provision && current.TrustEmails == form.TrustEmails && current.GroupClaim == form.GroupClaim &&
		current.GroupRules == form.GroupRules && current.PasswordsOff == form.PasswordsOff
	if same && hasSecret {
		if len(current.SealedSecret) == 0 {
			same = false
		} else {
			old, err := s.Keyring.OpenSSO(current.SealedSecret)
			if err != nil {
				return err
			}
			same = old == secret
		}
	}
	if same {
		r.add("single sign-on", form.Issuer, apitypes.ConfigSame, "")
		return nil
	}
	if !hasSecret && len(current.SealedSecret) == 0 && want.ClientSecret != "" {
		r.add("single sign-on", form.Issuer, apitypes.ConfigSkip, missing(want.ClientSecret))
		return nil
	}
	action := apitypes.ConfigChange
	if current.Issuer == "" {
		action = apitypes.ConfigCreate
	}
	note := "tell your provider to send people back to " + s.SSO.RedirectURL()
	if r.dry {
		note = "your provider is only asked when this is done; " + note
	} else if err := s.SSO.Configure(r.ctx, form, r.by); err != nil {
		r.failed("single sign-on", form.Issuer, err)
		return nil
	}
	r.add("single sign-on", form.Issuer, action, note)
	return nil
}
