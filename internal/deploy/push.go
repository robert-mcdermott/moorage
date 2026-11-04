package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/gitserver"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Pushes deploys what is pushed to the apps' repositories: the production
// branch (the repository's default branch) deploys to production
// (spec/04-build-and-deploy.md, "Branch → environment mapping").
type Pushes struct {
	Deployer *Deployer
}

// Receive implements gitserver.Receiver. It runs before the refs move:
// the push waits for its deployment, and fails if the deployment does, so
// the branch stays where it was.
func (ps *Pushes) Receive(ctx context.Context, p *gitserver.Push) error {
	_, err := ps.Deliver(ctx, p)
	return err
}

// Deliver does what a push asks, and says which deployment that made and how it ended, if any: it may be right to
// keep what was pushed and deploy none of it.
func (ps *Pushes) Deliver(ctx context.Context, p *gitserver.Push) (store.Deployment, error) {
	out := progress{p.Progress}
	var none store.Deployment
	chosen, picked := p.Option("env")
	var target *gitserver.Update
	targetEnv, targetKind := names.Production, store.KindProduction
	for i := range p.Updates {
		u := &p.Updates[i]
		switch {
		case u.Deletion():
			ps.branchDeleted(ctx, p, u, out)
		case u.Branch() == "":
			out.printf("moorage  %s: tags are kept, not deployed", strings.TrimPrefix(u.Ref, "refs/tags/"))
		default:
			env, kind, why := ps.destination(ctx, p, u, chosen, picked)
			if why != "" {
				out.printf("moorage  %s is kept, not deployed: %s", u.Branch(), why)
				continue
			}
			if target != nil {
				return none, errors.New("the push updates two branches that would deploy: push one at a time")
			}
			target, targetEnv, targetKind = u, env, kind
		}
	}
	if target == nil {
		return none, nil
	}
	if _, skip := p.Option("skip-deploy"); skip || strings.Contains(target.Message, "[skip deploy]") {
		out.printf("moorage  %s is kept, not deployed: the push asked to skip the deploy", target.Branch())
		return none, nil
	}
	if why := watchSkips(p, target); why != "" {
		out.printf("moorage  %s is kept, not deployed: %s", target.Branch(), why)
		return none, nil
	}
	if err := ps.Deployer.checkLock(ctx, p.App, targetEnv); err != nil {
		var locked *LockedError
		if errors.As(err, &locked) {
			out.printf("moorage  %s is kept, not deployed: %v", target.Branch(), err)
			return none, nil
		}
		return none, err
	}
	if targetKind == store.KindPreview {
		name, err := ps.Deployer.PreviewName(ctx, p.App, target.Branch())
		if err != nil {
			var limit *NotPreviewableError
			if errors.As(err, &limit) {
				out.printf("moorage  %s is kept, not deployed: %v", target.Branch(), err)
				return none, nil
			}
			return none, err
		}
		targetEnv = name
	}

	src := Source{
		Type: SourcePush, Ref: target.Ref, Commit: target.New, Message: firstLine(target.Message),
		EnvKind: targetKind, Branch: target.Branch(),
		Fill: func(dir string) (int, error) {
			ex, err := p.Export(target.New, dir)
			if n := len(ex.Symlinks); n > 0 {
				out.printf("moorage  note: %d symbolic link%s left out (%s): links are not deployed", n, plural(n), list(ex.Symlinks))
			}
			if n := len(ex.Submodules); n > 0 {
				out.printf("moorage  note: %d submodule%s left out (%s): submodules are not deployed yet", n, plural(n), list(ex.Submodules))
			}
			if n := len(ex.LFS); n > 0 {
				out.printf("moorage  warning: Git LFS is not supported: the app gets the pointers of %d file%s (%s), not their content", n, plural(n), list(ex.LFS))
			}
			return ex.Files, err
		},
	}
	dep, created, err := ps.Deployer.SubmitSource(ctx, p.App, targetEnv, p.By, src)
	if err != nil {
		return none, fmt.Errorf("the deployment could not start: %v", err)
	}
	if created {
		out.printf("moorage  created the app %s", p.App)
	}
	if p.Started != nil {
		p.Started()
	}
	if _, detach := p.Option("detach"); detach {
		out.printf("moorage  deployment %s is queued; follow it with: moor deployments follow %s", dep.ID, dep.ID)
		return dep, nil
	}

	// The deployment goes on if the client leaves (Ctrl-C): the push then
	// waits for it all the same, and moves the branch if it succeeds.
	dep, err = ps.Deployer.Follow(context.WithoutCancel(ctx), dep.ID, func(l logs.Line) error {
		if l.Stream == "M" {
			out.printf("%s", l.Text)
		} else {
			out.printf("             %s", l.Text)
		}
		return nil
	})
	if err != nil {
		return dep, fmt.Errorf("deployment %s: %v", dep.ID, err)
	}
	if dep.Status != store.Live {
		reason := dep.Error
		if reason == "" {
			reason = dep.Status
		}
		return dep, fmt.Errorf("deployment %s %s: %s", dep.ID, dep.Status, reason)
	}
	return dep, nil
}

// watchSkips says why a push should not deploy, if its manifest says which paths matter and it
// touches none of them. What is new, or cannot be compared, deploys: a force push may have changed
// anything, and it costs less to deploy once too often than to miss a change.
func watchSkips(p *gitserver.Push, u *gitserver.Update) string {
	if u.Old == "" {
		return ""
	}
	data, err := p.ReadFile(u.New, manifest.FileName)
	if err != nil {
		return ""
	}
	m, err := manifest.Parse(data, manifest.FileName)
	if err != nil || len(m.App.Watch) == 0 {
		return ""
	}
	changed, err := p.ChangedFiles(u.Old, u.New)
	if err != nil {
		return ""
	}
	for _, file := range changed {
		if file == manifest.FileName || branchMatches(m.App.Watch, file) {
			return ""
		}
	}
	return "it touches nothing that [app].watch names"
}

// destination says where a pushed branch deploys, or why it does not.
func (ps *Pushes) destination(ctx context.Context, p *gitserver.Push, u *gitserver.Update, chosen string, picked bool) (env, kind, why string) {
	if p.Preview == gitserver.PreviewOnly {
		return ps.previewOnly(ctx, p, u)
	}
	if picked {
		if chosen == names.Production {
			return names.Production, store.KindProduction, ""
		}
		if err := names.ValidateEnv(chosen); err != nil {
			return "", "", err.Error()
		}
		kind, err := ps.Deployer.db.EnvKind(ctx, p.App, chosen)
		if err != nil {
			return "", "", err.Error()
		}
		return chosen, kind, ""
	}
	if u.Branch() == p.DefaultBranch {
		return names.Production, store.KindProduction, ""
	}
	// An environment this branch was given earlier.
	if e, err := ps.Deployer.db.EnvironmentOfBranch(ctx, p.App, u.Branch()); err == nil {
		return e.Name, e.Kind, ""
	}
	// What the commit's own manifest says.
	var declared *manifest.Manifest
	if data, err := p.ReadFile(u.New, manifest.FileName); err == nil {
		if m, err := manifest.Parse(data, manifest.FileName); err == nil {
			declared = m
		}
	}
	env, kind, why = branchTarget(declared, u.Branch())
	if why == "" && kind == store.KindPreview && p.Preview == gitserver.PreviewNever {
		return "", "", "it has no preview for being pushed"
	}
	return env, kind, why
}

// previewOnly says where a pull request deploys: to its own preview, or nowhere. Its code comes with a manifest that
// could name any environment for it, and that is not for it to say. All it may do is ask for no preview.
func (ps *Pushes) previewOnly(ctx context.Context, p *gitserver.Push, u *gitserver.Update) (env, kind, why string) {
	if e, err := ps.Deployer.db.EnvironmentOfBranch(ctx, p.App, u.Branch()); err == nil && e.Kind != store.KindPreview {
		return "", "", "the environment " + e.Name + " is using its name"
	}
	if data, err := p.ReadFile(u.New, manifest.FileName); err == nil {
		if m, err := manifest.Parse(data, manifest.FileName); err == nil && m.Previews != nil && m.Previews.Enabled != nil && !*m.Previews.Enabled {
			return "", "", "previews are turned off in its moorage.toml"
		}
	}
	return "", store.KindPreview, ""
}

// branchDeleted destroys the preview of a branch that is gone.
func (ps *Pushes) branchDeleted(ctx context.Context, p *gitserver.Push, u *gitserver.Update, out progress) {
	if u.Branch() == "" {
		return
	}
	e, err := ps.Deployer.db.EnvironmentOfBranch(ctx, p.App, u.Branch())
	if err != nil || e.Kind != store.KindPreview {
		return
	}
	if err := ps.Deployer.DeleteEnvironment(ctx, p.App, e.Name, p.By); err != nil {
		out.printf("moorage  the preview %s stays: %v", e.Name, err)
		return
	}
	out.printf("moorage  destroyed the preview %s, with its branch", e.Name)
}

// progress writes lines to the client; once the client has gone, writes
// fail and are dropped.
type progress struct{ w io.Writer }

func (p progress) printf(format string, args ...any) {
	fmt.Fprintf(p.w, format+"\n", args...)
}

func firstLine(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return line
}

func list(paths []string) string {
	if len(paths) > 3 {
		return strings.Join(paths[:3], ", ") + ", …"
	}
	return strings.Join(paths, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
