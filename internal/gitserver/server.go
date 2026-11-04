// Package gitserver serves the apps' git repositories, over SSH and Smart
// HTTP, so that a push deploys (spec/04-build-and-deploy.md, "Git server").
// It is the only package that uses the git library, whose major version is
// still in beta (spec/12-decisions.md, D4).
package gitserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage"
)

// DefaultMaxPush is the largest push accepted unless Server.MaxPush says
// otherwise.
const DefaultMaxPush = 1 << 30

// Server serves the repositories of Repos. A push's ref updates are applied
// only once Receiver has accepted them.
type Server struct {
	Repos    *Repos
	Receiver Receiver
	// MaxPush is the largest push, in bytes (DefaultMaxPush when 0).
	MaxPush int64
	Log     *slog.Logger
}

// Receiver decides what a push does. Receive runs once the pushed objects
// are stored and before any ref moves; an error refuses the whole push,
// with the error's text as the reason the client shows.
type Receiver interface {
	Receive(ctx context.Context, p *Push) error
}

// A Push is a push's proposed ref updates.
type Push struct {
	App string
	// By is who pushed: what authentication said.
	By string
	// Created reports that this push created the repository.
	Created bool
	// DefaultBranch is the repository's default branch: the app's
	// production branch.
	DefaultBranch string
	Updates       []Update
	// Options are the push options (git push -o …).
	Options []string
	// Progress writes lines that the client shows as "remote: …". Writes
	// fail once the client has gone; the push goes on.
	Progress io.Writer

	st storage.Storer
}

// An Update is one proposed ref update.
type Update struct {
	Ref      string // refs/heads/main
	Old, New string // hex; Old is empty for a creation, New for a deletion
	// Message is the new commit's message (empty for a deletion).
	Message string
}

// Branch is the branch an update is for, or "".
func (u Update) Branch() string {
	b, _ := strings.CutPrefix(u.Ref, "refs/heads/")
	if b == u.Ref {
		return ""
	}
	return b
}

// Deletion reports whether the update deletes its ref.
func (u Update) Deletion() bool { return u.New == "" }

// Export writes the tree of commit into dir, which must be empty.
func (p *Push) Export(commit, dir string) (Exported, error) {
	return export(p.st, plumbing.NewHash(commit), dir, DefaultExportLimits)
}

// ReadFile reads a file of a commit: its manifest, say, before deciding where it deploys.
func (p *Push) ReadFile(commit, name string) ([]byte, error) {
	c, err := object.GetCommit(p.st, plumbing.NewHash(commit))
	if err != nil {
		return nil, err
	}
	file, err := c.File(name)
	if err != nil {
		return nil, err
	}
	text, err := file.Contents()
	return []byte(text), err
}

// Option returns a push option's value ("env=staging" gives "staging"), and
// whether it was given (an option without a value, such as "detach", gives
// "").
func (p *Push) Option(name string) (string, bool) {
	for _, o := range p.Options {
		k, v, _ := strings.Cut(o, "=")
		if k == name {
			return v, true
		}
	}
	return "", false
}

// receivePack serves git-receive-pack for app. With advertise, it only
// advertises the refs (Smart HTTP's info/refs); stateless is Smart HTTP's
// request-response mode.
func (s *Server) receivePack(ctx context.Context, app, by string, r io.Reader, w io.WriteCloser, advertise, stateless bool, protocol string) error {
	st, err := s.Repos.open(app)
	created := false
	if errors.Is(err, ErrNoRepository) {
		// Push to create: the repository, and the app with its first deploy.
		st, err = s.Repos.create(app)
		created = true
	}
	if err != nil {
		w.Close()
		return err
	}
	req := &transport.ReceivePackRequest{AdvertiseRefs: advertise, StatelessRPC: stateless, GitProtocol: protocol}
	if advertise {
		return transport.ReceivePack(ctx, st, nil, w, req)
	}

	state := s.Repos.lock(app)
	var reserved []string
	locked := false
	defer func() {
		if !locked {
			state.mu.Lock()
		}
		for _, ref := range reserved {
			delete(state.busy, ref)
		}
		state.mu.Unlock()
	}()

	req.Hooks.PreReceive = func(ctx context.Context, info *transport.PreReceiveInfo) error {
		p := &Push{App: app, By: by, Created: created, Options: info.PushOptions, Progress: info.Progress, st: info.Storer}
		if err := s.check(state, info.Storer, info.Commands, p, &reserved); err != nil {
			return err
		}
		if err := s.Receiver.Receive(ctx, p); err != nil {
			return err
		}
		// The refs move once this returns, under the repository's lock.
		state.mu.Lock()
		locked = true
		return nil
	}
	maxPush := s.MaxPush
	if maxPush == 0 {
		maxPush = DefaultMaxPush
	}
	return transport.ReceivePack(ctx, st, requestReader(r, maxPush), w, req)
}

// check applies the server's own rules to a push's commands, describes
// them in p, and reserves their refs, so that two pushes to one branch do
// not deploy at once.
func (s *Server) check(state *repoState, st storage.Storer, cmds []*packp.Command, p *Push, reserved *[]string) error {
	state.mu.Lock()
	defer state.mu.Unlock()

	// A new repository's default branch is main, or else the first branch
	// the push creates; so is that of a repository whose first push was
	// refused.
	if p.Created || empty(st) {
		branch := ""
		for _, c := range cmds {
			if b := c.Name.Short(); c.Name.IsBranch() && c.Action() == packp.Create {
				if b == "main" {
					branch = b
					break
				}
				if branch == "" {
					branch = b
				}
			}
		}
		if branch != "" {
			if err := setDefaultBranch(st, branch); err != nil {
				return err
			}
		}
	}
	p.DefaultBranch = defaultBranch(st)

	for _, c := range cmds {
		ref := c.Name.String()
		switch {
		case !c.Name.IsBranch() && !c.Name.IsTag():
			return fmt.Errorf("%s: only branches and tags can be pushed", ref)
		case c.Name.IsBranch() && c.Name.Short() == p.DefaultBranch && c.Action() == packp.Delete:
			return fmt.Errorf("%s is the production branch: it cannot be deleted", c.Name.Short())
		case state.busy[ref]:
			return fmt.Errorf("another push to %s is being deployed; push again when it ends", c.Name.Short())
		}
		current := plumbing.ZeroHash
		if r, err := st.Reference(c.Name); err == nil {
			current = r.Hash()
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		if current != c.Old {
			return fmt.Errorf("%s has moved since you fetched: fetch, then push again", c.Name.Short())
		}
		u := Update{Ref: ref}
		if !c.Old.IsZero() {
			u.Old = c.Old.String()
		}
		if !c.New.IsZero() {
			u.New = c.New.String()
			if commit, err := object.GetCommit(st, c.New); err == nil {
				u.Message = commit.Message
			}
		}
		p.Updates = append(p.Updates, u)
	}
	for _, u := range p.Updates {
		state.busy[u.Ref] = true
		*reserved = append(*reserved, u.Ref)
	}
	return nil
}

// uploadPack serves git-upload-pack (fetches and clones) for app.
func (s *Server) uploadPack(ctx context.Context, app string, r io.ReadCloser, w io.WriteCloser, advertise, stateless bool, protocol string) error {
	st, err := s.Repos.open(app)
	if err != nil {
		w.Close()
		return err
	}
	return transport.UploadPack(ctx, st, r, w, &transport.UploadPackRequest{
		AdvertiseRefs: advertise, StatelessRPC: stateless, GitProtocol: protocol,
	})
}

func (s *Server) logger() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}
