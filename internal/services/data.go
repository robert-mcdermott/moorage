// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// dataLimit is how long a dump or a restore can take. It is a long time, for what should not stop a backup for ever.
const dataLimit = 2 * time.Hour

// dumpPath is where a service's dump goes: in its environment's data directory, so that a backup holds it with no change to the format of a backup, and older versions can read it.
func (m *Manager) dumpPath(rec store.Service) string {
	return dumpPathIn(m.o.DataDir, rec.App, rec.Env, rec.Name, rec.Type)
}

func dumpPathIn(dataDir, app, env, name, kind string) string {
	file := name + ".sql"
	if kind == manifest.ServiceS3 {
		file = name + ".tar"
	}
	return filepath.Join(dataDir, "data", app, env, ".services", file)
}

// DumpAll writes a dump of each service of production and of the named environments, ahead of a backup. Volumes are not in a backup, so that this is what one holds of them. It
// says what went wrong, which does not lose the rest of the backup. Previews are left out, as they are not meant to last.
func (m *Manager) DumpAll(ctx context.Context) []string {
	if m == nil {
		return nil
	}
	list, err := m.o.DB.AllServices(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	for _, rec := range list {
		kind, err := m.o.DB.EnvKind(ctx, rec.App, rec.Env)
		if err != nil || kind == store.KindPreview {
			continue
		}
		if rec.State != store.ServiceRunning {
			continue // asleep: the last dump is still what it holds
		}
		if err := m.dump(ctx, rec); err != nil {
			m.o.Log.Warn("dump a service", "app", rec.App, "env", rec.Env, "service", rec.Name, "err", err)
			problems = append(problems, fmt.Sprintf("the service %s of %s of %s was not dumped: %v", rec.Name, rec.Env, rec.App, err))
		}
	}
	return problems
}

func (m *Manager) dump(ctx context.Context, rec store.Service) error {
	unlock := m.lock(rec.App + "/" + rec.Env)
	defer unlock()
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return err
	}
	path := m.dumpPath(rec)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Only a whole one replaces the last one.
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	limit, cancel := context.WithTimeout(ctx, dataLimit)
	defer cancel()
	if err := driver.ExecServiceTo(limit, container.ServiceName(rec.ID), temp, dumpCommand(rec)...); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func dumpCommand(rec store.Service) []string {
	if rec.Type == manifest.ServiceS3 {
		return []string{"tar", "-C", "/data", "-cf", "-", "s3", "meta"}
	}
	return []string{"pg_dump", "-U", postgresUser, "-d", postgresUser, "--no-owner", "--no-privileges"}
}

func loadCommand(rec store.Service) []string {
	if rec.Type == manifest.ServiceS3 {
		return []string{"tar", "-C", "/data", "-xf", "-"}
	}
	return []string{"psql", "-U", postgresUser, "-d", postgresUser, "-q", "-v", "ON_ERROR_STOP=1"}
}

// load puts a dump into a service, for a service that has just been given a fresh volume.
func (m *Manager) load(ctx context.Context, driver *container.Driver, rec store.Service, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	limit, cancel := context.WithTimeout(ctx, dataLimit)
	defer cancel()
	return driver.ExecServiceFrom(limit, container.ServiceName(rec.ID), f, loadCommand(rec)...)
}

// recover puts back what a service held, if it has come up on a fresh volume: on a new machine after a restore, or after someone removed its volume.
func (m *Manager) recover(ctx context.Context, driver *container.Driver, rec store.Service) error {
	path := m.dumpPath(rec)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		m.o.Log.Warn("a service has come up with no data, and there is no dump of it", "app", rec.App, "env", rec.Env, "service", rec.Name)
		return nil
	}
	if err != nil {
		return err
	}
	if err := m.waitReady(ctx, driver, rec); err != nil {
		return err
	}
	if err := m.load(ctx, driver, rec, path); err != nil {
		return fmt.Errorf("load its dump: %w", err)
	}
	m.o.Log.Info("a service has its dump back", "app", rec.App, "env", rec.Env, "service", rec.Name, "bytes", info.Size())
	return nil
}

// seed fills a new preview's service from production's latest dump, if the manifest asks for that, and it is safe. Copying production into previews is how customer data leaks, so
// that this is an opt-in, and it has to say that it has thought about what is in there. It comes before any app connects.
func (m *Manager) seed(ctx context.Context, driver *container.Driver, rec store.Service, s manifest.ServicePlan, plan manifest.Plan, appDir string, log io.Writer) error {
	refused, err := m.seedRefused(ctx, rec, plan)
	if err != nil {
		return err
	}
	if refused != "" {
		fmt.Fprintf(log, "▸ service    %s: not seeded from production, and it starts empty: %s\n", rec.Name, refused)
		return nil
	}
	path := dumpPathIn(m.o.DataDir, rec.App, names.Production, rec.Name, rec.Type)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(log, "▸ service    %s: not seeded from production, and it starts empty: there is no dump of it yet, which a backup makes\n", rec.Name)
		return nil
	}
	fmt.Fprintf(log, "▸ service    %s: seeding it from production's latest dump\n", rec.Name)
	if err := m.load(ctx, driver, rec, path); err != nil {
		return fmt.Errorf("load production's dump: %w", err)
	}
	if s.Sanitize == manifest.SanitizeNone {
		return nil
	}
	script, err := os.Open(filepath.Join(appDir, filepath.FromSlash(s.Sanitize)))
	if err != nil {
		return fmt.Errorf("sanitize: %w", err)
	}
	defer script.Close()
	fmt.Fprintf(log, "▸ service    %s: sanitizing it with %s\n", rec.Name, s.Sanitize)
	limit, cancel := context.WithTimeout(ctx, dataLimit)
	defer cancel()
	if err := driver.ExecServiceFrom(limit, container.ServiceName(rec.ID), script, loadCommand(rec)...); err != nil {
		return fmt.Errorf("sanitize: %w", err)
	}
	return nil
}

// seedRefused says why a preview should not have a copy of production's data.
func (m *Manager) seedRefused(ctx context.Context, rec store.Service, plan manifest.Plan) (string, error) {
	e, err := m.o.DB.Environment(ctx, rec.App, rec.Env)
	if err != nil {
		return "", err
	}
	switch {
	case e.Kind != store.KindPreview:
		return "only a preview is seeded", nil
	case !e.Inherits:
		return "a coding agent's preview takes nothing of production's", nil
	}
	fork, err := m.o.DB.EnvironmentFromFork(ctx, rec.App, rec.Env)
	if err != nil {
		return "", err
	}
	if fork {
		return "this is the preview of a pull request from a fork", nil
	}
	access, err := m.o.DB.EnvironmentAccess(ctx, rec.App, rec.Env)
	if err != nil {
		return "", err
	}
	mode, _ := m.o.GateFor(access.Mode, plan.AccessFor(rec.Env, e.Kind), e.Kind)
	if mode == "public" && len(access.Allow) == 0 {
		return "anyone can reach this preview: give previews a gate with access under [environments.preview]", nil
	}
	return "", nil
}

// Update takes the newest image of a service's version, which is something that you ask for. What it holds stays, as a version's images read one another's data.
func (m *Manager) Update(ctx context.Context, app, env, name string, log io.Writer) (string, error) {
	rec, err := m.o.DB.Service(ctx, app, env, name)
	if err != nil {
		return "", err
	}
	unlock := m.lock(app + "/" + env)
	defer unlock()
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return "", err
	}
	engine := driver.Engine()
	if err := engine.Pull(ctx, rec.Image, log, log); err != nil {
		return "", err
	}
	info, err := engine.InspectImage(ctx, rec.Image)
	if err != nil {
		return "", err
	}
	digest := repoDigest(info)
	if digest == rec.Digest {
		return "", nil
	}
	if rec.Type == manifest.ServicePostgres {
		if major := postgresMajor(info); major != rec.Version {
			return "", &MajorChangeError{fmt.Sprintf("%s now holds PostgreSQL %s, and this holds data of %s: moor services upgrade is for that", rec.Image, major, rec.Version)}
		}
	}
	password, err := m.password(ctx, rec)
	if err != nil {
		return "", err
	}
	if err := driver.StopService(container.ServiceName(rec.ID), stopLimit(rec)); err != nil {
		return "", err
	}
	if err := m.o.DB.SetServiceDigest(ctx, rec.ID, digest); err != nil {
		return "", err
	}
	rec.Digest = digest
	if _, err := m.start(ctx, driver, rec, password); err != nil {
		return "", err
	}
	if err := m.waitReady(ctx, driver, rec); err != nil {
		return "", err
	}
	return digest, nil
}

// Upgrade moves a PostgreSQL service to a newer major version: a dump with the newer version's tools, a new volume, a restore and a check. The images hold only their own major
// version, so that pg_upgrade is not available. The old volume stays until someone says that this was good, and anything going wrong leaves the old one running as it was.
// What connects to it has to be stopped first, or what is written after the dump is lost.
func (m *Manager) Upgrade(ctx context.Context, app, env, name, version string, log io.Writer) error {
	rec, err := m.o.DB.Service(ctx, app, env, name)
	if err != nil {
		return err
	}
	unlock := m.lock(app + "/" + env)
	defer unlock()
	if rec.Type != manifest.ServicePostgres {
		return &MajorChangeError{"only PostgreSQL has versions to move between"}
	}
	if rec.OldVolume != "" {
		return &MajorChangeError{fmt.Sprintf("it still has the volume from its last upgrade: moor services upgrade %s --finish lets it go", name)}
	}
	if !newerVersion(version, rec.Version) {
		return &MajorChangeError{fmt.Sprintf("it holds PostgreSQL %s, and %s is not newer: a dump from the newer does not load into the older", rec.Version, version)}
	}
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return err
	}
	engine := driver.Engine()
	image := fmt.Sprintf(postgresImage, version)
	fmt.Fprintf(log, "pulling %s\n", image)
	if err := engine.Pull(ctx, image, log, log); err != nil {
		return err
	}
	info, err := engine.InspectImage(ctx, image)
	if err != nil {
		return err
	}
	if major := postgresMajor(info); major != version {
		return fmt.Errorf("%s holds PostgreSQL %s", image, major)
	}
	password, err := m.password(ctx, rec)
	if err != nil {
		return err
	}
	old := container.ServiceName(rec.ID)
	next := rec
	next.Version, next.Image, next.Digest, next.Volume = version, image, repoDigest(info), container.ServiceVolume(rec.ID)+"-"+version
	temp := old + "-upgrade"
	spec := m.spec(next, password)
	spec.Name, spec.HostPort = temp, 0
	if _, err := driver.RunService(ctx, spec); err != nil {
		return err
	}
	failed := func(err error) error {
		driver.StopService(temp, 10*time.Second)
		driver.RemoveVolume(next.Volume)
		return err
	}
	if err := m.waitReadyNamed(ctx, driver, next, temp); err != nil {
		return failed(err)
	}
	fmt.Fprintf(log, "dumping PostgreSQL %s with the tools of %s, and restoring it\n", rec.Version, version)
	limit, cancel := context.WithTimeout(ctx, dataLimit)
	defer cancel()
	script := fmt.Sprintf(`set -euo pipefail; pg_dump -h %s -U %s -d %s --no-owner --no-privileges | psql -U %s -d %s -q -v ON_ERROR_STOP=1`, old, postgresUser, postgresUser, postgresUser, postgresUser)
	if _, err := driver.ExecService(limit, temp, map[string]string{"PGPASSWORD": password}, "bash", "-c", script); err != nil {
		return failed(fmt.Errorf("move its data: %w", err))
	}
	count := `select coalesce(sum(n_live_tup), 0) || ' rows in ' || count(*) || ' tables' from pg_stat_user_tables`
	analyze := []string{"psql", "-U", postgresUser, "-d", postgresUser, "-q", "-c", "analyze"}
	for _, target := range []string{old, temp} {
		if _, err := driver.ExecService(limit, target, nil, analyze...); err != nil {
			return failed(fmt.Errorf("check it: %w", err))
		}
	}
	had, err := driver.ExecService(limit, old, nil, "psql", "-U", postgresUser, "-d", postgresUser, "-At", "-c", count)
	if err != nil {
		return failed(fmt.Errorf("check it: %w", err))
	}
	has, err := driver.ExecService(limit, temp, nil, "psql", "-U", postgresUser, "-d", postgresUser, "-At", "-c", count)
	if err != nil {
		return failed(fmt.Errorf("check it: %w", err))
	}
	if strings.TrimSpace(had) != strings.TrimSpace(has) {
		return failed(fmt.Errorf("it had %s, and the new one has %s", strings.TrimSpace(had), strings.TrimSpace(has)))
	}
	fmt.Fprintf(log, "it holds %s, as it did\n", strings.TrimSpace(has))
	// From here, it comes up on the new one, and the record says so only once it has.
	if err := driver.StopService(temp, stopLimit(next)); err != nil {
		return failed(err)
	}
	if err := driver.StopService(old, stopLimit(rec)); err != nil {
		driver.RemoveVolume(next.Volume)
		return err
	}
	if _, err := m.start(ctx, driver, next, password); err == nil {
		err = m.waitReady(ctx, driver, next)
		if err == nil {
			return m.o.DB.SetServiceImage(ctx, rec.ID, next.Version, next.Image, next.Digest, next.Volume, rec.Volume)
		}
		fmt.Fprintf(log, "it did not come up, and goes back: %v\n", err)
	}
	if _, err := m.start(ctx, driver, rec, password); err != nil {
		return fmt.Errorf("go back to what it was: %w", err)
	}
	driver.RemoveVolume(next.Volume)
	return errors.New("the new version did not come up, and it has gone back to what it was")
}

// FinishUpgrade lets go of what a service held before its last upgrade.
func (m *Manager) FinishUpgrade(ctx context.Context, app, env, name string) error {
	rec, err := m.o.DB.Service(ctx, app, env, name)
	if err != nil {
		return err
	}
	if rec.OldVolume == "" {
		return nil
	}
	unlock := m.lock(app + "/" + env)
	defer unlock()
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return err
	}
	if err := driver.RemoveVolume(rec.OldVolume); err != nil {
		return err
	}
	return m.o.DB.SetServiceImage(ctx, rec.ID, rec.Version, rec.Image, rec.Digest, rec.Volume, "")
}

func newerVersion(version, than string) bool {
	a, err := strconv.Atoi(version)
	if err != nil {
		return false
	}
	b, err := strconv.Atoi(than)
	return err == nil && a > b
}
