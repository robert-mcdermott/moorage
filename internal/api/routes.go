package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/store"
	"strings"
)

// permission is what a route requires of its caller. Authorisation is enforced
// here only, and every route declares one (spec/08-security.md, "Authorisation").
type permission int

const (
	// public routes need no credentials.
	public permission = iota + 1
	// signedIn routes need valid credentials, of any scope.
	signedIn
	// canRead routes need the read scope: seeing apps and deployments.
	canRead
	// canReadLogs routes need the read scope too, and a role that may read logs.
	canReadLogs
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
	case canReadLogs:
		return "logs"
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
	// socket says that it becomes a WebSocket, and stays open.
	socket bool
	// sse, if set, is a prototype of the events of a server-sent event stream.
	sse any
	// multipart marks a deploy's multipart/form-data request.
	multipart bool
	// query lists the query parameters, with what each one does.
	query map[string]string
	// oneApp says that what it answers keeps to the one app that a token may be limited to. Others are refused to such a token.
	oneApp bool
	// previews says that a token limited to previews may use it, for an environment that stands apart from production. Anything not marked
	// is refused to such a token, so that a route added later is not open to it by accident.
	previews bool
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
		{method: "POST", path: "/auth/logout", previews: true, perm: signedIn, handler: s.logout, status: http.StatusNoContent,
			summary: "End the session, or revoke the token, in use"},
		{method: "POST", path: "/auth/device", perm: public, handler: s.deviceStart, summary: "Begin a CLI's sign-in, to approve in a browser",
			request: apitypes.DeviceRequest{}, response: apitypes.DeviceAuthorization{}},
		{method: "POST", path: "/auth/device/token", perm: public, handler: s.deviceToken, summary: "Ask whether a CLI's sign-in has been approved, and get its token",
			request: apitypes.DeviceTokenRequest{}, response: apitypes.DeviceToken{}},

		// The caller.
		{method: "GET", path: "/me", previews: true, perm: signedIn, handler: s.me, summary: "Who the caller is", response: apitypes.Me{}},
		{method: "POST", path: "/me/password", perm: canManage, handler: s.changePassword, status: http.StatusNoContent,
			summary: "Change your password, which ends your other sessions", request: apitypes.PasswordChange{}},
		{method: "DELETE", path: "/me/sessions", perm: canManage, handler: s.endOtherSessions, status: http.StatusNoContent, summary: "End your sessions but the one in use"},
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

		// Projects and who did what.
		{method: "GET", path: "/projects", perm: canRead, handler: s.projects, summary: "List the projects you have a role in", response: []apitypes.Project{}},
		{method: "POST", path: "/projects", perm: serverAdmin, handler: s.createProject, status: http.StatusCreated, summary: "Create a project", request: apitypes.NewProject{}, response: apitypes.Project{}},
		{method: "GET", path: "/projects/{project}", perm: canRead, handler: s.project, summary: "Get a project, with its members", response: apitypes.Project{}},
		{method: "PATCH", path: "/projects/{project}", perm: canManage, handler: s.changeProject, summary: "Change who may do what in a project without being a member", request: apitypes.ProjectChange{}, response: apitypes.Project{}},
		{method: "DELETE", path: "/projects/{project}", perm: serverAdmin, handler: s.deleteProject, status: http.StatusNoContent, summary: "Delete a project that has no apps"},
		{method: "PUT", path: "/projects/{project}/members/{user}", perm: canManage, handler: s.setMember, summary: "Give someone a role in a project, by email address or ID", request: apitypes.MemberChange{}, response: apitypes.Project{}},
		{method: "DELETE", path: "/projects/{project}/members/{user}", perm: canManage, handler: s.removeMember, summary: "Take someone out of a project", response: apitypes.Project{}},
		{method: "GET", path: "/audit", perm: serverAdmin, handler: s.auditLog, summary: "List who did what, newest first", response: apitypes.Page[apitypes.AuditEntry]{},
			query: map[string]string{
				"limit": "How many, up to 200.", "cursor": "Where the previous page ended.",
				"actor": "Only what this person did.", "action": "Only actions with this text.", "target": "Only what was done to this.",
			}},

		// Container registries.
		{method: "GET", path: "/notifications", oneApp: true, perm: canRead, handler: s.inbox, summary: "What you have been told, newest first", response: apitypes.Inbox{}},
		{method: "POST", path: "/notifications/read", perm: canDeploy, handler: s.readInbox, status: http.StatusNoContent, summary: "Mark all you have been told as read"},
		{method: "GET", path: "/notifications/channels", oneApp: true, perm: canRead, handler: s.channels, summary: "List where notifications can go; where each one goes is for administrators", response: []apitypes.NotificationChannel{}},
		{method: "POST", path: "/notifications/channels", perm: serverAdmin, handler: s.createChannel, status: http.StatusCreated, summary: "Add a channel; the answer has what signs a webhook, which is not shown again",
			request: apitypes.NewNotificationChannel{}, response: apitypes.CreatedChannel{}},
		{method: "DELETE", path: "/notifications/channels/{channel}", perm: serverAdmin, handler: s.deleteChannel, status: http.StatusNoContent, summary: "Remove a channel, and the rules that use it"},
		{method: "POST", path: "/notifications/channels/{channel}/test", perm: serverAdmin, handler: s.testChannel, summary: "Send a channel a test, and say how it went", response: apitypes.NotificationChannel{}},
		{method: "GET", path: "/notifications/rules", oneApp: true, perm: canRead, handler: s.rules, summary: "List the rules you can see", response: []apitypes.NotificationRule{}},
		{method: "POST", path: "/notifications/rules", perm: canManage, handler: s.createRule, status: http.StatusCreated, summary: "Add a rule: of the whole server, which takes an admin, or of a project or an app, which takes a maintainer of it",
			request: apitypes.NewNotificationRule{}, response: apitypes.NotificationRule{}},
		{method: "DELETE", path: "/notifications/rules/{rule}", perm: canManage, handler: s.deleteRule, status: http.StatusNoContent, summary: "Remove a rule"},
		{method: "GET", path: "/server/sso", perm: serverAdmin, handler: s.ssoConfig, summary: "How people sign in through a provider", response: apitypes.SSOConfig{}},
		{method: "PUT", path: "/server/sso", perm: serverAdmin, handler: s.configureSSO, summary: "Set up signing in through a provider, once it has been found at its address", request: apitypes.SSOConfig{}, response: apitypes.SSOConfig{}},
		{method: "DELETE", path: "/server/sso", perm: serverAdmin, handler: s.turnOffSSO, status: http.StatusNoContent, summary: "Turn off signing in through a provider, which gives passwords back"},
		{method: "GET", path: "/server/backups", perm: serverAdmin, handler: s.backups, summary: "Where backups go, and what has been backed up", response: apitypes.Backups{}},
		{method: "POST", path: "/server/backups", perm: serverAdmin, handler: s.startBackup, status: http.StatusAccepted, summary: "Start a backup", response: apitypes.Backup{}},
		{method: "PUT", path: "/server/backups/config", perm: serverAdmin, handler: s.configureBackups, summary: "Say where backups go and how often", request: apitypes.BackupConfig{}, response: apitypes.Backups{}},
		{method: "GET", path: "/server/config", perm: serverAdmin, handler: s.exportConfig, summary: "Read the server's settings, with no secrets in them", response: apitypes.ServerConfig{}},
		{method: "POST", path: "/server/config/import", perm: serverAdmin, handler: s.importConfig, summary: "Make the server match a configuration, in what it mentions, or with dry_run say what that would do",
			request: apitypes.ServerConfig{}, response: apitypes.ConfigReport{}},
		{method: "GET", path: "/registries", perm: serverAdmin, handler: s.registries, summary: "List the container registries the server has credentials for", response: []apitypes.Registry{}},
		{method: "PUT", path: "/registries/{host}", perm: serverAdmin, handler: s.setRegistry, summary: "Store the credentials of a container registry",
			request: apitypes.RegistryLogin{}, response: apitypes.Registry{}},
		{method: "DELETE", path: "/registries/{host}", perm: serverAdmin, handler: s.deleteRegistry, status: http.StatusNoContent, summary: "Forget the credentials of a container registry"},

		// What happens.
		{method: "GET", path: "/events", oneApp: true, perm: canRead, handler: s.listEvents, summary: "List what happened, newest first",
			response: apitypes.Page[apitypes.Event]{}, query: map[string]string{
				"limit": pageQuery["limit"], "cursor": pageQuery["cursor"],
				"app": "Only what happened to this app.", "env": "Only what happened to this environment.",
			}},
		{method: "GET", path: "/events/stream", oneApp: true, perm: canRead, handler: s.streamEvents, summary: "Follow what happens, as server-sent events", sse: apitypes.Event{},
			query: map[string]string{
				"app": "Only what happens to this app.", "env": "Only what happens to this environment.",
				"after": "Start after this event's ID, as the Last-Event-ID header also does.",
			}},

		// The server and its apps.
		{method: "GET", path: "/server/doctor", perm: serverAdmin, handler: s.serverDoctor, summary: "Check the server and what it depends on, as moor server doctor does", response: []apitypes.Check{}},
		{method: "GET", path: "/server", previews: true, perm: signedIn, handler: s.serverInfo, summary: "The server's version and the prefixes of the apps' git remotes", response: apitypes.ServerInfo{}},
		{method: "POST", path: "/apps", perm: canDeploy, handler: s.createApp, status: http.StatusCreated, summary: "Create an app in a project; deploying also creates one, in the default project", request: apitypes.NewApp{}, response: apitypes.App{}},
		{method: "GET", path: "/apps/{app}", previews: true, perm: canRead, handler: s.appInfo, summary: "An app, its project, and how much room it takes", response: apitypes.AppInfo{}},
		{method: "PATCH", path: "/apps/{app}", perm: canManage, handler: s.changeApp, summary: "Move an app to another project, or rename it, which restarts it", request: apitypes.AppChange{}, response: apitypes.App{}},
		{method: "DELETE", path: "/apps/{app}", perm: canManage, handler: s.deleteApp, status: http.StatusNoContent, summary: "Delete an app and everything of it; keep_data=true keeps its data directory",
			query: map[string]string{"keep_data": "true keeps its data directory."}},
		{method: "GET", path: "/apps", previews: true, oneApp: true, perm: canRead, handler: s.listApps, summary: "List the apps", response: apitypes.Page[apitypes.App]{}, query: pageQuery},
		{method: "POST", path: "/apps/{app}/envs/{env}/deployments", previews: true, perm: canDeploy, handler: s.deploy, status: http.StatusCreated, multipart: true,
			summary: "Deploy uploaded files, creating the app if it does not exist", response: apitypes.Deployment{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/deployments", previews: true, perm: canRead, handler: s.deployments, summary: "List an environment's deployments, newest first",
			response: apitypes.Page[apitypes.Deployment]{}, query: pageQuery},
		{method: "GET", path: "/deployments/{id}", previews: true, oneApp: true, perm: canRead, handler: s.deployment, summary: "Get a deployment", response: apitypes.Deployment{}},
		{method: "GET", path: "/deployments/{id}/log", previews: true, oneApp: true, perm: canReadLogs, handler: s.deploymentLog, summary: "Stream a deployment's log", stream: apitypes.LogLine{},
			query: map[string]string{"follow": "true keeps the stream open until the deployment ends.", "skip": "Leave out this many of the first lines."}},
		{method: "GET", path: "/apps/{app}/envs/{env}/releases", previews: true, perm: canRead, handler: s.releases, summary: "List an environment's releases, newest first",
			response: apitypes.Page[apitypes.Release]{}, query: pageQuery},
		{method: "GET", path: "/apps/{app}/envs/{env}/releases/{version}", previews: true, perm: canRead, handler: s.release, summary: "A release, and, for those who can deploy, its manifest", response: apitypes.ReleaseDetail{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/releases/{version}/diff/{other}", previews: true, perm: canRead, handler: s.releaseDiff,
			summary: "What changed from one release to another; of variables, their names only, and all but whether the build changed is for those who can deploy", response: apitypes.ReleaseDiff{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/rollback", previews: true, perm: canDeploy, handler: s.rollback, summary: "Serve an earlier release again",
			request: apitypes.RollbackRequest{}, response: apitypes.Deployment{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/redeploy", previews: true, perm: canDeploy, handler: s.redeploy, status: http.StatusAccepted,
			summary: "Build what the current release was built from again, and roll it out", response: apitypes.Deployment{}},
		{method: "GET", path: "/apps/{app}/envs", previews: true, perm: canRead, handler: s.environments, summary: "List an app's environments", response: []apitypes.Environment{}},
		{method: "POST", path: "/apps/{app}/envs", perm: canDeploy, handler: s.createEnvironment, status: http.StatusCreated, summary: "Make a named environment", request: apitypes.NewEnvironment{}, response: apitypes.Environment{}},
		{method: "DELETE", path: "/apps/{app}/envs/{env}", previews: true, perm: canDeploy, handler: s.deleteEnvironment, status: http.StatusNoContent, summary: "Remove an environment that is not production, and everything of it: a preview takes a deployer, and a named environment a maintainer"},
		{method: "POST", path: "/apps/{app}/envs/{env}/promote", perm: canDeploy, handler: s.promote, summary: "Release the build another environment runs", request: apitypes.Promotion{}, response: apitypes.Deployment{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/lock", perm: canDeploy, handler: s.lock, summary: "Stop deployments to an environment", response: apitypes.Environment{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/unlock", perm: canDeploy, handler: s.unlock, summary: "Let deployments to an environment go on", response: apitypes.Environment{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/metrics", previews: true, perm: canRead, handler: s.environmentMetrics, summary: "What an environment did over a range (range= live, 1h, 24h, 7d or 30d): requests, response times, CPU and memory, with its deployments", response: apitypes.Metrics{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/cron", previews: true, perm: canRead, handler: s.cronJobs, summary: "List an environment's cron jobs, with when each next runs and how the last one went", response: []apitypes.CronJob{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/cron/{job}/run", perm: canDeploy, handler: s.runCronJob, status: http.StatusAccepted, summary: "Run a cron job now", response: apitypes.Run{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/runs", previews: true, perm: canRead, handler: s.runs, summary: "List an environment's runs, newest first; job= is those of a cron job", response: []apitypes.Run{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/runs", previews: true, perm: canDeploy, handler: s.startRun, status: http.StatusAccepted, summary: "Run a command in the current release, with its variables and data", request: apitypes.NewRun{}, response: apitypes.Run{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/files", previews: true, perm: canDeploy, handler: s.dataFiles, summary: "List a directory within an environment's data directory", query: map[string]string{"path": "Where, from the top of it."}, response: apitypes.DataListing{}},
		{method: "DELETE", path: "/apps/{app}/envs/{env}/files", previews: true, perm: canDeploy, handler: s.removeData, status: http.StatusNoContent, summary: "Remove something from a data directory", query: map[string]string{"path": "What.", "recursive": "true takes a directory with all that is in it."}},
		{method: "GET", path: "/apps/{app}/envs/{env}/files/content", previews: true, perm: canDeploy, handler: s.dataFileContent, summary: "Read a file in a data directory", query: map[string]string{"path": "Which."}},
		{method: "PUT", path: "/apps/{app}/envs/{env}/files/content", previews: true, perm: canDeploy, handler: s.putDataFile, status: http.StatusCreated, summary: "Write a file in a data directory, up to 1 GB: it appears all at once", query: map[string]string{"path": "Where.", "replace": "true takes the place of what is there."}, response: apitypes.DataFile{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/files/directory", previews: true, perm: canDeploy, handler: s.makeDataDir, status: http.StatusCreated, summary: "Make a directory in a data directory", query: map[string]string{"path": "Where."}},
		{method: "GET", path: "/apps/{app}/envs/{env}/thresholds", perm: canRead, handler: s.thresholds, summary: "List when someone is told about how an environment is doing", response: []apitypes.Threshold{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/thresholds", perm: canManage, handler: s.addThreshold, status: http.StatusCreated, summary: "Say when to tell someone about how an environment is doing", request: apitypes.NewThreshold{}, response: apitypes.Threshold{}},
		{method: "DELETE", path: "/apps/{app}/envs/{env}/thresholds/{id}", perm: canManage, handler: s.removeThreshold, status: http.StatusNoContent, summary: "Remove a threshold"},
		{method: "GET", path: "/server/telemetry", perm: serverAdmin, handler: s.telemetrySinks, summary: "List where what apps say and do is sent", response: []apitypes.TelemetrySink{}},
		{method: "POST", path: "/server/telemetry", perm: serverAdmin, handler: s.setTelemetrySink, summary: "Add somewhere to send what apps say and do, or change the one of that name", request: apitypes.NewTelemetrySink{}, response: apitypes.TelemetrySink{}},
		{method: "DELETE", path: "/server/telemetry/{name}", perm: serverAdmin, handler: s.removeTelemetrySink, status: http.StatusNoContent, summary: "Stop sending to a sink"},
		{method: "POST", path: "/server/telemetry/{name}/test", perm: serverAdmin, handler: s.testTelemetrySink, status: http.StatusNoContent, summary: "Send something to a sink now, and say how that went"},
		{method: "GET", path: "/apps/{app}/envs/{env}/terminal", previews: true, perm: canDeploy, handler: s.terminal, socket: true, summary: "Run a command with someone at the other end of it, as moor run -it does. It becomes a WebSocket, with what is typed and shown in binary messages, and TerminalMessage in text ones. This takes a token, and not a session",
			query: map[string]string{"command": "What to run.", "instance": "A running container to run it in, rather than a new one-off instance.", "timeout": "How long it may take, such as 10m.", "tty": "1 gives it a terminal. Otherwise it has pipes.", "rows": "How tall the terminal is.", "cols": "How wide the terminal is.", "term": "The kind of terminal it is."}},
		{method: "GET", path: "/apps/{app}/runs/{id}", previews: true, perm: canRead, handler: s.run, summary: "How a run went", response: apitypes.Run{}},
		{method: "POST", path: "/apps/{app}/runs/{id}/stop", previews: true, perm: canDeploy, handler: s.stopRun, summary: "Stop a run that is going on", response: apitypes.Run{}},
		{method: "GET", path: "/apps/{app}/runs/{id}/log", previews: true, perm: canReadLogs, handler: s.runLog, summary: "What a run says, as NDJSON; follow=true stays open until it ends", stream: apitypes.LogLine{}},
		{method: "GET", path: "/apps/{app}/forge", perm: canRead, handler: s.getForge, summary: "Say where an app's code is kept, on a forge", response: apitypes.ForgeConnection{}},
		{method: "PUT", path: "/apps/{app}/forge", perm: canManage, handler: s.connectForge, summary: "Connect an app to a repository on a forge: the answer alone has the secret that its webhook signs with", request: apitypes.ForgeConnect{}, response: apitypes.ForgeConnection{}},
		{method: "DELETE", path: "/apps/{app}/forge", perm: canManage, handler: s.disconnectForge, status: http.StatusNoContent, summary: "Let go of an app's repository: what it deployed stays"},
		{method: "GET", path: "/apps/{app}/forge/pulls", perm: canRead, handler: s.forgePulls, summary: "List an app's open pull requests", response: []apitypes.ForgePull{}},
		{method: "POST", path: "/apps/{app}/forge/pulls/{number}/approve", perm: canManage, handler: s.approveForgePull, status: http.StatusAccepted, summary: "Have a pull request built at the commit that someone looked at, which runs its code: this is for that one commit", request: apitypes.ForgeApproval{}, response: apitypes.ForgePull{}},
		{method: "POST", path: "/apps/{app}/forge/sync", perm: canDeploy, handler: s.syncForge, status: http.StatusAccepted, summary: "Look at an app's repository now, and deploy what has changed"},
		{method: "POST", path: "/hooks/{hook}", perm: public, handler: s.forgeHook, status: http.StatusAccepted, summary: "Where a forge sends what happens, signed with the hook's secret"},
		{method: "GET", path: "/apps/{app}/envs/{env}/domains", perm: canRead, handler: s.domains, summary: "List an environment's domains", response: []apitypes.Domain{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/domains", perm: canManage, handler: s.addDomain, status: http.StatusCreated, summary: "Attach a domain, which serves once its DNS points here", request: apitypes.NewDomain{}, response: apitypes.Domain{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/domains/verify", perm: canManage, handler: s.verifyDomain, summary: "Look now at where a domain points", request: apitypes.NewDomain{}, response: apitypes.Domain{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/domains/remove", perm: canManage, handler: s.removeDomain, status: http.StatusNoContent, summary: "Detach a domain", request: apitypes.NewDomain{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/access", perm: canRead, handler: s.access, summary: "Who may reach an environment", response: apitypes.Access{}},
		{method: "PUT", path: "/apps/{app}/envs/{env}/access", perm: canManage, handler: s.setAccess, summary: "Change who may reach an environment, at once", request: apitypes.AccessChange{}, response: apitypes.Access{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/share-links", perm: canDeploy, handler: s.shareLinks, summary: "List an environment's share links", response: []apitypes.ShareLink{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/share-links", perm: canDeploy, handler: s.createShareLink, status: http.StatusCreated, summary: "Make a link that lets someone reach an environment: the answer alone has its address", request: apitypes.NewShareLink{}, response: apitypes.ShareLink{}},
		{method: "DELETE", path: "/apps/{app}/envs/{env}/share-links/{id}", perm: canDeploy, handler: s.revokeShareLink, status: http.StatusNoContent, summary: "End a share link, and within a minute everyone it let in"},
		{method: "POST", path: "/apps/{app}/envs/{env}/sleep", previews: true, perm: canDeploy, handler: s.sleep, summary: "Stop an environment's instances until a request wakes it", response: apitypes.Environment{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/wake", previews: true, perm: canDeploy, handler: s.wake, summary: "Start an environment that is asleep, and answer when it is ready", response: apitypes.Environment{}},
		{method: "POST", path: "/apps/{app}/previews", perm: canDeploy, handler: s.createPreview, status: http.StatusAccepted, summary: "Deploy a preview of a branch that was pushed", request: apitypes.NewPreview{}, response: apitypes.Deployment{}},
		{method: "GET", path: "/previews", oneApp: true, perm: canRead, handler: s.previews, summary: "List the previews you can see, of every app", response: []apitypes.Environment{}},
		{method: "GET", path: "/apps/{app}/envs/{env}", previews: true, perm: canRead, handler: s.environment, summary: "How an environment stands: its release, formation and instances", response: apitypes.Environment{}},
		{method: "PATCH", path: "/apps/{app}/envs/{env}", perm: canDeploy, handler: s.changeEnvironment, summary: "Scale an environment: how many instances of which process run",
			request: apitypes.EnvironmentChange{}, response: apitypes.Environment{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/restart", previews: true, perm: canDeploy, handler: s.restart, summary: "Replace an environment's instances", response: []apitypes.Instance{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/services", perm: canRead, handler: s.services, summary: "List an environment's services", response: []apitypes.Service{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/services/{name}/logs", perm: canReadLogs, handler: s.serviceLogs, summary: "Read the newest of what a service has said",
			response: apitypes.ServiceLog{}, query: map[string]string{"tail": "How many lines: 100, unless it says, and at most 5,000."}},
		{method: "POST", path: "/apps/{app}/envs/{env}/services/{name}/update", perm: canManage, handler: s.updateService, summary: "Take the newest image of a service's version", response: apitypes.ServiceChange{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/services/{name}/upgrade", perm: canManage, handler: s.upgradeService, summary: "Move a service to a newer major version, with what connects to it stopped", request: apitypes.ServiceUpgrade{}, response: apitypes.ServiceChange{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/services/{name}/restore", perm: canManage, handler: s.restoreService, status: http.StatusNoContent, summary: "Put a service back to its latest dump, with what connects to it stopped"},
		{method: "DELETE", path: "/apps/{app}/envs/{env}/services/{name}/old-volume", perm: canManage, handler: s.finishServiceUpgrade, status: http.StatusNoContent, summary: "Let go of what a service held before its last upgrade"},
		{method: "DELETE", path: "/apps/{app}/envs/{env}/services/{name}", perm: canManage, handler: s.removeService, status: http.StatusNoContent, summary: "Remove a service, with what it holds"},
		{method: "GET", path: "/apps/{app}/envs/{env}/instances", previews: true, perm: canRead, handler: s.instances, summary: "List an environment's instances", response: []apitypes.Instance{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/vars", previews: true, perm: canDeploy, handler: s.vars, summary: "List an environment's variables; a secret's value is never shown", response: []apitypes.Var{}},
		{method: "POST", path: "/apps/{app}/envs/{env}/vars/{name}/reveal", perm: canManage, handler: s.revealVar, summary: "Read a secret's value, which is recorded", response: apitypes.RevealedVar{}},
		{method: "PATCH", path: "/apps/{app}/envs/{env}/vars", perm: canManage, handler: s.changeVars, summary: "Set and unset variables, released at once unless no_restart is set",
			request: apitypes.VarsChange{}, response: apitypes.VarsResult{}},
		{method: "GET", path: "/apps/{app}/envs/{env}/logs", previews: true, perm: canReadLogs, handler: s.logs, summary: "Stream an environment's logs", stream: apitypes.LogLine{},
			query: map[string]string{
				"follow":   "true keeps the stream open.",
				"tail":     "How many of the latest lines to start with.",
				"process":  "Only this process's lines.",
				"instance": "Only this instance's lines: its number or its ID.",
				"source":   "Which sources, separated by commas: stdout, stderr, moorage, build, access and events. The first three unless asked.",
				"grep":     "Only the lines with this text, whatever its case.",
				"problems": "true keeps the lines that report a problem: errors, warnings, standard error, and requests that failed.",
				"since":    "Only lines since then: how long ago (15m, 2h, 7d) or a moment.",
				"until":    "Only lines until then.",
				"after":    "Only lines after this moment, for a follower that reconnects.",
				"format":   "text sends plain text in place of NDJSON.",
				"download": "1 sends it as a file to keep, of up to 200,000 lines.",
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
		mutating := r.Method != http.MethodGet && r.Method != http.MethodHead
		record := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		if mutating {
			defer func() { s.audit(r, rt, p, record.status) }()
			w = record
		}
		app := r.PathValue("app")
		if msg := refused(rt, p, app); msg != "" {
			problem(w, http.StatusForbidden, "forbidden", msg)
			return
		}
		if app != "" {
			access, err := s.auth.AppAccess(r.Context(), p, app)
			if err != nil {
				s.internal(w, err)
				return
			}
			// Nothing says whether it exists: an app someone may not see gets the answer that one that is not there gets.
			if !access.See || access.NoApp && !mutating && names.ValidateApp(app) == nil {
				problem(w, http.StatusNotFound, "not_found", sentenceOf(fmt.Errorf("no app %q: %w", app, store.ErrNotFound)))
				return
			}
			if msg := roleRefused(rt, access); msg != "" {
				problem(w, http.StatusForbidden, "forbidden", msg)
				return
			}
		}
		// After whether the app can be seen, so that this does not say that it is there.
		if msg, err := s.refusedByEnvs(r, rt, p, app); err != nil {
			s.internal(w, err)
			return
		} else if msg != "" {
			problem(w, http.StatusForbidden, "forbidden", msg)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		if rt.stream != nil || rt.sse != nil || rt.socket {
			// What stays open must end when whoever opened it may no longer see it.
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			go s.watchAccess(ctx, cancel, r.Clone(ctx), rt, app)
		}
		rt.handler(w, r.WithContext(ctx))
	}
}

// watchAccess looks again, every so often, at whether a request may go on.
func (s *Server) watchAccess(ctx context.Context, cancel context.CancelFunc, r *http.Request, rt route, app string) {
	every := s.recheck
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.stillAllowed(ctx, r, rt, app) {
				cancel()
				return
			}
		}
	}
}

// stillAllowed answers as the guard would. Only a clear no counts: the database being busy for a
// moment ends nothing.
func (s *Server) stillAllowed(ctx context.Context, r *http.Request, rt route, app string) bool {
	p, err := s.auth.Request(r)
	if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrLocalOnly) {
		return false
	}
	if err != nil {
		return true
	}
	if refused(rt, p, app) != "" {
		return false
	}
	// A preview can go, and another with its name be something else.
	if msg, err := s.refusedByEnvs(r, rt, p, app); err == nil && msg != "" {
		return false
	}
	if app == "" {
		return true
	}
	access, err := s.auth.AppAccess(ctx, p, app)
	if err != nil {
		return true
	}
	return access.See && roleRefused(rt, access) == ""
}

// statusRecorder notes how a request ended, for the audit log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// audit keeps a record of a request that changes something, whoever it is from, and
// whether or not it was allowed. Nothing of its body goes in the record.
func (s *Server) audit(r *http.Request, rt route, p auth.Principal, status int) {
	result := "ok"
	switch {
	case status == http.StatusForbidden || status == http.StatusUnauthorized || status == http.StatusNotFound:
		result = "refused"
	case status >= 400:
		result = "failed"
	}
	s.auth.Audit(r.Context(), store.AuditEntry{
		Actor: p.Name, IP: s.auth.ClientIP(r), Action: rt.method + " " + rt.path, Target: r.URL.Path, Result: result,
		Data: map[string]any{"via": "api", "status": status},
	})
}

// roleRefused says why someone's role in an app's project does not allow a route, or "".
func roleRefused(rt route, access auth.Access) string {
	role := access.Role
	switch rt.perm {
	case canReadLogs:
		if !access.Logs {
			return "Viewers of the project " + access.Project.Name + " may not read logs."
		}
	case canDeploy:
		if !access.Deploy {
			return "A " + role + " of the project " + access.Project.Name + " may not do this: it takes a deployer."
		}
	case canManage:
		if !access.Manage {
			return "A " + role + " of the project " + access.Project.Name + " may not do this: it takes a maintainer."
		}
	}
	return ""
}

// refusedByEnvs keeps a token limited to previews to the previews that stand apart from production, which are the ones such a token makes. It can do nothing else:
// a route has to say that it is fit for one, and the routes with no environment in their address keep to these by themselves.
func (s *Server) refusedByEnvs(r *http.Request, rt route, p auth.Principal, app string) (string, error) {
	if p.Envs == "" || rt.perm == public {
		return "", nil
	}
	const limited = "This token is limited to previews that it has made, whose names start with " + deploy.AgentPreviewPrefix + "."
	if !rt.previews {
		return limited, nil
	}
	env := r.PathValue("env")
	if env == "" {
		return "", nil
	}
	e, err := s.db.Environment(r.Context(), app, env)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Deploying to a name that is free makes a preview of it, if it is one that nothing else will want.
		if r.Method == http.MethodPost && strings.HasSuffix(rt.path, "/deployments") && strings.HasPrefix(env, deploy.AgentPreviewPrefix) {
			return "", nil
		}
		return limited, nil
	case err != nil:
		return "", err
	case !standsAlone(e):
		return limited, nil
	}
	return "", nil
}

// standsAlone says that an environment is a preview that takes nothing of production's.
func standsAlone(e store.Environment) bool { return e.Kind == store.KindPreview && !e.Inherits }

// outOfBounds says that a token limited to previews may not see this environment, for what has none in its address.
func (s *Server) outOfBounds(r *http.Request, app, env string) bool {
	if who(r).Envs == "" {
		return false
	}
	e, err := s.db.Environment(r.Context(), app, env)
	return err != nil || !standsAlone(e)
}

// refused says why a caller may not use a route, or "".
func refused(rt route, p auth.Principal, app string) string {
	switch rt.perm {
	case canRead, canReadLogs:
		if !p.Can(auth.ScopeRead) {
			return "This token's scope does not allow reading."
		}
		if p.App != "" && app == "" && !rt.oneApp {
			return "A token restricted to an app cannot do this."
		}
	case canDeploy:
		if !p.Can(auth.ScopeDeploy) {
			return "This token's scope does not allow deploying: it needs the deploy scope."
		}
		if p.App != "" && app == "" {
			return "A token restricted to an app cannot do this."
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
