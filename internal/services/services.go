// SPDX-License-Identifier: Apache-2.0

// Package services runs what an app declares under [services]: a database or an object store for each environment, which Moorage starts and connects it to
// (spec/13-services.md). What it holds has to outlast any release, which is why this is beside the runtime and not part of it.
//
// Nothing here asks for a container engine unless an environment has or wants a service, which is what keeps an engine optional for everyone else.
package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

const (
	// postgresImage is the image of a version. Never latest: what it means changes under a database's data.
	postgresImage = "docker.io/library/postgres:%s-trixie"
	postgresPort  = 5432
	// postgresUser names the user and the database that an app is given.
	postgresUser = "app"
	// postgresStop is how long it has to stop. Its image warns that even 90 seconds may not be enough, and the engines' default is ten.
	postgresStop = 120 * time.Second
	// readyLimit is how long a service has to become healthy. A first start initialises a database.
	readyLimit = 2 * time.Minute
	// sealedName is what a service's password is sealed under, which no variable can be called.
	sealedPrefix = "service:"

	// The S3 server: Versity Gateway on its POSIX backend, with what goes with each file in a directory of its own, so that a plain tar of the two is a dump. Its
	// default keeps that in extended attributes, which tar in its image would lose. See spec/13-services.md.
	versityImage = "docker.io/versity/versitygw:v1.10.0"
	versityPort  = 7070
	versityStop  = 30 * time.Second
	s3DataDir    = "/data/s3"
	s3MetaDir    = "/data/meta"
	// s3Bucket and s3Access are what an app is given, along with a secret key of its own. It has this one bucket and one credential.
	s3Bucket = "app"
	s3Access = "moorage"
	s3Region = "us-east-1"
)

// Keyring seals and opens a service's password.
type Keyring interface {
	Seal(ctx context.Context, appID, scope, name, value string) ([]byte, error)
	Open(ctx context.Context, appID, scope, name string, sealed []byte) (string, error)
}

// Options configure a Manager.
type Options struct {
	DB      *store.DB
	Keyring Keyring
	Engines *container.Source
	// AllocPort gives a free port, and ReleasePort says that it has been recorded.
	AllocPort   func(ctx context.Context) (int, error)
	ReleasePort func(port int)
	Events      *events.Bus
	Log         *slog.Logger
	// DataDir is the server's, as a dump goes in an environment's data directory.
	DataDir string
	// GateFor says who can reach an environment, as a copy of production's data is not for everyone.
	GateFor func(set, asked, kind string) (mode, source string)
	// Tick is how often it looks that what should be running is.
	Tick time.Duration
	// ReadyLimit is how long a service has to become healthy, for tests.
	ReadyLimit time.Duration
}

// Manager runs services. A nil Manager has none.
type Manager struct {
	o     Options
	locks sync.Map // by app/env: one change at a time
	said  sync.Map // by service ID: what stops it from being looked after, as last logged
}

// New makes a Manager.
func New(o Options) *Manager {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Tick == 0 {
		o.Tick = 10 * time.Second
	}
	if o.ReadyLimit == 0 {
		o.ReadyLimit = readyLimit
	}
	return &Manager{o: o}
}

// MajorChangeError says that a manifest asks for another major version than a service holds, which needs its data moving.
type MajorChangeError struct{ Msg string }

func (e *MajorChangeError) Error() string { return e.Msg }

// Wanted says which of a plan's services an environment gets. It needs none of its own if a variable set on it says where to go instead, and a preview has none if the
// manifest says so.
func (m *Manager) Wanted(ctx context.Context, app, env, kind string, plan manifest.Plan) (map[string]manifest.ServicePlan, error) {
	if m == nil || len(plan.Services) == 0 {
		return nil, nil
	}
	own := map[string]bool{}
	vars, err := m.o.DB.Vars(ctx, app, env)
	if err != nil {
		return nil, err
	}
	for _, v := range vars {
		if !v.AllEnvs {
			own[v.Name] = true
		}
	}
	wanted := map[string]manifest.ServicePlan{}
	for name, s := range plan.Services {
		if kind == store.KindPreview && s.Previews == "none" {
			continue
		}
		if own[s.AddressVariable()] {
			continue
		}
		wanted[name] = s
	}
	return wanted, nil
}

// CheckEngine says why what a plan asks for could not run here, before anything is built for it.
func (m *Manager) CheckEngine(ctx context.Context, app, env, kind string, plan manifest.Plan) error {
	wanted, err := m.Wanted(ctx, app, env, kind, plan)
	if err != nil || len(wanted) == 0 {
		return err
	}
	if st := m.o.Engines.Status(ctx); !st.Ready() {
		return fmt.Errorf("this app asks for %s under [services], and %s", strings.Join(sortedNames(wanted), ", "), st.Problem())
	}
	return nil
}

// Any says whether there are any services on this server.
func (m *Manager) Any(ctx context.Context) bool {
	if m == nil {
		return false
	}
	list, err := m.o.DB.AllServices(ctx)
	return err == nil && len(list) > 0
}

// Ensure makes and starts what a release needs, and returns when it is healthy, so that a release command can migrate. It comes before the release: a service
// that does not become healthy fails the deployment, with the previous release still serving.
func (m *Manager) Ensure(ctx context.Context, app, env, kind, by, appDir string, plan manifest.Plan, log io.Writer) error {
	wanted, err := m.Wanted(ctx, app, env, kind, plan)
	if err != nil || len(wanted) == 0 {
		return err
	}
	unlock := m.lock(app + "/" + env)
	defer unlock()
	st := m.o.Engines.Status(ctx)
	if !st.Ready() {
		return fmt.Errorf("this app asks for %s under [services], and %s", strings.Join(sortedNames(wanted), ", "), st.Problem())
	}
	for _, name := range sortedNames(wanted) {
		s := wanted[name]
		rec, err := m.o.DB.Service(ctx, app, env, name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			if err := m.create(ctx, app, env, by, appDir, name, s, plan, log); err != nil {
				return fmt.Errorf("the service %s: %w", name, err)
			}
		case err != nil:
			return err
		case rec.State == store.ServiceSeeding:
			// A server stopped as it was being made: what is there has not been sanitized, and no app has seen it.
			driver, err := m.driverFor(ctx, rec)
			if err != nil {
				return fmt.Errorf("the service %s: %w", name, err)
			}
			m.forget(rec, driver)
			if err := m.create(ctx, app, env, by, appDir, name, s, plan, log); err != nil {
				return fmt.Errorf("the service %s: %w", name, err)
			}
		default:
			if err := m.ensureExisting(ctx, rec, s, log); err != nil {
				return fmt.Errorf("the service %s: %w", name, err)
			}
		}
	}
	return nil
}

func sortedNames(services map[string]manifest.ServicePlan) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// create makes a service afresh. If it does not come up, nothing is left: it holds nothing yet.
func (m *Manager) create(ctx context.Context, app, env, by, appDir, name string, s manifest.ServicePlan, plan manifest.Plan, log io.Writer) (err error) {
	engine := m.o.Engines.Claim()
	driver := m.o.Engines.Driver()
	if engine == nil || driver == nil {
		return errors.New("this server has no container engine")
	}
	image := s.Image
	switch {
	case s.Type == manifest.ServiceS3:
		image = versityImage
	case image == "":
		version := s.Version
		if version == "" {
			version = manifest.DefaultPostgres
		}
		image = fmt.Sprintf(postgresImage, version)
	default:
		image = qualified(image)
	}
	fmt.Fprintf(log, "▸ service    %s: pulling %s\n", name, image)
	if err := engine.Pull(ctx, image, log, log); err != nil {
		return err
	}
	info, err := engine.InspectImage(ctx, image)
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(image[strings.LastIndex(image, ":")+1:], "v")
	if s.Type == manifest.ServicePostgres {
		version = postgresMajor(info)
		if version == "" {
			return fmt.Errorf("%s does not say which version of PostgreSQL it holds, as an image based on the official one does", image)
		}
		if s.Version != "" && s.Version != version {
			return fmt.Errorf("%s holds PostgreSQL %s, and this asks for %s", image, version, s.Version)
		}
	}
	appID, envID, err := m.o.DB.Scope(ctx, app, env)
	if err != nil {
		return err
	}
	password, err := newPassword()
	if err != nil {
		return err
	}
	id := ids.New(ids.Service)
	sealed, err := m.o.Keyring.Seal(ctx, appID, envID, sealedPrefix+name, password)
	if err != nil {
		return err
	}
	port, err := m.o.AllocPort(ctx)
	if err != nil {
		return err
	}
	mount := s3DataDir
	if s.Type == manifest.ServicePostgres {
		mount = dataMount(info, version)
	}
	state, refused := store.ServiceRunning, ""
	if s.Seed != "" {
		if refused, err = m.seedRefused(ctx, app, env, plan); err != nil {
			return err
		}
		if refused == "" {
			state = store.ServiceSeeding
		}
	}
	rec := store.Service{
		ID: id, App: app, Env: env, Name: name, Type: s.Type, Version: version, Image: image, Digest: repoDigest(info),
		Engine: engine.Bin, Volume: container.ServiceVolume(id), Mount: mount, Port: port, Sealed: sealed, EnvVar: s.AddressVariable(), Memory: s.Memory,
		State: state, CreatedBy: by, CreatedAt: time.Now().UTC(),
	}
	err = m.o.DB.CreateService(ctx, rec)
	m.o.ReleasePort(port)
	if err != nil {
		return err
	}
	rec.EnvID = envID
	defer func() {
		if err != nil {
			m.forget(rec, driver)
		}
	}()
	seeded := false
	if refused != "" {
		fmt.Fprintf(log, "▸ service    %s: not seeded from production, and it starts empty: %s\n", name, refused)
	}
	if state == store.ServiceSeeding {
		// If this goes wrong, nothing is left of it, which is better than half a copy with what sanitizing it was to remove.
		if err := m.seed(ctx, driver, rec, s, appDir, password, log); err != nil {
			return err
		}
		if err := m.o.DB.SetServiceState(ctx, rec.ID, store.ServiceRunning); err != nil {
			return err
		}
		rec.State, seeded = store.ServiceRunning, true
	}
	if err := driver.RunService(ctx, m.spec(rec, password)); err != nil {
		return err
	}
	fmt.Fprintf(log, "▸ service    %s: waiting for it to be ready\n", name)
	if err := m.waitReady(ctx, driver, rec); err != nil {
		return err
	}
	if s.Type == manifest.ServiceS3 {
		if !seeded {
			if err := m.makeBucket(ctx, rec, password); err != nil {
				return err
			}
		}
		fmt.Fprintf(log, "▸ service    %s: S3 is ready, and the app gets %s\n", name, strings.Join(s.Variables(), ", "))
	} else {
		fmt.Fprintf(log, "▸ service    %s: PostgreSQL %s is ready, and the app gets %s\n", name, version, rec.EnvVar)
	}
	m.emit(ctx, events.ServiceCreated, rec, nil)
	m.o.Log.Info("made a service", "app", app, "env", env, "service", name, "type", s.Type, "version", version)
	return nil
}

// ensureExisting starts what is there, and refuses what would need its data moving.
func (m *Manager) ensureExisting(ctx context.Context, rec store.Service, s manifest.ServicePlan, log io.Writer) error {
	if s.Type != rec.Type {
		return &MajorChangeError{fmt.Sprintf("it is a service of %s, and the manifest asks for %s: remove it with moor services remove, or give the new one another name", rec.Type, s.Type)}
	}
	if s.Type == manifest.ServicePostgres && s.Version != "" && s.Version != rec.Version {
		return &MajorChangeError{fmt.Sprintf("it holds data of PostgreSQL %s, and the manifest asks for %s. That takes moving its data: dump it, remove it with moor services remove, and deploy again, or go back to %s", rec.Version, s.Version, rec.Version)}
	}
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return err
	}
	if rec.State != store.ServiceRunning {
		if err := m.o.DB.SetServiceState(ctx, rec.ID, store.ServiceRunning); err != nil {
			return err
		}
		rec.State = store.ServiceRunning
	}
	if s.Image != "" && qualified(s.Image) != rec.Image {
		fmt.Fprintf(log, "▸ service    %s: it goes on running %s, as what it holds was made with that: moor services upgrade --image is the way to change it\n", rec.Name, rec.Image)
	}
	// A limit is a change to how it runs, and what it holds stays.
	restart := false
	if s.Memory != rec.Memory {
		if err := m.o.DB.SetServiceMemory(ctx, rec.ID, s.Memory); err != nil {
			return err
		}
		rec.Memory, restart = s.Memory, true
		fmt.Fprintf(log, "▸ service    %s: restarting it with its new limit on memory\n", rec.Name)
	}
	running, err := driver.ServiceRunning(ctx, container.ServiceName(rec.ID))
	if err != nil {
		return err
	}
	if !running || restart || rec.PendingLoad {
		password, err := m.password(ctx, rec)
		if err != nil {
			return err
		}
		if !running {
			fmt.Fprintf(log, "▸ service    %s: starting it\n", rec.Name)
		}
		if running {
			if err := driver.StopService(container.ServiceName(rec.ID), stopLimit(rec)); err != nil {
				return err
			}
		}
		if err := m.bringUp(ctx, driver, rec, password); err != nil {
			return err
		}
	}
	return m.waitReady(ctx, driver, rec)
}

// driverFor is the driver of the engine that holds a service. If another is in use now, what it holds is not to be found there.
func (m *Manager) driverFor(ctx context.Context, rec store.Service) (*container.Driver, error) {
	st := m.o.Engines.Status(ctx)
	if err := st.Err(); err != nil {
		return nil, err
	}
	if st.Bin != rec.Engine {
		// On a new machine, after a restore, its engine may be somewhere else, and its volumes are not there anyway. A program that is still here may hold what it held.
		if _, err := os.Stat(rec.Engine); err == nil {
			return nil, fmt.Errorf("the service %s is held by %s, and this server is using %s: set engine under [containers] in server.toml", rec.Name, rec.Engine, st.Bin)
		}
		if err := m.o.DB.SetServiceEngine(ctx, rec.ID, st.Bin); err != nil {
			return nil, err
		}
	}
	driver := m.o.Engines.Driver()
	if driver == nil {
		return nil, errors.New("this server has no container engine")
	}
	return driver, nil
}

func (m *Manager) password(ctx context.Context, rec store.Service) (string, error) {
	appID, _, err := m.o.DB.Scope(ctx, rec.App, rec.Env)
	if err != nil {
		return "", err
	}
	return m.o.Keyring.Open(ctx, appID, rec.EnvID, sealedPrefix+rec.Name, rec.Sealed)
}

// spec is how a service runs.
func (m *Manager) spec(rec store.Service, password string) container.ServiceSpec {
	image := rec.Image
	if rec.Digest != "" {
		image = rec.Digest
	}
	spec := container.ServiceSpec{
		ID: rec.ID, Name: container.ServiceName(rec.ID), App: rec.App, Env: rec.Env, Service: rec.Name, Image: image,
		Network: container.ServiceNetwork(rec.EnvID), HostPort: rec.Port, Memory: rec.Memory, StopTimeout: stopLimit(rec),
	}
	if rec.Type == manifest.ServiceS3 {
		spec.ContainerPort = versityPort
		spec.Volumes = map[string]string{rec.Volume: s3DataDir, container.ServiceMetadataVolume(rec.ID): s3MetaDir}
		spec.Vars = map[string]string{"VGW_BACKEND": "posix", "VGW_BACKEND_ARGS": s3DataDir, "VGW_META_SIDECAR": s3MetaDir, "VGW_HEALTH": "/health", "ROOT_ACCESS_KEY": s3Access}
		spec.Secrets = map[string]string{"ROOT_SECRET_KEY": password}
	} else {
		spec.ContainerPort, spec.StopSignal = postgresPort, "SIGINT"
		spec.Volumes = map[string]string{rec.Volume: rec.Mount}
		spec.Vars = map[string]string{"POSTGRES_USER": postgresUser, "POSTGRES_DB": postgresUser}
		spec.Secrets = map[string]string{"POSTGRES_PASSWORD": password}
	}
	return spec
}

// privateSpec is a service where no app can reach it: for what it has to go through first.
func (m *Manager) privateSpec(rec store.Service, password string) container.ServiceSpec {
	spec := m.spec(rec, password)
	spec.Name, spec.HostPort, spec.Network = spec.Name+"-private", 0, ""
	return spec
}

// volumesOf are what a service keeps what it holds in.
func volumesOf(rec store.Service) []string {
	if rec.Type == manifest.ServiceS3 {
		return []string{rec.Volume, container.ServiceMetadataVolume(rec.ID)}
	}
	return []string{rec.Volume}
}

// stopLimit is how long a service has to stop without losing anything.
func stopLimit(rec store.Service) time.Duration {
	if rec.Type == manifest.ServiceS3 {
		return versityStop
	}
	return postgresStop
}

// makeBucket makes the bucket that an app is given. It speaks to the service on the address that a native app would, signing as an app would.
func (m *Manager) makeBucket(ctx context.Context, rec store.Service, secret string) error {
	asking, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(asking, http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/%s", rec.Port, s3Bucket), nil)
	if err != nil {
		return err
	}
	backup.Sign(req, nil, s3Access, secret, s3Region, time.Now())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("make its bucket: %w", err)
	}
	defer res.Body.Close()
	said, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	// One that is there already is what was wanted.
	if res.StatusCode/100 != 2 && res.StatusCode != http.StatusConflict {
		return fmt.Errorf("make its bucket: %s: %s", res.Status, strings.TrimSpace(string(said)))
	}
	return nil
}

// dataMount is where a volume has to go, for what an image keeps to last: the volume that it declares around its data, or its data. It moved at 18, and an image can say otherwise,
// and a volume in the wrong place holds nothing, while what the database writes goes in one that nothing looks at again.
func dataMount(info container.ImageInfo, major string) string {
	pgdata := ""
	for _, env := range info.Config.Env {
		if value, ok := strings.CutPrefix(env, "PGDATA="); ok {
			pgdata = value
		}
	}
	if pgdata != "" {
		for volume := range info.Config.Volumes {
			if pgdata == volume || strings.HasPrefix(pgdata, strings.TrimRight(volume, "/")+"/") {
				return volume
			}
		}
		return pgdata
	}
	if n, err := strconv.Atoi(major); err == nil && n < 18 {
		return "/var/lib/postgresql/data"
	}
	return "/var/lib/postgresql"
}

// waitReady returns once a service answers. It asks on the loopback address, as its image first starts a server on a socket alone, to set things up.
func (m *Manager) waitReady(ctx context.Context, driver *container.Driver, rec store.Service) error {
	return m.waitReadyNamed(ctx, driver, rec, container.ServiceName(rec.ID))
}

func (m *Manager) waitReadyNamed(ctx context.Context, driver *container.Driver, rec store.Service, name string) error {
	limit, cancel := context.WithTimeout(ctx, m.o.ReadyLimit)
	defer cancel()
	for {
		check := []string{"pg_isready", "-q", "-h", "127.0.0.1", "-U", postgresUser}
		if rec.Type == manifest.ServiceS3 {
			check = []string{"wget", "-q", "-O", "/dev/null", fmt.Sprintf("http://127.0.0.1:%d/health", versityPort)}
		}
		if _, err := driver.ExecService(limit, name, nil, check...); err == nil {
			return nil
		}
		select {
		case <-limit.Done():
			said, _ := driver.ServiceLogs(context.WithoutCancel(ctx), name, 20)
			return fmt.Errorf("it did not become ready in %s. What it said:\n%s", m.o.ReadyLimit, strings.TrimSpace(said))
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Addresses gives the variables that connect an app to an environment's services: a loopback address for a native app, and a name on a network for one in a container. They are
// given when an instance starts, as a secret is, and are not captured in a release. What is secret is kept apart from what is not, as what is secret is removed from what an app
// prints, and a bucket's name is too short a word for that. It also says the network, if there is one.
func (m *Manager) Addresses(ctx context.Context, app, env string, plan manifest.Plan, inContainer bool) (secret, plain map[string]string, network string, err error) {
	if m == nil || len(plan.Services) == 0 {
		return nil, nil, "", nil
	}
	list, err := m.o.DB.Services(ctx, app, env)
	if err != nil {
		return nil, nil, "", err
	}
	secret, plain = map[string]string{}, map[string]string{}
	for _, rec := range list {
		s, named := plan.Services[rec.Name]
		// A release that does not name a service does not get it, so that going back behaves.
		if !named || s.Type != rec.Type {
			continue
		}
		password, err := m.password(ctx, rec)
		if err != nil {
			return nil, nil, "", err
		}
		host, port := "127.0.0.1", rec.Port
		if inContainer {
			host = container.ServiceName(rec.ID)
			network = container.ServiceNetwork(rec.EnvID)
			port = postgresPort
			if rec.Type == manifest.ServiceS3 {
				port = versityPort
			}
		}
		if rec.Type != manifest.ServiceS3 {
			secret[s.Env] = postgresURL(host, port, password)
			// On its own as well, so that it is kept out of what an app prints as it is, and not only in its address.
			secret[s.Env+"_PASSWORD"] = password
			continue
		}
		endpoint := fmt.Sprintf("http://%s:%d", host, port)
		names := s.Variables()
		// In the order that Variables has them.
		plain[names[0]], plain[names[1]], plain[names[2]] = endpoint, s3Bucket, s3Region
		plain[names[3]], secret[names[4]] = s3Access, password
		if len(names) > 5 {
			plain[names[5]], plain[names[6]] = endpoint, s3Region
			plain[names[7]], secret[names[8]] = s3Access, password
		}
	}
	return secret, plain, network, nil
}

func postgresURL(host string, port int, password string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(postgresUser, password), Host: fmt.Sprintf("%s:%d", host, port), Path: "/" + postgresUser, RawQuery: "sslmode=disable"}
	return u.String()
}

// Run keeps what should be running running, until ctx ends. Containers run with --restart=no, as Moorage starts them again, which needs no lingering or systemd units under
// rootless Podman. With no services, it asks nothing of any engine.
func (m *Manager) Run(ctx context.Context) {
	if m == nil {
		return
	}
	m.look(ctx)
	t := time.NewTicker(m.o.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.look(ctx)
		}
	}
}

func (m *Manager) look(ctx context.Context) {
	list, err := m.o.DB.AllServices(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.o.Log.Error("read the services", "err", err)
		}
		return
	}
	if !slices.ContainsFunc(list, func(s store.Service) bool { return s.State == store.ServiceRunning }) {
		return
	}
	if !m.o.Engines.Status(ctx).Ready() {
		return // what is running is left as it is, and says nothing of what the daemon was running
	}
	for _, rec := range list {
		if rec.State != store.ServiceRunning || ctx.Err() != nil {
			continue
		}
		// One that takes a while to come back, as one putting back its dump does, holds up no other. What is already being looked at is left to that.
		go m.keepUp(ctx, rec)
	}
}

func (m *Manager) keepUp(ctx context.Context, rec store.Service) {
	unlock, ok := m.tryLock(rec.App + "/" + rec.Env)
	if !ok {
		return
	}
	defer unlock()
	// What was read before the lock was held may have changed: it may be going to sleep, or be gone, and starting it again would be the last thing wanted.
	rec, err := m.o.DB.Service(ctx, rec.App, rec.Env, rec.Name)
	if err != nil || rec.State != store.ServiceRunning {
		return
	}
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		m.sayOnce(rec.ID, err.Error(), rec)
		return
	}
	running, err := driver.ServiceRunning(ctx, container.ServiceName(rec.ID))
	if err != nil {
		m.sayOnce(rec.ID, err.Error(), rec)
		return
	}
	if running && !rec.PendingLoad {
		m.said.Delete(rec.ID)
		return
	}
	password, err := m.password(ctx, rec)
	if err == nil {
		m.o.Log.Warn("a service had stopped: starting it again", "app", rec.App, "env", rec.Env, "service", rec.Name)
		err = m.bringUp(ctx, driver, rec, password)
	}
	if err != nil {
		m.sayOnce(rec.ID, err.Error(), rec)
		return
	}
	m.said.Delete(rec.ID)
	m.emit(ctx, events.ServiceRestarted, rec, nil)
}

// sayOnce logs what stops a service from being looked after, until that changes.
func (m *Manager) sayOnce(id, problem string, rec store.Service) {
	if last, ok := m.said.Load(id); ok && last == problem {
		return
	}
	m.said.Store(id, problem)
	if m.o.Log != nil {
		m.o.Log.Error("a service cannot be looked after", "app", rec.App, "env", rec.Env, "service", rec.Name, "err", problem)
	}
}

// Sleep stops an environment's services, and keeps what they hold.
func (m *Manager) Sleep(ctx context.Context, app, env string) error {
	return m.each(ctx, app, env, func(driver *container.Driver, rec store.Service) error {
		if err := driver.StopService(container.ServiceName(rec.ID), stopLimit(rec)); err != nil {
			return err
		}
		return m.o.DB.SetServiceState(ctx, rec.ID, store.ServiceStopped)
	})
}

// Wake starts an environment's services again, and returns when they are ready, as what comes next connects to them. They should be running from now on, whether or not this manages
// it now, so that what keeps them running picks them up if it cannot.
func (m *Manager) Wake(ctx context.Context, app, env string) error {
	if m == nil {
		return nil
	}
	list, err := m.o.DB.Services(ctx, app, env)
	if err != nil {
		return err
	}
	for _, rec := range list {
		if err := m.o.DB.SetServiceState(ctx, rec.ID, store.ServiceRunning); err != nil {
			return err
		}
	}
	return m.each(ctx, app, env, func(driver *container.Driver, rec store.Service) error {
		running, err := driver.ServiceRunning(ctx, container.ServiceName(rec.ID))
		if err != nil {
			return err
		}
		if !running {
			password, err := m.password(ctx, rec)
			if err != nil {
				return err
			}
			if err := m.bringUp(ctx, driver, rec, password); err != nil {
				return err
			}
		}
		return m.waitReady(ctx, driver, rec)
	})
}

// each does something to every service of an environment. With none, it asks nothing of any engine.
func (m *Manager) each(ctx context.Context, app, env string, do func(*container.Driver, store.Service) error) error {
	if m == nil {
		return nil
	}
	list, err := m.o.DB.Services(ctx, app, env)
	if err != nil || len(list) == 0 {
		return err
	}
	unlock := m.lock(app + "/" + env)
	defer unlock()
	for _, rec := range list {
		driver, err := m.driverFor(ctx, rec)
		if err != nil {
			return err
		}
		if err := do(driver, rec); err != nil {
			return fmt.Errorf("the service %s: %w", rec.Name, err)
		}
	}
	return nil
}

// Blocks says why an environment's services, or with none named any of an app's, could not be removed now. Whatever is about to lose the record of one asks this first, as
// what it holds would otherwise go on with nothing to say that it was there.
func (m *Manager) Blocks(ctx context.Context, app, env string) error {
	if m == nil {
		return nil
	}
	list, err := m.o.DB.AppServices(ctx, app)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(list, func(s store.Service) bool { return env == "" || s.Env == env }) {
		return nil
	}
	return m.o.Engines.Status(ctx).Err()
}

// Remove removes a service, with what it holds. It is the one way that happens to a service that its environment still has, as nothing deletes data because a line went from a file.
func (m *Manager) Remove(ctx context.Context, app, env, name, by string) error {
	rec, err := m.o.DB.Service(ctx, app, env, name)
	if err != nil {
		return err
	}
	unlock := m.lock(app + "/" + env)
	defer unlock()
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return err
	}
	if err := driver.StopService(container.ServiceName(rec.ID), stopLimit(rec)); err != nil {
		return err
	}
	if err := removeVolumes(driver, rec); err != nil {
		return err
	}
	if rec.OldVolume != "" {
		if err := driver.RemoveVolume(rec.OldVolume); err != nil {
			return err
		}
	}
	if err := os.Remove(m.dumpPath(rec)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.o.Log.Warn("remove a service's dump", "service", rec.Name, "err", err)
	}
	if err := m.o.DB.DeleteService(ctx, rec.ID); err != nil {
		return err
	}
	m.emit(ctx, events.ServiceRemoved, rec, map[string]any{"by": by})
	m.o.Log.Info("removed a service", "app", app, "env", env, "service", name, "by", by)
	return nil
}

// RemoveEnv removes the services of an environment that is going, with what they hold.
func (m *Manager) RemoveEnv(ctx context.Context, app, env string) error {
	if m == nil {
		return nil
	}
	list, err := m.o.DB.Services(ctx, app, env)
	if err != nil || len(list) == 0 {
		return err
	}
	for _, rec := range list {
		if err := m.Remove(ctx, app, env, rec.Name, "moorage"); err != nil {
			return err
		}
	}
	if driver := m.o.Engines.Driver(); driver != nil {
		if err := driver.RemoveNetwork(container.ServiceNetwork(list[0].EnvID)); err != nil {
			m.o.Log.Warn("remove an environment's network", "app", app, "env", env, "err", err)
		}
	}
	return nil
}

// RemoveApp removes the services of an app that is going. With keepData, what they hold stays in its volumes, which an administrator can find by their names in the log.
func (m *Manager) RemoveApp(ctx context.Context, app string, keepData bool) error {
	if m == nil {
		return nil
	}
	list, err := m.o.DB.AppServices(ctx, app)
	if err != nil {
		return err
	}
	envs := map[string]bool{}
	for _, rec := range list {
		envs[rec.Env] = true
		if !keepData {
			continue
		}
		driver, err := m.driverFor(ctx, rec)
		if err != nil {
			return err
		}
		if err := driver.StopService(container.ServiceName(rec.ID), stopLimit(rec)); err != nil {
			return err
		}
		m.o.Log.Warn("a deleted app's service keeps what it held", "app", app, "env", rec.Env, "service", rec.Name, "volume", rec.Volume)
		if err := m.o.DB.DeleteService(ctx, rec.ID); err != nil {
			return err
		}
	}
	for env := range envs {
		if err := m.RemoveEnv(ctx, app, env); err != nil {
			return err
		}
	}
	return nil
}

// Status is how a service stands: its record, and what its container is doing, if that can be found out.
type Status struct {
	store.Service
	// Running is what the engine says.
	Running bool
	// Unknown says that the engine could not be asked.
	Unknown bool
}

// List says how an environment's services stand. It asks the engine if there are any.
func (m *Manager) List(ctx context.Context, app, env string) ([]Status, error) {
	if m == nil {
		return nil, nil
	}
	list, err := m.o.DB.Services(ctx, app, env)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(list))
	for _, rec := range list {
		st := Status{Service: rec}
		driver, err := m.driverFor(ctx, rec)
		if err == nil {
			st.Running, err = driver.ServiceRunning(ctx, container.ServiceName(rec.ID))
		}
		st.Unknown = err != nil
		out = append(out, st)
	}
	return out, nil
}

// Logs says the newest of what a service has said.
func (m *Manager) Logs(ctx context.Context, app, env, name string, tail int) (string, error) {
	rec, err := m.o.DB.Service(ctx, app, env, name)
	if err != nil {
		return "", err
	}
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return "", err
	}
	return driver.ServiceLogs(ctx, container.ServiceName(rec.ID), tail)
}

// removeVolumes removes what a service holds.
func removeVolumes(driver *container.Driver, rec store.Service) error {
	for _, name := range volumesOf(rec) {
		if err := driver.RemoveVolume(name); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) forget(rec store.Service, driver *container.Driver) {
	ctx := context.Background()
	if err := driver.StopService(container.ServiceName(rec.ID), 10*time.Second); err != nil {
		m.o.Log.Warn("remove a service that did not start", "service", rec.Name, "err", err)
	}
	if err := removeVolumes(driver, rec); err != nil {
		m.o.Log.Warn("remove a service's volume", "service", rec.Name, "err", err)
	}
	if err := m.o.DB.DeleteService(ctx, rec.ID); err != nil {
		m.o.Log.Error("forget a service that did not start", "service", rec.Name, "err", err)
	}
}

func (m *Manager) emit(ctx context.Context, kind string, rec store.Service, data map[string]any) {
	if m.o.Events == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["service"] = rec.Name
	data["type"] = rec.Type
	m.o.Events.Publish(ctx, store.Event{Type: kind, App: rec.App, Env: rec.Env, Data: data})
}

func (m *Manager) lockFor(key string) *sync.Mutex {
	lock, _ := m.locks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *Manager) lock(key string) func() {
	lock := m.lockFor(key)
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) tryLock(key string) (func(), bool) {
	lock := m.lockFor(key)
	if !lock.TryLock() {
		return nil, false
	}
	return lock.Unlock, true
}

// qualified gives a short image name its registry, as Podman would otherwise have to ask which.
func qualified(image string) string {
	first, _, found := strings.Cut(image, "/")
	if !found {
		return "docker.io/library/" + image
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return image
	}
	return "docker.io/" + image
}

var majorOf = regexp.MustCompile(`^PG_MAJOR=(\d+)$`)

// postgresMajor is the version that an image holds, by what the official image and those based on it say.
func postgresMajor(info container.ImageInfo) string {
	for _, env := range info.Config.Env {
		if m := majorOf.FindStringSubmatch(env); m != nil {
			return m[1]
		}
	}
	return ""
}

func repoDigest(info container.ImageInfo) string {
	if len(info.RepoDigests) > 0 {
		return info.RepoDigests[0]
	}
	return ""
}

// newPassword makes a password that goes in an address as it is.
func newPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Problems says how many services there are, and what is wrong with them, for the doctor.
func (m *Manager) Problems(ctx context.Context) (int, []string) {
	if m == nil {
		return 0, nil
	}
	list, err := m.o.DB.AllServices(ctx)
	if err != nil {
		return 0, []string{err.Error()}
	}
	if len(list) == 0 {
		return 0, nil
	}
	if st := m.o.Engines.Status(ctx); !st.Ready() {
		return len(list), []string{st.Problem()}
	}
	var problems []string
	for _, rec := range list {
		if rec.State != store.ServiceRunning {
			continue
		}
		driver, err := m.driverFor(ctx, rec)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if running, err := driver.ServiceRunning(ctx, container.ServiceName(rec.ID)); err != nil {
			problems = append(problems, fmt.Sprintf("%s of %s of %s: %v", rec.Name, rec.Env, rec.App, err))
		} else if !running {
			problems = append(problems, fmt.Sprintf("%s of %s of %s should be running, and is not", rec.Name, rec.Env, rec.App))
		}
	}
	return len(list), problems
}
