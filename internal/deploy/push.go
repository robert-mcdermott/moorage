package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/gitserver"
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
	out := progress{p.Progress}
	env, chosen := p.Option("env")
	if chosen && env != "production" {
		return fmt.Errorf("there is no environment %q: only production exists until named environments arrive", env)
	}
	var target *gitserver.Update
	for i := range p.Updates {
		u := &p.Updates[i]
		switch {
		case u.Deletion():
		case u.Branch() == "":
			out.printf("moorage  %s: tags are kept, not deployed", strings.TrimPrefix(u.Ref, "refs/tags/"))
		case chosen || u.Branch() == p.DefaultBranch:
			if target != nil {
				return errors.New("the push updates two branches that would deploy: push one at a time")
			}
			target = u
		default:
			out.printf("moorage  %s is kept, not deployed: %s deploys (previews of other branches arrive in 0.4.0)", u.Branch(), p.DefaultBranch)
		}
	}
	if target == nil {
		return nil
	}
	if _, skip := p.Option("skip-deploy"); skip || strings.Contains(target.Message, "[skip deploy]") {
		out.printf("moorage  %s is kept, not deployed: the push asked to skip the deploy", target.Branch())
		return nil
	}

	src := Source{
		Type: "git-push", Ref: target.Ref, Commit: target.New, Message: firstLine(target.Message),
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
	dep, created, err := ps.Deployer.SubmitSource(ctx, p.App, "production", p.By, src)
	if err != nil {
		return fmt.Errorf("the deployment could not start: %v", err)
	}
	if created {
		out.printf("moorage  created the app %s", p.App)
	}
	if _, detach := p.Option("detach"); detach {
		out.printf("moorage  deployment %s is queued; follow it with: moor deployments follow %s", dep.ID, dep.ID)
		return nil
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
		return fmt.Errorf("deployment %s: %v", dep.ID, err)
	}
	if dep.Status != store.Live {
		reason := dep.Error
		if reason == "" {
			reason = dep.Status
		}
		return fmt.Errorf("deployment %s %s: %s", dep.ID, dep.Status, reason)
	}
	return nil
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
