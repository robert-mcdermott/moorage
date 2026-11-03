package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
)

// permission is what a route requires of its caller. Authorisation is enforced
// here only, and every route declares one (spec/08-security.md, "Authorisation").
type permission int

const (
	// public routes need no credentials.
	public permission = iota + 1
	// signedIn routes need valid credentials, of any scope.
	signedIn
	// canRead routes need the read scope: seeing apps, deployments and logs.
	canRead
	// canDeploy routes need the deploy scope: deploying, restarting, rolling
	// back, and seeing variables.
	canDeploy
	// canManage routes need the admin scope: changing variables, and managing
	// one's own tokens, keys and sessions.
	canManage
	// serverAdmin routes need the admin scope and the admin role: managing users.
	serverAdmin
)

// String is the permission's name in the OpenAPI document.
func (p permission) String() string {
	switch p {
	case public:
		return "public"
	case signedIn:
		return "signed-in"
	case canRead:
		return "read"
	case canDeploy:
		return "deploy"
	case canManage:
		return "admin"
	case serverAdmin:
		return "server-admin"
	}
	return "unset"
}

// route is one API endpoint: what it needs, and what the OpenAPI document says
// of it.
type route struct {
	method, path string
	perm         permission
	handler      http.HandlerFunc
	summary      string
	// request and response are prototypes of the JSON documents, if any.
	request, response any
	// status is the status of success (200 if unset).
	status int
	// stream, if set, is a prototype of the lines of an NDJSON response.
	stream any
	// multipart marks a deploy's multipart/form-data request.
	multipart bool
	// query lists the query parameters, with what each one does.
	query map[string]string
}

var pageQuery = map[string]string{
	"limit":  "How many items to return (at most 200).",
	"cursor": "The next_cursor of the previous page.",
}

// routes is every endpoint of the API.
func (s *Server) routes() []route {
	return []route{
		{method: "GET", path: "/openapi.json", perm: public, handler: s.openapi, summary: "This API's OpenAPI document"},

		// Signing in.
		{method: "POST", path: "/auth/login", perm: public, handler: s.login, summary: "Sign in with an email address and password, starting a session",
			request: apitypes.LoginRequest{}, response: apitypes.LoginResult{}},
		{method: "POST", path: "/auth/logout", perm: signedIn, handler: s.logout, status: http.StatusNoContent,
			summary: "End the session, or revoke the token, in use"},
		{method: "POST", path: "/auth/device", perm: public, handler: s.deviceStart, summary: "Begin a CLI's sign-in, to approve in a browser",
			request: apitypes.DeviceRequest{}, response: apitypes.DeviceAuthorization{}},
		{method: "POST", path: "/auth/device/token", perm: public, handler: s.deviceToken, summary: "Ask whether a CLI's sign-in has been approved, and get its token",
			request: apitypes.DeviceTokenRequest{}, response: apitypes.DeviceToken{}},

		// The caller.
		{method: "GET", path: "/me", perm: signedIn, handler: s.me, summary: "Who the caller is", response: apitypes.Me{}},
		{method: "GET", path: "/me/sessions", perm: canManage, handler: s.sessions, summary: "List your sessions", response: []apitypes.Session{}},
		{method: "DELETE", path: "/me/sessions/{session}", perm: canManage, handler: s.endSession, status: http.StatusNoContent, summary: "End one of your sessions"},
		{method: "GET", path: "/me/tokens", perm: canManage, handler: s.tokens, summary: "List your API tokens", response: []apitypes.Token{}},
		{method: "POST", path: "/me/tokens", perm: canManage, handler: s.createToken, status: http.StatusCreated, summary: "Create an API token; its secret is shown this once",
			request: apitypes.NewToken{}, response: apitypes.CreatedToken{}},
		{method: "DELETE", path: "/me/tokens/{token}", perm: canManage, handler: s.revokeToken, summary: "Revoke one of your API tokens, by name or ID", response: apitypes.Token{}},
		{method: "GET", path: "/me/ssh-keys", perm: canManage, handler: s.sshKeys, summary: "List your SSH keys", response: []apitypes.SSHKey{}},
		{method: "POST", path: "/me/ssh-keys", perm: canManage, handler: s.addSSHKey, status: http.StatusCreated, summary: "Register an SSH key",
			request: apitypes.NewSSHKey{}, response: apitypes.SSHKey{}},
		{method: "DELETE", path: "/me/ssh-keys/{key}", perm: canManage, handler: s.removeSSHKey, summary: "Remove one of your SSH keys, by name or fingerprint", response: apitypes.SSHKey{}},

		// Users.
		{method: "GET", path: "/users", perm: serverAdmin, handler: s.users, summary: "List the users", response: apitypes.Page[apitypes.User]{}, query: pageQuery},
		{method: "POST", path: "/users", perm: serverAdmin, handler: s.createUser, status: http.StatusCreated, summary: "Create a user, with an invitation to choose a password",
			request: apitypes.NewUser{}, response: apitypes.Invitation{}},
		{method: "PATCH", path: "/users/{user}", perm: serverAdmin, handler: s.changeUser, summary: "Change a user's role, or disable or enable them",
			request: apitypes.UserChange{}, response: apitypes.User{}},
		{method: "POST", path: "/users/{user}/invitation", perm: serverAdmin, handler: s.inviteUser, status: http.StatusCreated,
			summary: "Make a new one-time link for a user to choose a password", response: apitypes.Invitation{}},

		// Container registries.
		{method: "GET", path: "/registries", perm: serverAdmin, handler: s.registries, summary: "List the container registries the server has credentials for", response: []apitypes.Registry{}},
		{method: "PUT", path: "/registries/{host}", perm: serverAdmin, handler: s.setRegistry, summary: "Store the credentials of a container registry",
			request: apitypes.RegistryLogin{}, response: apitypes.Registry{}},
		{method: "DELETE", path: "/registries/{host}", perm: serverAdmin, handler: s.deleteRegistry, status: http.StatusNoContent, summary: "Forget the credentials of a container registry"},

		// The server and its apps.
		{method: "GET", path: "/server", perm: signedIn, handler: s.serverInfo, summary: "The server's version and the prefixes of the apps' git remotes", response: apitypes.ServerInfo{}},
		{method: "GET", path: "/apps", perm: canRead, handler: s.listApps, summary: "List the apps", response: apitypes.Page[apitypes.App]{}, query: pageQuery},
		{method: "POST", path: "/apps/{app}/envs/{env}/deployments", perm: canDeploy, handler: s.deploy, status: http.StatusCreated, multipart: true,
			summary: "Deploy uploaded files, creating the app if it does not exist", response: apitypes.Deployment{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/deployments", perm: canRead, handler: s.deployments, summary: "List an environment's deployments, newest first",
			response: apitypes.Page[apitypes.Deployment]{}, query: pageQuery},
		{method: "GET", path: "/deployments/{id}", perm: canRead, handler: s.deployment, summary: "Get a deployment", response: apitypes.Deployment{}},
		{method: "GET", path: "/deployments/{id}/log", perm: canRead, handler: s.deploymentLog, summary: "Stream a deployment's log", stream: apitypes.LogLine{},
			query: map[string]string{"follow": "true keeps the stream open until the deployment ends."}},
		{method: "GET", path: "/apps/{app}/envs/{env}/releases", perm: canRead, handler: s.releases, summary: "List an environment's releases, newest first",
			response: apitypes.Page[apitypes.Release]{}, query: pageQuery},
		{method: "POST", path: "/apps/{app}/envs/{env}/rollback", perm: canDeploy, handler: s.rollback, summary: "Serve an earlier release again",
			request: apitypes.RollbackRequest{}, response: apitypes.Deployment{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/restart", perm: canDeploy, handler: s.restart, summary: "Replace an environment's instances", response: []apitypes.Instance{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/instances", perm: canRead, handler: s.instances, summary: "List an environment's instances", response: []apitypes.Instance{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/vars", perm: canDeploy, handler: s.vars, summary: "List an environment's variables; a secret's value is never shown", response: []apitypes.Var{}},
		{method: "PATCH", path: "/apps/{app}/envs/{env}/vars", perm: canManage, handler: s.changeVars, summary: "Set and unset variables, released at once unless no_restart is set",
			request: apitypes.VarsChange{}, response: apitypes.VarsResult{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/logs", perm: canRead, handler: s.logs, summary: "Stream an environment's logs", stream: apitypes.LogLine{},
			query: map[string]string{
				"follow":  "true keeps the stream open.",
				"tail":    "How many of the latest lines to start with.",
				"process": "Only this process's lines.",
			}},
	}
}

type principalKey struct{}

// who returns the caller of a guarded route.
func who(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return p
}

// guard authenticates a route's requests and checks its permission.
func (s *Server) guard(rt route) http.HandlerFunc {
	if rt.perm < public || rt.perm > serverAdmin {
		panic(fmt.Sprintf("the route %s %s declares no permission", rt.method, rt.path))
	}
	if rt.perm == public {
		return rt.handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Request(r)
		switch {
		case errors.Is(err, auth.ErrLocalOnly):
			unauthorized(w, "local_only", err.Error())
			return
		case errors.Is(err, auth.ErrUnauthenticated):
			unauthorized(w, "unauthorized", "A valid token is required (Authorization: Bearer <token>).")
			return
		case err != nil:
			s.internal(w, err)
			return
		}
		// A browser's session cookie is sent whatever the page: a request that
		// changes anything must prove it came from the console.
		if p.SessionID != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !auth.SameOrigin(r) || !auth.CheckCSRF(p.CookieValue, r.Header.Get("X-CSRF-Token")) {
				problem(w, http.StatusForbidden, "csrf", "A request signed in with a session cookie needs its CSRF token (X-CSRF-Token), from the console's own pages.")
				return
			}
		}
		if msg := refused(rt, p, r.PathValue("app")); msg != "" {
			problem(w, http.StatusForbidden, "forbidden", msg)
			return
		}
		rt.handler(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

// refused says why a caller may not use a route, or "".
func refused(rt route, p auth.Principal, app string) string {
	switch rt.perm {
	case canRead:
		if !p.Can(auth.ScopeRead) {
			return "This token's scope does not allow reading."
		}
	case canDeploy:
		if !p.Can(auth.ScopeDeploy) {
			return "This token's scope does not allow deploying: it needs the deploy scope."
		}
	case canManage, serverAdmin:
		if !p.Can(auth.ScopeAdmin) {
			return "This token's scope does not allow this: it needs the admin scope."
		}
		if rt.perm == serverAdmin && !p.Admin {
			return "Only an admin can do this."
		}
		// A token for one app has no business with the account's credentials.
		if p.App != "" && app == "" {
			return "A token restricted to an app cannot do this."
		}
	}
	if app != "" && !p.CanApp(app) {
		return "This token is restricted to the app " + p.App + "."
	}
	return ""
}

func unauthorized(w http.ResponseWriter, code, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="moorage"`)
	problem(w, http.StatusUnauthorized, code, detail)
}
