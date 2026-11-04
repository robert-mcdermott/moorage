package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// DomainResolver looks up what a domain points to. Tests have their own.
type DomainResolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// InvalidDomainError says what is wrong with a domain.
type InvalidDomainError struct{ Msg string }

func (e *InvalidDomainError) Error() string { return e.Msg }

var (
	validDomainHost = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	validPathSeg    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,62}$`)
)

// ParseDomain reads a hostname, and perhaps a path on it, as someone would write them:
// portal.example.com, or tools.example.com/reports/.
func ParseDomain(address string) (host, prefix string, err error) {
	address = strings.TrimSpace(strings.ToLower(address))
	for _, scheme := range []string{"https://", "http://"} {
		address = strings.TrimPrefix(address, scheme)
	}
	host, path, hasPath := strings.Cut(address, "/")
	if !validDomainHost.MatchString(host) || len(host) > 253 {
		return "", "", &InvalidDomainError{fmt.Sprintf("%q is not a hostname such as portal.example.com", host)}
	}
	if !hasPath {
		return host, "", nil
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return host, "", nil
	}
	for _, seg := range strings.Split(path, "/") {
		if !validPathSeg.MatchString(seg) || strings.HasPrefix(seg, ".") {
			return "", "", &InvalidDomainError{fmt.Sprintf("%q is not a path such as /reports", "/"+path)}
		}
	}
	return host, "/" + path, nil
}

// DomainRecord says what DNS record a domain needs: a CNAME, for a whole hostname.
func (d *Deployer) DomainRecord(app, env, hostname string) string {
	return hostname + " CNAME " + d.opts.Config.EnvHost(app, env)
}

// AddDomain attaches a domain to an environment, and looks at once whether it points here.
func (d *Deployer) AddDomain(ctx context.Context, app, env, by, address string, strip bool) (store.Domain, error) {
	host, prefix, err := ParseDomain(address)
	if err != nil {
		return store.Domain{}, err
	}
	cfg := d.opts.Config
	switch {
	case host == cfg.ConsoleHost:
		return store.Domain{}, &InvalidDomainError{"that is the console's hostname"}
	case host == cfg.Domain && cfg.Routing != "path", strings.HasSuffix(host, "."+cfg.Domain):
		return store.Domain{}, &InvalidDomainError{host + " is under the server's own domain, which names its apps by itself"}
	case cfg.Routing == "path" && host == cfg.Domain:
		return store.Domain{}, &InvalidDomainError{host + " is where the server serves its apps by itself"}
	case strip && prefix == "":
		return store.Domain{}, &InvalidDomainError{"only a path can be stripped: a domain needs one, such as tools.example.com/reports"}
	}
	dom := store.Domain{ID: ids.New(ids.Domain), Hostname: host, Prefix: prefix, StripPrefix: strip, CreatedBy: by, CreatedAt: time.Now().UTC()}
	if err := d.db.AddDomain(ctx, app, env, dom); err != nil {
		if errors.Is(err, store.ErrDomainTaken) {
			return dom, &InvalidDomainError{dom.Address() + " is attached already"}
		}
		return dom, err
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.DomainAdded, App: app, Env: env, Actor: by, Data: map[string]any{"domain": dom.Address()}})
	return d.VerifyDomain(ctx, app, env, dom.Address())
}

func (d *Deployer) findDomain(ctx context.Context, app, env, address string) (store.Domain, error) {
	host, prefix, err := ParseDomain(address)
	if err != nil {
		return store.Domain{}, err
	}
	list, err := d.db.Domains(ctx, app, env)
	if err != nil {
		return store.Domain{}, err
	}
	for _, dom := range list {
		if dom.Hostname == host && dom.Prefix == prefix {
			return dom, nil
		}
	}
	return store.Domain{}, fmt.Errorf("no domain %s of %s: %w", host+prefix, env, store.ErrNotFound)
}

// VerifyDomain looks at what a domain points to, and if that is here, it starts to serve.
func (d *Deployer) VerifyDomain(ctx context.Context, app, env, address string) (store.Domain, error) {
	dom, err := d.findDomain(ctx, app, env, address)
	if err != nil {
		return dom, err
	}
	return d.checkDomain(ctx, dom)
}

func (d *Deployer) checkDomain(ctx context.Context, dom store.Domain) (store.Domain, error) {
	lookup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := time.Now().UTC()
	problem := d.domainProblem(lookup, dom)
	if err := d.db.SetDomainChecked(ctx, dom.ID, now, problem); err != nil {
		return dom, err
	}
	became := problem == "" && !dom.Verified()
	dom.CheckedAt, dom.CheckError = now, problem
	if problem == "" && dom.VerifiedAt.IsZero() {
		dom.VerifiedAt = now
	}
	if became {
		if err := d.opts.Reload(ctx); err != nil {
			return dom, err
		}
		d.opts.Events.Publish(ctx, store.Event{Type: events.DomainVerified, App: dom.App, Env: dom.Env, Data: map[string]any{"domain": dom.Address()}})
	}
	return dom, nil
}

// domainProblem says why a domain does not point here: empty if it does. It may be a CNAME to
// the environment's own hostname, or have addresses in common with the server's.
func (d *Deployer) domainProblem(ctx context.Context, dom store.Domain) string {
	cfg := d.opts.Config
	target := cfg.EnvHost(dom.App, dom.Env)
	resolver := d.opts.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if cname, err := resolver.LookupCNAME(ctx, dom.Hostname); err == nil {
		cname = strings.TrimSuffix(strings.ToLower(cname), ".")
		if cname == target || cname == cfg.Domain || strings.HasSuffix(cname, "."+cfg.Domain) {
			return ""
		}
	}
	found, err := resolver.LookupIPAddr(ctx, dom.Hostname)
	if err != nil || len(found) == 0 {
		return "its DNS has no record yet: add " + d.DomainRecord(dom.App, dom.Env, dom.Hostname)
	}
	var ours []string
	for _, name := range []string{target, cfg.ConsoleHost} {
		addrs, err := resolver.LookupIPAddr(ctx, name)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ours = append(ours, a.IP.String())
		}
	}
	var theirs []string
	for _, a := range found {
		theirs = append(theirs, a.IP.String())
		if slices.Contains(ours, a.IP.String()) {
			return ""
		}
	}
	return "it points to " + strings.Join(theirs, ", ") + ", not to this server: add " + d.DomainRecord(dom.App, dom.Env, dom.Hostname)
}

// RemoveDomain detaches a domain, at once.
func (d *Deployer) RemoveDomain(ctx context.Context, app, env, by, address string) error {
	host, prefix, err := ParseDomain(address)
	if err != nil {
		return err
	}
	if err := d.db.RemoveDomain(ctx, app, env, host, prefix); err != nil {
		return err
	}
	if err := d.opts.Reload(ctx); err != nil {
		return err
	}
	d.opts.Events.Publish(ctx, store.Event{Type: events.DomainRemoved, App: app, Env: env, Actor: by, Data: map[string]any{"domain": host + prefix}})
	return nil
}

// domainRecheck is how often a domain that does not yet point here is looked at again.
const domainRecheck = 5 * time.Minute

// sweepDomains looks again at those that have not yet been seen to point here.
func (d *Deployer) sweepDomains(ctx context.Context, now time.Time) {
	list, err := d.db.AllDomains(ctx)
	if err != nil {
		d.log.Warn("look at the domains", "err", err)
		return
	}
	for _, dom := range list {
		if dom.Verified() || now.Sub(dom.CheckedAt) < domainRecheck {
			continue
		}
		if _, err := d.checkDomain(ctx, dom); err != nil {
			d.log.Warn("look at a domain", "domain", dom.Address(), "err", err)
		}
	}
}
