// Package console serves the pages on the console's hostname: the few an account
// needs (the first-run claim, signing in and out, choosing a password, approving a
// CLI's sign-in) and the web console itself. Every page is plain HTML and forms;
// scripts add the live parts (spec/09-interfaces.md, "Web console").
package console

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/brand"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/doctor"
	"github.com/robert-mcdermott/moorage/internal/forge"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/sso"
	"github.com/robert-mcdermott/moorage/internal/store"
	"github.com/robert-mcdermott/moorage/internal/telemetry"
)

//go:embed templates assets
var embedded embed.FS

// cards are the pages shown alone in a card, for someone not yet at home here;
// the others are in the console's frame.
var cards = map[string]bool{
	"unclaimed": true, "setup": true, "login": true, "login-code": true, "invite": true,
	"device-enter": true, "device-confirm": true, "message": true,
}

func pageNames() []string {
	names := []string{"login-code", "apps", "activity", "welcome", "app", "deployments", "deployment", "deploy", "new-app", "logs", "variables", "variables-edit",
		"account", "account-keys", "account-tokens", "account-sessions", "account-security", "settings", "settings-users", "settings-registries", "settings-notifications", "settings-backups", "settings-sign-in", "notifications", "settings-system", "settings-audit", "projects", "project", "app-settings", "app-environments", "previews", "app-access", "app-cron", "app-run", "app-metrics", "app-console", "app-files", "settings-telemetry"}
	for name := range cards {
		names = append(names, name)
	}
	return names
}

// parseTemplates makes each page: the templates with "body" being that page's.
func parseTemplates(source fs.FS, asset func(string) string) (map[string]*template.Template, error) {
	base, err := template.New("pages").Funcs(template.FuncMap{
		"asset": asset, "ago": ago, "iso": iso, "clock": clock, "date": func(t time.Time) string { return t.Format("2 Jan 2006") },
		"mask": func() string { return secretMask }, "countText": countText, "size": humanSizeOf, "envq": envQuery, "agoOrDash": agoOrDash, "untilText": untilText, "bareURL": bareURL,
		"orDash": orNone,
		"json": func(v any) (string, error) {
			encoded, err := json.Marshal(v)
			return string(encoded), err
		},
		"orNobody": func(s string) string {
			if s == "" {
				return "Nobody"
			}
			return s
		},
		"dict": func(pairs ...any) map[string]any {
			out := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				out[pairs[i].(string)] = pairs[i+1]
			}
			return out
		},
	}).ParseFS(source, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := map[string]*template.Template{}
	for _, page := range pageNames() {
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := t.New("body").Parse(`{{template "` + page + `" .}}`); err != nil {
			return nil, err
		}
		out[page] = t
	}
	return out, nil
}

// Options configure the console.
type Options struct {
	Auth   *auth.Service
	DB     *store.DB
	Config config.Config
	Log    *slog.Logger
	// Dir, if set, is the source of the console's templates and assets
	// (internal/console), read again for every request, for working on them.
	Dir string
	// Deployer does what the pages' forms ask of a deployment, and Runtime says what
	// is running.
	Deployer *deploy.Deployer
	Runtime  Runtime
	// Keyring seals what the settings pages are given to keep.
	Keyring Keyring
	// GateRoute says what a hostname and a path serve, for the sign-in gates.
	GateRoute func(host, path string) (ingress.RouteInfo, bool)
	// Metrics has what environments have been doing.
	Metrics *metrics.Store
	// Notifier sends what the settings pages set up.
	Notifier *notify.Service
	// Backups makes backups.
	Backups *backup.Service
	// Forges connects apps to repositories.
	Forges *forge.Service
	// Telemetry sends what apps say and do elsewhere.
	Telemetry *telemetry.Service
	// SSO signs people in through a provider.
	SSO *sso.Service
}

// Console serves the pages.
type Console struct {
	auth   *auth.Service
	db     *store.DB
	cfg    config.Config
	log    *slog.Logger
	source fs.FS
	dev    bool
	assets map[string]asset
	pages  map[string]*template.Template

	deployer  *deploy.Deployer
	forges    *forge.Service
	telemetry *telemetry.Service
	runtime   Runtime
	keyring   Keyring
	started   time.Time
	// gateRoute says what a hostname and a path serve, for the sign-in gates.
	gateRoute func(host, path string) (ingress.RouteInfo, bool)
	metrics   *metrics.Store
	notifier  *notify.Service
	backups   *backup.Service
	sso       *sso.Service
}

// New returns the pages' handler.
func New(o Options) (http.Handler, error) {
	c := &Console{forges: o.Forges, telemetry: o.Telemetry, notifier: o.Notifier, backups: o.Backups, sso: o.SSO, auth: o.Auth, db: o.DB, cfg: o.Config, log: o.Log, source: embedded, deployer: o.Deployer, runtime: o.Runtime, keyring: o.Keyring, started: time.Now(), gateRoute: o.GateRoute, metrics: o.Metrics}
	if o.Dir != "" {
		c.source, c.dev = os.DirFS(o.Dir), true
	}
	var err error
	if c.assets, err = loadAssets(c.source); err != nil {
		return nil, err
	}
	if c.pages, err = parseTemplates(c.source, c.assetURL); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", c.home)
	mux.HandleFunc("GET /assets/{file}", c.serveAsset)
	mux.HandleFunc("GET /activity", c.activity)
	mux.HandleFunc("GET /welcome", c.welcome)
	mux.HandleFunc("GET /new", c.newAppPage)
	mux.HandleFunc("POST /new", c.accountAction(c.createApp))
	mux.HandleFunc("GET /projects", c.projectsPage)
	mux.HandleFunc("POST /projects", c.adminAction(c.createProject))
	mux.HandleFunc("GET /projects/{project}", c.projectPage)
	mux.HandleFunc("POST /projects/{project}/settings", c.projectAction(c.changeProjectSettings))
	mux.HandleFunc("POST /projects/{project}/members/set", c.projectAction(c.setProjectMember))
	mux.HandleFunc("POST /projects/{project}/members/remove", c.projectAction(c.removeProjectMember))
	mux.HandleFunc("POST /projects/{project}/delete", c.projectAction(c.deleteProject))
	mux.HandleFunc("GET /settings/audit", c.auditPage)
	mux.HandleFunc("POST /apps/{app}/variables/reveal", c.actionWith(auth.ScopeAdmin, maxForm, c.revealVariable))
	mux.HandleFunc("GET /apps/{app}", c.appPage)
	mux.HandleFunc("GET /apps/{app}/deployments", c.deploymentsList)
	mux.HandleFunc("GET /apps/{app}/deploy", c.deployPage)
	mux.HandleFunc("POST /apps/{app}/deploy/image", c.deployImage)
	mux.HandleFunc("POST /apps/{app}/deploy/archive", c.deployArchive)
	mux.HandleFunc("POST /apps/{app}/deploy/site", c.deploySite)
	mux.HandleFunc("POST /apps/{app}/restart", c.action(c.restart))
	mux.HandleFunc("POST /apps/{app}/redeploy", c.action(c.redeploy))
	mux.HandleFunc("POST /apps/{app}/rollback", c.action(c.rollback))
	mux.HandleFunc("POST /apps/{app}/scale", c.action(c.scale))
	mux.HandleFunc("GET /deployments/{id}", c.deploymentPage)
	mux.HandleFunc("GET /apps/{app}/metrics", c.metricsPage)
	mux.HandleFunc("GET /apps/{app}/cron", c.cronPage)
	mux.HandleFunc("GET /apps/{app}/runs/{id}", c.runPage)
	mux.HandleFunc("GET /apps/{app}/console", c.consolePage)
	mux.HandleFunc("POST /apps/{app}/console/run", c.actionWith(auth.ScopeDeploy, maxForm, c.consoleRun))
	mux.HandleFunc("GET /apps/{app}/files", c.filesPage)
	mux.HandleFunc("GET /apps/{app}/files/download", c.downloadFile)
	mux.HandleFunc("POST /apps/{app}/files/upload", c.uploadAction(deploy.MaxDataFile+1<<20, c.uploadFile))
	mux.HandleFunc("POST /apps/{app}/files/folder", c.actionWith(auth.ScopeDeploy, maxForm, c.makeDirectory))
	mux.HandleFunc("POST /apps/{app}/files/remove", c.actionWith(auth.ScopeDeploy, maxForm, c.removeFile))
	mux.HandleFunc("POST /apps/{app}/runs", c.action(c.startRun))
	mux.HandleFunc("POST /apps/{app}/runs/{id}/stop", c.action(c.stopRun))
	mux.HandleFunc("POST /apps/{app}/cron/{job}/run", c.action(c.runCronJob))
	mux.HandleFunc("GET /auth/gate", c.gateSignIn)
	mux.HandleFunc("GET /apps/{app}/access", c.accessPage)
	mux.HandleFunc("POST /apps/{app}/access", c.actionWith(auth.ScopeAdmin, maxForm, c.setAccess))
	mux.HandleFunc("POST /apps/{app}/domains", c.actionWith(auth.ScopeAdmin, maxForm, c.addDomain))
	mux.HandleFunc("POST /apps/{app}/domains/verify", c.actionWith(auth.ScopeAdmin, maxForm, c.verifyDomain))
	mux.HandleFunc("POST /apps/{app}/domains/remove", c.actionWith(auth.ScopeAdmin, maxForm, c.removeDomain))
	mux.HandleFunc("POST /apps/{app}/share-links", c.action(c.createShareLink))
	mux.HandleFunc("POST /apps/{app}/share-links/{id}/revoke", c.action(c.revokeShareLink))
	mux.HandleFunc("POST /apps/{app}/sleep", c.action(c.sleepApp))
	mux.HandleFunc("POST /apps/{app}/wake", c.action(c.wakeApp))
	mux.HandleFunc("GET /previews", c.previewsPage)
	mux.HandleFunc("GET /apps/{app}/environments", c.environmentsPage)
	mux.HandleFunc("POST /apps/{app}/environments", c.action(c.createEnvironment))
	mux.HandleFunc("POST /apps/{app}/previews", c.action(c.createPreview))
	mux.HandleFunc("POST /apps/{app}/environments/{env}/delete", c.environmentAction(c.deleteEnvironment))
	mux.HandleFunc("POST /apps/{app}/environments/{env}/lock", c.environmentAction(c.lockEnvironment))
	mux.HandleFunc("POST /apps/{app}/environments/{env}/unlock", c.environmentAction(c.unlockEnvironment))
	mux.HandleFunc("POST /apps/{app}/environments/{env}/pin", c.environmentAction(c.pinEnvironment))
	mux.HandleFunc("POST /apps/{app}/environments/{env}/promote", c.environmentAction(c.promoteEnvironment))
	mux.HandleFunc("GET /apps/{app}/settings", c.appSettingsPage)
	mux.HandleFunc("POST /apps/{app}/settings/move", c.actionWith(auth.ScopeAdmin, maxForm, c.moveApp))
	mux.HandleFunc("POST /apps/{app}/settings/forge/connect", c.actionWith(auth.ScopeAdmin, maxForm, c.connectForge))
	mux.HandleFunc("POST /apps/{app}/settings/forge/disconnect", c.actionWith(auth.ScopeAdmin, maxForm, c.disconnectForge))
	mux.HandleFunc("POST /apps/{app}/settings/forge/sync", c.actionWith(auth.ScopeAdmin, maxForm, c.syncForge))
	mux.HandleFunc("POST /apps/{app}/settings/forge/pulls/{number}/approve", c.actionWith(auth.ScopeAdmin, maxForm, c.approveForgePull))
	mux.HandleFunc("POST /apps/{app}/settings/rename", c.actionWith(auth.ScopeAdmin, maxForm, c.renameApp))
	mux.HandleFunc("POST /apps/{app}/settings/delete", c.actionWith(auth.ScopeAdmin, maxForm, c.deleteApp))
	mux.HandleFunc("GET /apps/{app}/logs", c.logsPage)
	mux.HandleFunc("GET /apps/{app}/variables", c.variablesPage)
	mux.HandleFunc("GET /apps/{app}/variables/edit", c.editVariablesPage)
	mux.HandleFunc("POST /apps/{app}/variables/set", c.actionWith(auth.ScopeAdmin, maxVarsForm, c.setVariable))
	mux.HandleFunc("POST /apps/{app}/variables/unset", c.actionWith(auth.ScopeAdmin, maxVarsForm, c.unsetVariable))
	mux.HandleFunc("POST /apps/{app}/variables/preview", c.actionWith(auth.ScopeAdmin, maxVarsForm, c.previewVariables))
	mux.HandleFunc("POST /apps/{app}/variables/apply", c.actionWith(auth.ScopeAdmin, maxVarsForm, c.applyVariables))
	mux.HandleFunc("POST /apps/{app}/variables/release", c.actionWith(auth.ScopeAdmin, maxVarsForm, c.releaseVariables))
	mux.HandleFunc("GET /account", c.accountPage("profile", "Your account", "account", c.loadProfile))
	mux.HandleFunc("GET /account/keys", c.accountPage("keys", "SSH keys", "account-keys", c.loadKeys))
	mux.HandleFunc("GET /account/tokens", c.accountPage("tokens", "API tokens", "account-tokens", c.loadTokens))
	mux.HandleFunc("GET /account/sessions", c.accountPage("sessions", "Sessions", "account-sessions", c.loadSessions))
	mux.HandleFunc("POST /account/password", c.accountAction(c.changePassword))
	mux.HandleFunc("POST /account/keys/add", c.accountAction(c.addKey))
	mux.HandleFunc("POST /account/keys/remove", c.accountAction(c.removeKey))
	mux.HandleFunc("POST /account/tokens/create", c.accountAction(c.createToken))
	mux.HandleFunc("POST /account/tokens/revoke", c.accountAction(c.revokeToken))
	mux.HandleFunc("POST /account/sessions/end", c.accountAction(c.endSession))
	mux.HandleFunc("POST /account/sessions/end-others", c.accountAction(c.endOtherSessions))
	mux.HandleFunc("GET /settings", c.settingsPage)
	mux.HandleFunc("GET /settings/users", c.usersPage)
	mux.HandleFunc("GET /settings/registries", c.registriesPage)
	mux.HandleFunc("GET /settings/notifications", c.notificationSettings)
	mux.HandleFunc("GET /settings/telemetry", c.telemetrySettings)
	mux.HandleFunc("POST /settings/telemetry", c.adminAction(c.addSink))
	mux.HandleFunc("POST /settings/telemetry/test", c.adminAction(c.testSink))
	mux.HandleFunc("POST /settings/telemetry/remove", c.adminAction(c.removeSink))
	mux.HandleFunc("POST /apps/{app}/metrics/thresholds", c.actionWith(auth.ScopeAdmin, maxForm, c.addThreshold))
	mux.HandleFunc("POST /apps/{app}/metrics/thresholds/remove", c.actionWith(auth.ScopeAdmin, maxForm, c.removeThreshold))
	mux.HandleFunc("POST /settings/notifications/channels", c.adminAction(c.addChannel))
	mux.HandleFunc("POST /settings/notifications/channels/test", c.adminAction(c.testChannel))
	mux.HandleFunc("POST /settings/notifications/channels/remove", c.adminAction(c.removeChannel))
	mux.HandleFunc("POST /settings/notifications/rules", c.adminAction(c.addServerRule))
	mux.HandleFunc("POST /settings/notifications/rules/remove", c.adminAction(c.removeServerRule))
	mux.HandleFunc("GET /settings/backups", c.backupsPage)
	mux.HandleFunc("POST /settings/backups/now", c.adminAction(c.startBackup))
	mux.HandleFunc("POST /settings/backups/config", c.adminAction(c.configureBackups))
	mux.HandleFunc("GET /account/security", c.securityPage)
	mux.HandleFunc("POST /account/security/start", c.accountAction(c.startSecondFactor))
	mux.HandleFunc("POST /account/security/confirm", c.accountAction(c.confirmSecondFactor))
	mux.HandleFunc("POST /account/security/off", c.accountAction(c.turnOffSecondFactor))
	mux.HandleFunc("POST /settings/users/second-factor-off", c.adminAction(c.adminSecondFactorOff))
	mux.HandleFunc("GET /notifications", c.inbox)
	mux.HandleFunc("POST /notifications/read", c.accountAction(c.readInbox))
	mux.HandleFunc("POST /projects/{project}/notifications", c.projectAction(c.addProjectRule))
	mux.HandleFunc("POST /projects/{project}/notifications/remove", c.projectAction(c.removeProjectRule))
	mux.HandleFunc("GET /settings/system", c.systemPage)
	mux.HandleFunc("POST /settings/users/create", c.adminAction(c.createUser))
	mux.HandleFunc("POST /settings/users/invite", c.adminAction(c.inviteUser))
	mux.HandleFunc("POST /settings/users/change", c.adminAction(c.changeUser))
	mux.HandleFunc("POST /settings/registries/set", c.adminAction(c.setRegistry))
	mux.HandleFunc("POST /settings/registries/remove", c.adminAction(c.removeRegistry))
	mux.HandleFunc("GET /setup", c.setupPage)
	mux.HandleFunc("POST /setup", c.post(c.setup))
	mux.HandleFunc("GET /login", c.loginPage)
	mux.HandleFunc("POST /login", c.post(c.login))
	mux.HandleFunc("GET /login/sso", c.ssoBegin)
	mux.HandleFunc("GET "+sso.CallbackPath, c.ssoCallback)
	mux.HandleFunc("GET /settings/system/configuration", c.downloadConfiguration)
	mux.HandleFunc("GET /settings/sign-in", c.signInSettings)
	mux.HandleFunc("POST /settings/sign-in", c.adminAction(c.configureSignIn))
	mux.HandleFunc("POST /settings/sign-in/off", c.adminAction(c.turnOffSignIn))
	mux.HandleFunc("GET /login/code", c.codePage)
	mux.HandleFunc("POST /login/code", c.post(c.code))
	mux.HandleFunc("POST /logout", c.post(c.logout))
	mux.HandleFunc("GET /invite", c.invitePage)
	mux.HandleFunc("POST /invite", c.post(c.invite))
	mux.HandleFunc("GET /device", c.devicePage)
	mux.HandleFunc("POST /device", c.post(c.device))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "Not found", Message: "There is nothing at this address.", Link: "/", LinkText: "Back to the start"})
	})
	return c.trustedSignIn(mux), nil
}

// data is what a page shows.
type data struct {
	Title, Heading, Message string
	Link, LinkText          string
	Error                   string
	// User is who is signed in.
	User string
	// CSRF is the session's token, for forms that change anything.
	CSRF      string
	Unclaimed bool
	// A form's values, kept when it is refused. Passwords are not kept.
	Email, Name, Next string
	Code              string
	Client, ClientIP  string

	Version string
	Icon    template.HTML
	Favicon template.URL

	// The console's frame: which page this is, and who may see what.
	Page    string
	Admin   bool
	Palette []paletteItem
	// Stream is where this page follows events from, if not the default.
	Stream string
	// Crumbs lead back from this page, and Tab is which of an app's pages it is.
	Crumbs []crumb
	Tab    string
	// RefreshOn is the app whose events make this page draw its live parts again;
	// "*" is any app. Poll also does so every few seconds, for what has no events:
	// the stages of a deployment. Noscript has a page without scripts reload.
	RefreshOn string
	Poll      bool
	Noscript  bool
	// Notice says that what was asked is done.
	Notice string
	// Failure says why what was asked was not done.
	Failure *failure

	// The apps list.
	Apps []appRow

	// An app, its deployments, and one deployment.
	App         appView
	Deployments []deploymentRow
	Deployment  deploymentView

	// An app's logs and variables.
	Logs       logsView
	TimeRanges []struct{ Value, Label string }
	Variables  varsView

	// One's own account, the administrator's pages, and projects.
	Account  accountView
	Settings settingsView
	Projects []projectRow
	Project  projectView
	// Notifications is the pages of notifications.
	Notifications notificationsView
	// Backups is the page of backups.
	Backups backupsView
	// SignIn is the page of how people sign in, and SSOLabel and Passwords what the sign-in page offers.
	SignIn    signInView
	SSOLabel  string
	Passwords bool
	// Unread is how many notifications they have not read.
	Unread int

	// Access says what the viewer may do with the app the page is about. Env is the
	// environment in its addresses, empty for production; EnvNames are the app's
	// environments, EnvInfo that one, and EnvRows a list of them.
	Access   auth.Access
	Env      string
	EnvNames []string
	EnvInfo  store.Environment
	EnvRows  []environmentRow
	// Gate says who may reach it, on its Access page.
	Gate *accessView
	// Metrics is an environment's charts, and Glance how it has been doing lately.
	Metrics *metricsView
	Glance  []glanceTile
	// Cron is an environment's cron jobs and runs, and Run one of those.
	Cron *cronView
	Run  *runView
	// Usage says how much room it takes.
	Usage deploy.AppUsage
	// Forge is the repository it deploys from, if the server does that.
	Forge *forgeView
	// Files is what is in its data directory, and LineConsole where commands are run.
	Files       *filesView
	LineConsole *lineConsoleView
	// Telemetry is where things are sent, and Thresholds when someone is told.
	Telemetry  *telemetryView
	Thresholds *thresholdsView
	// ThresholdFailure says what was wrong with one just asked for.
	ThresholdFailure string

	// Deploying an app.
	Image                 string
	Commands, GitCommands []string
	MaxUpload             string
	MaxUploadBytes        int64

	// The timeline.
	Events    []eventRow
	AppNames  []string
	FilterApp string
	Older     string
	Live      bool

	// Getting started.
	Checks                                    []doctor.Check
	ChecksPassed                              bool
	InstallUnix, InstallWindows, LoginCommand string
	AppPattern                                string
}

// render sends a page.
func (c *Console) render(w http.ResponseWriter, status int, page string, d data) {
	d.Version, d.Icon, d.Favicon = buildinfo.Version, brand.Icon(), brand.FaviconURL()
	pages := c.pages
	if c.dev {
		var err error
		if pages, err = parseTemplates(c.source, c.assetURL); err != nil {
			c.log.Error("parse the templates", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	layout := "shell"
	if cards[page] {
		layout = "card"
	}
	var buf bytes.Buffer
	if err := pages[page].ExecuteTemplate(&buf, layout, d); err != nil {
		c.log.Error("render a page", "page", page, "err", err)
		http.Error(w, "The page could not be shown.", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("X-Content-Type-Options", "nosniff")
	// The one-time codes in these pages' addresses must not leave in a Referer.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (c *Console) message(w http.ResponseWriter, status int, d data) {
	c.render(w, status, "message", d)
}

func (c *Console) internal(w http.ResponseWriter, err error) {
	c.log.Error("a page failed", "err", err)
	c.message(w, http.StatusInternalServerError, data{Title: "Something went wrong", Heading: "Something went wrong", Message: "The server could not complete the request; its log has the details."})
}

// post guards a form's handler: the request must come from the console's own
// pages, and its form is read with a limit.
func (c *Console) post(next http.HandlerFunc) http.HandlerFunc {
	return c.postUpTo(maxForm, next)
}

// A form is read up to this much; one of variables may hold many, and long ones.
const (
	maxForm     = 64 << 10
	maxVarsForm = 8 << 20
)

// postUpTo is post, with a limit of its own.
func (c *Console) postUpTo(limit int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.SameOrigin(r) {
			c.auth.Security("a form was posted from another site", "ip", c.auth.ClientIP(r), "path", r.URL.Path)
			c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "That request was refused", Message: "It did not come from this site's own pages.", Link: "/", LinkText: "Back to the start"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if err := r.ParseForm(); err != nil {
			c.message(w, http.StatusBadRequest, data{Title: "Refused", Heading: "That request was refused", Message: "The form could not be read."})
			return
		}
		next(w, r)
	}
}

// signedIn returns who the request's session is, if any.
func (c *Console) signedIn(r *http.Request) (auth.Principal, bool) {
	cookie, err := r.Cookie(c.auth.CookieName())
	if err != nil {
		return auth.Principal{}, false
	}
	p, err := c.auth.Session(r.Context(), cookie.Value)
	return p, err == nil
}

func (c *Console) startSession(w http.ResponseWriter, r *http.Request, u store.User) bool {
	value, err := c.auth.StartSession(r.Context(), u, c.auth.ClientIP(r), r.UserAgent())
	if err != nil {
		c.internal(w, err)
		return false
	}
	http.SetCookie(w, c.auth.SessionCookie(value))
	return true
}

// failed shows why a form was refused, on its own page.
func (c *Console) failed(w http.ResponseWriter, page string, d data, err error) {
	var limited *auth.RateLimitedError
	var field *auth.FieldError
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.Wait.Seconds())+1))
		d.Error = "Too many attempts. Try again in a moment."
		c.render(w, http.StatusTooManyRequests, page, d)
	case errors.As(err, &field):
		d.Error = sentence(field.Detail)
		c.render(w, http.StatusUnprocessableEntity, page, d)
	case errors.Is(err, auth.ErrBadCredentials):
		d.Error = "The email address or password is wrong."
		c.render(w, http.StatusUnauthorized, page, d)
	case errors.Is(err, auth.ErrPasswordsOff):
		d.Error = "This server signs people in through its provider, and not with passwords."
		c.render(w, http.StatusForbidden, page, d)
	case errors.Is(err, auth.ErrSecondFactor):
		d.Error = "That code is not right. Each one works once."
		c.render(w, http.StatusUnauthorized, page, d)
	default:
		c.internal(w, err)
	}
}

// sentence capitalises an error's text and ends it with a full stop.
func sentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

func (c *Console) home(w http.ResponseWriter, r *http.Request) {
	if p, ok := c.signedIn(r); ok {
		c.appsPage(w, r, p)
		return
	}
	claimed, err := c.auth.Claimed(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	if !claimed {
		c.render(w, http.StatusOK, "unclaimed", data{Title: "Moorage"})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func badClaim() data {
	return data{Title: "Claim this server", Heading: "That setup link is not valid", Message: "Open the link that moor server status shows on the server."}
}

// claimRefused answers a claim whose code is wrong, or that is rate limited.
func (c *Console) claimRefused(w http.ResponseWriter, err error) {
	var limited *auth.RateLimitedError
	if errors.As(err, &limited) {
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.Wait.Seconds())+1))
		c.message(w, http.StatusTooManyRequests, data{Title: "Claim this server", Heading: "Too many attempts", Message: "Try again in a moment."})
		return
	}
	c.message(w, http.StatusForbidden, badClaim())
}

func (c *Console) setupPage(w http.ResponseWriter, r *http.Request) {
	claimed, err := c.auth.Claimed(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	if claimed {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	code := r.URL.Query().Get("code")
	if err := c.auth.CheckClaimCode(code, c.auth.ClientIP(r)); err != nil {
		c.claimRefused(w, err)
		return
	}
	c.render(w, http.StatusOK, "setup", data{Title: "Claim this server", Code: code})
}

func (c *Console) setup(w http.ResponseWriter, r *http.Request) {
	code, email, name, password := r.PostFormValue("code"), r.PostFormValue("email"), r.PostFormValue("name"), r.PostFormValue("password")
	d := data{Title: "Claim this server", Code: code, Email: email, Name: name}
	ip := c.auth.ClientIP(r)
	if err := c.auth.CheckClaimCode(code, ip); err != nil {
		c.claimRefused(w, err)
		return
	}
	if password != r.PostFormValue("confirm") {
		d.Error = "The two passwords differ."
		c.render(w, http.StatusUnprocessableEntity, "setup", d)
		return
	}
	u, err := c.auth.Claim(r.Context(), code, ip, email, name, password)
	switch {
	case errors.Is(err, auth.ErrBadClaimCode):
		c.claimRefused(w, err)
		return
	case errors.Is(err, store.ErrAlreadyClaimed):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case err != nil:
		c.failed(w, "setup", d, err)
		return
	}
	if c.startSession(w, r, u) {
		http.Redirect(w, r, "/welcome", http.StatusSeeOther)
	}
}

// safeNext keeps a redirect after signing in on this site.
func safeNext(next string) string {
	if !ingress.SafeLocalPath(next) {
		return "/"
	}
	return next
}

func (c *Console) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if _, ok := c.signedIn(r); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	claimed, err := c.auth.Claimed(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	if !claimed { // there is no account to sign in with: the start page says why
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	d := data{Title: "Sign in", Next: next, Passwords: true}
	if c.sso != nil {
		d.SSOLabel, d.Passwords = c.sso.Button(r.Context())
	}
	c.render(w, http.StatusOK, "login", d)
}

func (c *Console) login(w http.ResponseWriter, r *http.Request) {
	email, next := r.PostFormValue("email"), safeNext(r.PostFormValue("next"))
	u, err := c.auth.SignIn(r.Context(), email, r.PostFormValue("password"), c.auth.ClientIP(r))
	if err != nil {
		d := data{Title: "Sign in", Email: email, Next: next, Passwords: true}
		if c.sso != nil {
			d.SSOLabel, d.Passwords = c.sso.Button(r.Context())
		}
		c.failed(w, "login", d, err)
		return
	}
	needed, err := c.auth.NeedsSecondFactor(r.Context(), u.ID)
	if err != nil {
		c.internal(w, err)
		return
	}
	if needed {
		http.SetCookie(w, c.pendingCookie(c.auth.PendingSecondFactor(u), secondFactorMaxAge))
		http.Redirect(w, r, "/login/code?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	if c.startSession(w, r, u) {
		http.Redirect(w, r, next, http.StatusSeeOther)
	}
}

const secondFactorMaxAge = 300

// pendingCookie is what a browser holds between a password and a code.
func (c *Console) pendingCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: "moorage_pending", Value: value, Path: "/login", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: c.auth.Secure}
}

func (c *Console) codePage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	cookie, err := r.Cookie("moorage_pending")
	if err == nil {
		if _, err = c.auth.PendingUser(r.Context(), cookie.Value); err == nil {
			c.render(w, http.StatusOK, "login-code", data{Title: "Sign in", Next: next})
			return
		}
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
}

func (c *Console) code(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.PostFormValue("next"))
	cookie, err := r.Cookie("moorage_pending")
	if err != nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	u, err := c.auth.PendingUser(r.Context(), cookie.Value)
	if err != nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	if err := c.auth.CheckSecondFactor(r.Context(), u, r.PostFormValue("code"), c.auth.ClientIP(r)); err != nil {
		c.failed(w, "login-code", data{Title: "Sign in", Next: next}, err)
		return
	}
	http.SetCookie(w, c.pendingCookie("", -1))
	if c.startSession(w, r, u) {
		http.Redirect(w, r, next, http.StatusSeeOther)
	}
}

func (c *Console) logout(w http.ResponseWriter, r *http.Request) {
	if p, ok := c.signedIn(r); ok {
		if !auth.CheckCSRF(p.CookieValue, r.PostFormValue("csrf")) {
			c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "That request was refused", Message: "It did not come from this site's own pages.", Link: "/", LinkText: "Back to the start"})
			return
		}
		if err := c.auth.EndSession(r.Context(), p.CookieValue); err != nil {
			c.internal(w, err)
			return
		}
	}
	http.SetCookie(w, c.auth.SessionCookie(""))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func badInvitation() data {
	return data{Title: "Choose a password", Heading: "That link is not valid", Message: "It has been used, or it has expired. Ask an administrator for a new one."}
}

func (c *Console) invitePage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	u, err := c.db.InvitedUser(r.Context(), auth.Hex(code))
	if errors.Is(err, store.ErrNotFound) {
		c.message(w, http.StatusNotFound, badInvitation())
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	c.render(w, http.StatusOK, "invite", data{Title: "Choose a password", Code: code, Email: u.Email})
}

func (c *Console) invite(w http.ResponseWriter, r *http.Request) {
	code, password := r.PostFormValue("code"), r.PostFormValue("password")
	invited, err := c.db.InvitedUser(r.Context(), auth.Hex(code))
	if errors.Is(err, store.ErrNotFound) {
		c.message(w, http.StatusNotFound, badInvitation())
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	d := data{Title: "Choose a password", Code: code, Email: invited.Email}
	if password != r.PostFormValue("confirm") {
		d.Error = "The two passwords differ."
		c.render(w, http.StatusUnprocessableEntity, "invite", d)
		return
	}
	u, err := c.auth.AcceptInvitation(r.Context(), code, password)
	if errors.Is(err, store.ErrNotFound) {
		c.message(w, http.StatusNotFound, badInvitation())
		return
	}
	if err != nil {
		c.failed(w, "invite", d, err)
		return
	}
	// A link replaces a password, and not a second factor.
	if needed, err := c.auth.NeedsSecondFactor(r.Context(), u.ID); err != nil || needed {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if u.Disabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if c.startSession(w, r, u) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// normalizeUserCode tolerates a code typed in lower case, or without its dash.
func normalizeUserCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) == 8 && !strings.Contains(code, "-") {
		code = code[:4] + "-" + code[4:]
	}
	return code
}

func (c *Console) devicePage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.signedIn(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	code := normalizeUserCode(r.URL.Query().Get("code"))
	if code == "" {
		c.render(w, http.StatusOK, "device-enter", data{Title: "Approve a sign-in"})
		return
	}
	dev, err := c.auth.FindDevice(r.Context(), code, p.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.render(w, http.StatusNotFound, "device-enter", data{Title: "Approve a sign-in", Error: "That code is not valid, or it has expired. Check your terminal."})
			return
		}
		c.failed(w, "device-enter", data{Title: "Approve a sign-in"}, err)
		return
	}
	c.render(w, http.StatusOK, "device-confirm", data{
		Title: "Approve a sign-in", User: p.Name, CSRF: auth.CSRFToken(p.CookieValue), Code: dev.UserCode, Client: dev.Client, ClientIP: dev.IP,
	})
}

func (c *Console) device(w http.ResponseWriter, r *http.Request) {
	p, ok := c.signedIn(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !auth.CheckCSRF(p.CookieValue, r.PostFormValue("csrf")) {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "That request was refused", Message: "It did not come from this site's own pages.", Link: "/device", LinkText: "Start again"})
		return
	}
	approve := r.PostFormValue("decision") == "approve"
	userID := ""
	if approve {
		userID = p.UserID
	}
	err := c.db.DecideDevice(r.Context(), normalizeUserCode(r.PostFormValue("code")), userID)
	if errors.Is(err, store.ErrNotFound) {
		c.render(w, http.StatusNotFound, "device-enter", data{Title: "Approve a sign-in", Error: "That code is not valid, or it has expired. Check your terminal."})
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	if approve {
		c.auth.Security("a CLI's sign-in was approved", "user", p.Name, "ip", c.auth.ClientIP(r))
		c.message(w, http.StatusOK, data{Title: "Approved", Heading: "Signed in", Message: "You can close this page and go back to your terminal."})
		return
	}
	c.message(w, http.StatusOK, data{Title: "Denied", Heading: "Denied", Message: "The sign-in was refused. You can close this page."})
}
