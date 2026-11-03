// Package apitypes defines the JSON documents of Moorage's REST API, shared
// by the server and the CLI.
package apitypes

import "time"

// App is an app and its production environment.
type App struct {
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"created_at"`
	Production *Release  `json:"production,omitempty"`
}

// Release is one numbered release of an environment.
type Release struct {
	Version   int       `json:"version"`
	Build     string    `json:"build"`
	Reason    string    `json:"reason"`
	Files     int       `json:"files"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	Current   bool      `json:"current"`
}

// Deployment is the job that produces a release: a deploy, a rollback. A
// finished deployment that went live carries the release now serving.
type Deployment struct {
	ID     string `json:"id"`
	App    string `json:"app"`
	Env    string `json:"env"`
	URL    string `json:"url"`
	Source string `json:"source"`
	// Ref, Commit and Message say what a push deployed.
	Ref        string    `json:"ref,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	Message    string    `json:"message,omitempty"`
	Status     string    `json:"status"`
	Build      string    `json:"build,omitempty"`
	AppCreated bool      `json:"app_created,omitempty"`
	Release    *Release  `json:"release,omitempty"`
	Error      string    `json:"error,omitempty"`
	Output     []string  `json:"output,omitempty"`
	CreatedBy  string    `json:"created_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Deployment statuses: queued, building, releasing and rolling_out while it
// runs; live, failed, superseded or canceled once it has finished.
const (
	StatusLive   = "live"
	StatusFailed = "failed"
)

// Finished reports whether the deployment has ended.
func (d Deployment) Finished() bool {
	switch d.Status {
	case "live", "failed", "superseded", "canceled":
		return true
	}
	return false
}

// RollbackRequest asks for an earlier release to be published again; with
// Version 0, the release before the current one. The new release runs
// with the environment's current variables, or with WithConfig, with
// those the earlier release ran with.
type RollbackRequest struct {
	Version    int  `json:"version,omitempty"`
	WithConfig bool `json:"with_config,omitempty"`
}

// Var is a variable of an environment. A secret's value is never shown.
type Var struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Secret bool   `json:"secret,omitempty"`
	Build  bool   `json:"build,omitempty"`
	// Source is "env" (set on the server for this environment), "app"
	// (set on the server for every environment) or "manifest" ([env] of
	// the manifest of the current release).
	Source string `json:"source"`
	// Overridden reports a variable that another source sets too, and wins.
	Overridden bool      `json:"overridden,omitempty"`
	UpdatedBy  string    `json:"updated_by,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
}

// VarsChange sets and unsets variables of an environment (or, with
// AllEnvs, of every environment of the app). Unless NoRestart is set, the
// change is released at once: a new release of the current build.
type VarsChange struct {
	Set       []VarSet `json:"set,omitempty"`
	Unset     []string `json:"unset,omitempty"`
	AllEnvs   bool     `json:"all_envs,omitempty"`
	NoRestart bool     `json:"no_restart,omitempty"`
}

// VarSet is one variable to set.
type VarSet struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
	Build  bool   `json:"build,omitempty"`
}

// VarsResult says what a change of variables did.
type VarsResult struct {
	App        string   `json:"app"`
	Env        string   `json:"env"`
	AppCreated bool     `json:"app_created,omitempty"`
	NotSet     []string `json:"not_set,omitempty"`
	// Deployment is the deployment that released the change, if any.
	Deployment *Deployment `json:"deployment,omitempty"`
}

// SSHKey is a public key that may push over SSH.
type SSHKey struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	PublicKey   string    `json:"public_key"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at,omitzero"`
}

// NewSSHKey registers a public key, in authorized_keys format. Name
// defaults to the key's comment.
type NewSSHKey struct {
	Name      string `json:"name,omitempty"`
	PublicKey string `json:"public_key"`
}

// ServerInfo describes the server.
type ServerInfo struct {
	Version string  `json:"version"`
	Git     GitInfo `json:"git"`
}

// GitInfo gives the prefixes of the apps' git remotes: an app's remote is
// <prefix>/<app>.git. SSH is empty when pushes over SSH are off.
type GitInfo struct {
	SSH  string `json:"ssh,omitempty"`
	HTTP string `json:"http"`
}

// Page is one page of a list. NextCursor, when set, is passed back as
// ?cursor=… for the next page.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// User is an account.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	// Role is "admin" or "member".
	Role      string    `json:"role"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Me says who the caller is. Local marks the server's local administrator, who
// holds the admin token on the server's machine and has no account.
type Me struct {
	ID    string `json:"id,omitempty"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Local bool   `json:"local,omitempty"`
	// Scopes and App are the limits of the token in use, if any.
	Scopes []string `json:"scopes,omitempty"`
	App    string   `json:"app,omitempty"`
}

// LoginRequest signs in with an email address and password.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResult is a new session: the session cookie is set, and
// cookie-authenticated requests that change anything send CSRFToken in the
// X-CSRF-Token header.
type LoginResult struct {
	User      User   `json:"user"`
	CSRFToken string `json:"csrf_token"`
}

// DeviceRequest begins a CLI's sign-in. Client names it for the person
// approving it.
type DeviceRequest struct {
	Client string `json:"client"`
}

// DeviceAuthorization is a sign-in waiting for approval in a browser: the CLI
// shows UserCode and opens VerificationURLComplete, and polls with DeviceCode
// every Interval seconds.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURL         string `json:"verification_url"`
	VerificationURLComplete string `json:"verification_url_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceTokenRequest asks whether a sign-in has been approved. Until it is,
// the answer is a problem with the code authorization_pending or slow_down;
// access_denied and expired_token end it.
type DeviceTokenRequest struct {
	DeviceCode string `json:"device_code"`
}

// DeviceToken is an approved sign-in's API token, given once.
type DeviceToken struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

// Session is a browser's sign-in.
type Session struct {
	ID         string    `json:"id"`
	Current    bool      `json:"current,omitempty"`
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Token is an API token, without its secret.
type Token struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Scopes     []string  `json:"scopes"`
	App        string    `json:"app,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	CreatedAt  time.Time `json:"created_at"`
}

// NewToken creates an API token. Scopes are read, deploy, admin and gate; App,
// if set, restricts it to one app.
type NewToken struct {
	Name      string    `json:"name"`
	Scopes    []string  `json:"scopes"`
	App       string    `json:"app,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// CreatedToken is a new token, with its secret, shown this once.
type CreatedToken struct {
	Token
	Secret string `json:"token"`
}

// NewUser creates an account, with an invitation to choose a password. Role is
// "member" unless it says "admin".
type NewUser struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"`
}

// UserChange changes an account; fields left out stay as they are.
type UserChange struct {
	Role     *string `json:"role,omitempty"`
	Disabled *bool   `json:"disabled,omitempty"`
}

// Invitation is a one-time link for a user to choose a password.
type Invitation struct {
	User      User      `json:"user"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// FieldProblem says what is wrong with one field of a request.
type FieldProblem struct {
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// Problem is an RFC 9457 error document, with a stable Code. Errors, when
// present, say what is wrong field by field; Output holds the last lines of a
// failing instance's log.
type Problem struct {
	Type   string         `json:"type,omitempty"`
	Title  string         `json:"title"`
	Status int            `json:"status"`
	Detail string         `json:"detail,omitempty"`
	Code   string         `json:"code,omitempty"`
	Errors []FieldProblem `json:"errors,omitempty"`
	Output []string       `json:"output,omitempty"`
}

// Instance is one running copy of a process.
type Instance struct {
	ID        string    `json:"id"`
	Process   string    `json:"process"`
	Index     int       `json:"index"`
	Release   int       `json:"release"`
	Port      int       `json:"port,omitempty"`
	Status    string    `json:"status"`
	Healthy   bool      `json:"healthy"`
	PID       int       `json:"pid,omitempty"`
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"started_at,omitempty"`
	LastExit  *Exit     `json:"last_exit,omitempty"`
}

// Exit is how an instance's app last ended.
type Exit struct {
	Code   int       `json:"code"`
	Signal string    `json:"signal,omitempty"`
	At     time.Time `json:"at"`
}

// LogLine is one line of an app's log. Stream is "O" (stdout), "E"
// (stderr) or "M" (a note from Moorage).
type LogLine struct {
	Time     time.Time `json:"time"`
	Instance string    `json:"instance"`
	Process  string    `json:"process"`
	Index    int       `json:"index"`
	Stream   string    `json:"stream"`
	Text     string    `json:"text"`
}

// Content types and form fields.
const (
	ProblemType = "application/problem+json"
	NDJSONType  = "application/x-ndjson"
	// PlanPart and ArchivePart are the multipart form fields of the deploy
	// of an upload that needs no build: the plan (manifest.Plan) as JSON,
	// then the gzip-compressed tar. SourcePart alone is source for the
	// server to build.
	PlanPart    = "plan"
	ArchivePart = "archive"
	SourcePart  = "source"
	// ImagePart, before a source part, is an image to run instead of building
	// the source, which then holds no more than the app's manifest.
	ImagePart = "image"
)

// Registry is a container registry the server has credentials for.
type Registry struct {
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// RegistryLogin is a container registry's credentials.
type RegistryLogin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
