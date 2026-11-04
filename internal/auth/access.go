package auth

import (
	"context"
	"errors"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Access says what someone may do with an app: what their role in its project allows,
// narrowed by what their token's scope and app allow (spec/08-security.md, "Authorisation").
type Access struct {
	// Role is their role: a project's role, or "admin" for a server's administrator.
	Role string
	// See is seeing it at all, and Logs reading its logs.
	See, Logs bool
	// Deploy is deploying, restarting, rolling back and scaling it, and seeing its variables.
	Deploy bool
	// Manage is changing its variables, its members and its fate.
	Manage bool
	// Project is the project it is in.
	Project store.Project
}

// RoleAdmin is what Access calls a server's administrator.
const RoleAdmin = "admin"

// roleAccess says what a role allows in a project.
func roleAccess(role string, project store.Project) Access {
	a := Access{Role: role, Project: project}
	switch role {
	case RoleAdmin, store.RoleMaintainer:
		a.See, a.Logs, a.Deploy, a.Manage = true, true, true, true
	case store.RoleDeployer:
		a.See, a.Logs, a.Deploy = true, true, true
	case store.RoleViewer:
		a.See, a.Logs = true, project.ViewersSeeLogs
	}
	return a
}

// narrow keeps to what a token's scope allows.
func (a Access) narrow(p Principal) Access {
	a.See = a.See && p.Can(ScopeRead)
	a.Logs = a.Logs && p.Can(ScopeRead)
	a.Deploy = a.Deploy && p.Can(ScopeDeploy)
	a.Manage = a.Manage && p.Can(ScopeAdmin)
	return a
}

// ProjectAccess says what someone may do in a project.
func (s *Service) ProjectAccess(ctx context.Context, p Principal, project store.Project) (Access, error) {
	if p.Local() || p.Admin {
		return roleAccess(RoleAdmin, project).narrow(p), nil
	}
	role, err := s.DB.MemberRole(ctx, project.ID, p.UserID)
	if err != nil {
		return Access{}, err
	}
	if role == "" {
		role = project.EveryoneRole
	}
	return roleAccess(role, project).narrow(p), nil
}

// AppAccess says what someone may do with an app. An app that does not exist yet would
// be in the default project: deploying is what creates it.
func (s *Service) AppAccess(ctx context.Context, p Principal, app string) (Access, error) {
	if !p.CanApp(app) {
		return Access{}, nil
	}
	project, err := s.DB.AppProject(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		project, err = s.DB.ProjectByName(ctx, store.DefaultProject)
	}
	if err != nil {
		return Access{}, err
	}
	return s.ProjectAccess(ctx, p, project)
}

// Visible returns what tells which apps someone may see, for lists and feeds: one call
// reads what many questions need. An event about no app is for everyone.
func (s *Service) Visible(ctx context.Context, p Principal) (func(app string) bool, error) {
	if p.Local() || p.Admin {
		return func(app string) bool { return p.CanApp(app) || app == "" }, nil
	}
	projects, err := s.DB.VisibleProjects(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	apps, err := s.DB.AppProjectIDs(ctx)
	if err != nil {
		return nil, err
	}
	return func(app string) bool {
		if app == "" {
			return true
		}
		if !p.CanApp(app) {
			return false
		}
		_, ok := projects[apps[app]]
		return ok
	}, nil
}

// AuditRetention is how long records of who did what are kept.
const AuditRetention = 365 * 24 * time.Hour

// Audit keeps a record of who did what. A failure to is logged: what happened has happened.
func (s *Service) Audit(ctx context.Context, e store.AuditEntry) {
	e.ID, e.At = ids.New(ids.Audit), s.now().UTC()
	if err := s.DB.AddAudit(context.WithoutCancel(ctx), e); err != nil {
		s.logger().Error("keep a record of who did what", "action", e.Action, "err", err)
	}
}
