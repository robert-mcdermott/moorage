// SPDX-License-Identifier: Apache-2.0

package services

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

// dumpPath is where a service's dump goes: under the app's own directory of data, so that a backup holds it with no change to the format of a backup. Not in an environment's data
// directory, which its app's user owns, and could leave a link in that the server would follow, and where a preview's code could reach production's.
func (m *Manager) dumpPath(rec store.Service) string {
	return dumpPathIn(m.o.DataDir, rec.App, rec.Env, rec.Name, rec.Type)
}

func dumpPathIn(dataDir, app, env, name, kind string) string {
	file := name + ".sql"
	if kind == manifest.ServiceS3 {
		file = name + ".tar"
	}
	return filepath.Join(dataDir, "data", app, ".services", env, file)
}

// DumpAll writes a dump of each service of production and of the named environments, ahead of a backup. Volumes are not in a backup, so that this is what one holds of them. It
// says what went wrong, which does not lose the rest of the backup. Previews are left out, as they are not meant to last. One that is asleep is woken for it, where no app can reach
// it, as what it holds may have changed since the last one.
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
		if err := m.dump(ctx, rec); err != nil {
			m.o.Log.Warn("dump a service", "app", rec.App, "env", rec.Env, "service", rec.Name, "err", err)
			problems = append(problems, fmt.Sprintf("the service %s of %s of %s was not dumped: %v", rec.Name, rec.Env, rec.App, err))
		}
	}
	return problems
}

// dump replaces a service's dump. It holds no lock while it reads: a dump is of a moment, and it can take hours.
func (m *Manager) dump(ctx context.Context, rec store.Service) error {
	// The others may have taken a long time, and what it would dump now may not be all that it held.
	rec, err := m.o.DB.Service(ctx, rec.App, rec.Env, rec.Name)
	if err != nil {
		return err
	}
	if rec.PendingLoad {
		return errors.New("what it held has not been put back yet")
	}
	if rec.State == store.ServiceSeeding {
		return errors.New("it is still being made")
	}
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return err
	}
	name := container.ServiceName(rec.ID)
	if rec.State == store.ServiceStopped {
		unlock := m.lock(rec.App + "/" + rec.Env)
		defer unlock()
		// It may have woken while this waited.
		if now, err := m.o.DB.Service(ctx, rec.App, rec.Env, rec.Name); err == nil && now.State != store.ServiceStopped {
			return errors.New("it woke as this was about to dump it: the next backup has it")
		}
		// Starting it to dump it would make its volume, if that has gone, and the dump of that would replace the only good one.
		there, err := m.volumesThere(ctx, driver, rec)
		if err != nil {
			return err
		}
		if there != len(volumesOf(rec)) {
			if _, err := os.Stat(m.dumpPath(rec)); err == nil {
				return nil // it holds no more than its last dump does, as after a restore
			}
			return errors.New("it is asleep, and has neither a volume nor a dump")
		}
		password, err := m.password(ctx, rec)
		if err != nil {
			return err
		}
		spec := m.privateSpec(rec, password)
		if err := driver.RunService(ctx, spec); err != nil {
			return err
		}
		defer driver.StopService(spec.Name, stopLimit(rec))
		if err := m.waitReady(ctx, driver, rec); err != nil {
			return err
		}
	} else if running, err := driver.ServiceRunning(ctx, name); err != nil {
		return err
	} else if !running {
		// It may be having its own dump put back, and part of that is no replacement for it.
		return errors.New("it is not running just now: the last dump stays, and the next backup has it")
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
	if err := driver.ExecServiceTo(limit, name, nil, temp, dumpCommand(rec)...); err != nil {
		return err
	}
	// It is what a restore has to go on: on the disk, and all there, before it replaces one that is.
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := dumpWhole(temp.Name(), rec.Type); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes a rename last.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Windows cannot do this to a directory, and does not need it to.
	if err := d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

// dumpEnd is the last thing that pg_dumpall writes.
const dumpEnd = "-- PostgreSQL database cluster dump complete"

// dumpWhole says whether a dump is all there. One cut short would load without complaint, as PostgreSQL takes the end of its input as the end of a table's rows.
func dumpWhole(path, kind string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	tail := make([]byte, min(info.Size(), 1024))
	if _, err := f.ReadAt(tail, info.Size()-int64(len(tail))); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if kind != manifest.ServiceS3 {
		if !bytes.Contains(tail[max(0, len(tail)-256):], []byte(dumpEnd)) {
			return errors.New("its dump is not whole: it does not get to the end")
		}
		return nil
	}
	// A tar ends in two blocks of nothing, and each thing in it says how long it is.
	if len(tail) < 1024 || len(bytes.Trim(tail, "\x00")) != 0 {
		return errors.New("its dump is not whole: it does not get to the end")
	}
	reader := tar.NewReader(f)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for {
		if _, err := reader.Next(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("its dump is not whole: %w", err)
		}
	}
}

func dumpCommand(rec store.Service) []string {
	if rec.Type == manifest.ServiceS3 {
		return []string{"tar", "-C", "/data", "-cf", "-", "s3", "meta"}
	}
	// All of it: an app can make other databases and roles, and a policy can name them, as it has the rights to.
	return []string{"pg_dumpall", "-U", postgresUser}
}

func loadCommand(rec store.Service) []string {
	if rec.Type == manifest.ServiceS3 {
		return []string{"tar", "-C", "/data", "-xf", "-"}
	}
	return []string{"psql", "-U", postgresUser, "-d", postgresUser, "-q", "-v", "ON_ERROR_STOP=1"}
}

// load puts a dump into a service's container, which has only just been given a fresh volume. A dump holds the password of what it came from, which is not always this one's.
func (m *Manager) load(ctx context.Context, driver *container.Driver, rec store.Service, name, path, password string) error {
	if err := dumpWhole(path, rec.Type); err != nil {
		return err
	}
	f, err := os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return err
	}
	defer f.Close()
	limit, cancel := context.WithTimeout(ctx, dataLimit)
	defer cancel()
	if rec.Type != manifest.ServicePostgres {
		return driver.ExecServiceFrom(limit, name, nil, f, loadCommand(rec)...)
	}
	dump := withoutWhatIsThere(f)
	defer dump.Close()
	if err := driver.ExecServiceFrom(limit, name, nil, dump, loadCommand(rec)...); err != nil {
		return err
	}
	// By its input, and so not on a command line.
	statement := fmt.Sprintf("ALTER ROLE %s WITH PASSWORD '%s';\n", postgresUser, password)
	return driver.ExecServiceFrom(limit, name, nil, strings.NewReader(statement), loadCommand(rec)...)
}

// withoutWhatIsThere leaves out of a dump the two statements that make what a new service already has, which would otherwise stop it there. Rows of data can look like
// anything, so that it knows when it is among them.
func withoutWhatIsThere(r io.Reader) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		in := bufio.NewReaderSize(r, 1<<16)
		copying := false
		for {
			line, err := in.ReadBytes('\n')
			if len(line) > 0 {
				text := bytes.TrimRight(line, "\r\n")
				switch {
				case copying:
					copying = !bytes.Equal(text, []byte(`\.`))
				case bytes.HasPrefix(text, []byte("COPY ")) && bytes.HasSuffix(text, []byte("FROM stdin;")):
					copying = true
				case bytes.Equal(text, []byte("CREATE ROLE "+postgresUser+";")), bytes.HasPrefix(text, []byte("CREATE DATABASE "+postgresUser+" ")):
					line = nil
				}
				if len(line) > 0 {
					if _, werr := writer.Write(line); werr != nil {
						return
					}
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				writer.CloseWithError(err)
				return
			}
		}
	}()
	return reader
}

// putBack gives a service with no volume what it held, from its dump. It does this where nothing can reach it, as an app that connected to an empty database would migrate it, and
// then its dump would not load, and the next backup would hold only that. It is noted first, so that if it does not get to the end, the next try starts again from nothing, however it ended.
func (m *Manager) putBack(ctx context.Context, driver *container.Driver, rec store.Service, password string) error {
	// Before anything is removed: a dump that will not do is no reason to lose what there may be.
	if err := dumpWhole(m.dumpPath(rec), rec.Type); err != nil {
		return err
	}
	if !rec.PendingLoad {
		if err := m.o.DB.SetServicePendingLoad(ctx, rec.ID, true); err != nil {
			return err
		}
	}
	spec := m.privateSpec(rec, password)
	// What is there, if anything, is what the last try left.
	if err := driver.StopService(spec.Name, 10*time.Second); err != nil {
		return err
	}
	if err := removeVolumes(driver, rec); err != nil {
		return err
	}
	if err := driver.RunService(ctx, spec); err != nil {
		return err
	}
	defer driver.StopService(spec.Name, stopLimit(rec))
	if err := m.waitReady(ctx, driver, rec); err != nil {
		return err
	}
	if err := m.load(ctx, driver, rec, spec.Name, m.dumpPath(rec), password); err != nil {
		return fmt.Errorf("load its dump: %w", err)
	}
	if err := driver.StopService(spec.Name, stopLimit(rec)); err != nil {
		return err
	}
	if err := m.o.DB.SetServicePendingLoad(ctx, rec.ID, false); err != nil {
		return err
	}
	m.o.Log.Info("a service has its dump back", "app", rec.App, "env", rec.Env, "service", rec.Name)
	return nil
}

// bringUp starts a service. If its volume has gone, as on a new machine after a restore, what it held comes back from its dump first. If it cannot be known whether its volume is
// there, nothing is done, as loading a dump over what it holds would lose what is newer.
func (m *Manager) bringUp(ctx context.Context, driver *container.Driver, rec store.Service, password string) error {
	there, err := m.volumesThere(ctx, driver, rec)
	if err != nil {
		return err
	}
	fresh := there == 0
	// One gone of two says that something has happened that nobody here understands, and what is left may be all that there is.
	if there > 0 && there < len(volumesOf(rec)) && !rec.PendingLoad {
		return fmt.Errorf("one of its volumes has gone, and the other is still there: moor services restore goes back to its latest dump, and loses that")
	}
	if fresh || rec.PendingLoad {
		if _, err := os.Stat(m.dumpPath(rec)); err == nil {
			if err := m.putBack(ctx, driver, rec, password); err != nil {
				return err
			}
			fresh = false
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			m.o.Log.Warn("a service has come up with nothing, and there is no dump of it", "app", rec.App, "env", rec.Env, "service", rec.Name)
			if rec.PendingLoad {
				if err := m.o.DB.SetServicePendingLoad(ctx, rec.ID, false); err != nil {
					return err
				}
			}
		}
	}
	if err := driver.RunService(ctx, m.spec(rec, password)); err != nil {
		return err
	}
	// A new volume of S3 holds no bucket, and a dump would have brought it.
	if fresh && rec.Type == manifest.ServiceS3 {
		if err := m.waitReady(ctx, driver, rec); err != nil {
			return err
		}
		return m.makeBucket(ctx, rec, password)
	}
	return nil
}

// volumesThere says how many of a service's volumes there are.
func (m *Manager) volumesThere(ctx context.Context, driver *container.Driver, rec store.Service) (int, error) {
	count := 0
	for _, name := range volumesOf(rec) {
		there, err := driver.VolumeThere(ctx, name)
		if err != nil {
			return 0, err
		}
		if there {
			count++
		}
	}
	return count, nil
}

// seed fills a new preview's service from production's latest dump, if the manifest asks for that, and it is safe. Copying production into previews is how customer data leaks, so
// that this is an opt-in, and it has to say that it has thought about what is in there. It happens where no app can reach it, and it is on record as not finished until it is, so
// that a server stopping halfway does not leave a copy that nothing has sanitized.
func (m *Manager) seed(ctx context.Context, driver *container.Driver, rec store.Service, s manifest.ServicePlan, appDir, password string, log io.Writer) error {
	path := dumpPathIn(m.o.DataDir, rec.App, names.Production, rec.Name, rec.Type)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(log, "▸ service    %s: not seeded from production, and it starts empty: there is no dump of it yet, which a backup makes\n", rec.Name)
		return nil
	}
	spec := m.privateSpec(rec, password)
	if err := driver.RunService(ctx, spec); err != nil {
		return err
	}
	if err := m.waitReady(ctx, driver, rec); err != nil {
		return err
	}
	fmt.Fprintf(log, "▸ service    %s: seeding it from production's latest dump\n", rec.Name)
	if err := m.load(ctx, driver, rec, spec.Name, path, password); err != nil {
		return fmt.Errorf("load production's dump: %w", err)
	}
	if s.Sanitize == manifest.SanitizeNone {
		return driver.StopService(spec.Name, stopLimit(rec))
	}
	// A build can leave anything at that path, and what is outside the app is not its to give.
	script, err := os.OpenInRoot(appDir, filepath.FromSlash(s.Sanitize))
	if err != nil {
		return fmt.Errorf("sanitize: %w", err)
	}
	defer script.Close()
	fmt.Fprintf(log, "▸ service    %s: sanitizing it with %s\n", rec.Name, s.Sanitize)
	limit, cancel := context.WithTimeout(ctx, dataLimit)
	defer cancel()
	if err := driver.ExecServiceFrom(limit, spec.Name, nil, script, loadCommand(rec)...); err != nil {
		return fmt.Errorf("sanitize: %w", err)
	}
	return driver.StopService(spec.Name, stopLimit(rec))
}

// seedRefused says why a preview should not have a copy of production's data.
func (m *Manager) seedRefused(ctx context.Context, app, env string, plan manifest.Plan) (string, error) {
	e, err := m.o.DB.Environment(ctx, app, env)
	if err != nil {
		return "", err
	}
	switch {
	case e.Kind != store.KindPreview:
		return "only a preview is seeded", nil
	case !e.Inherits:
		return "a coding agent's preview takes nothing of production's", nil
	}
	fork, err := m.o.DB.EnvironmentFromFork(ctx, app, env)
	if err != nil {
		return "", err
	}
	if fork {
		return "this is the preview of a pull request that may come from a fork", nil
	}
	access, err := m.o.DB.EnvironmentAccess(ctx, app, env)
	if err != nil {
		return "", err
	}
	mode, _ := m.o.GateFor(access.Mode, plan.AccessFor(env, e.Kind), e.Kind)
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
	if rec.State == store.ServiceSeeding {
		return "", &MajorChangeError{"it is still being made"}
	}
	password, err := m.password(ctx, rec)
	if err != nil {
		return "", err
	}
	// What is asleep stays asleep, and runs this when it wakes.
	if rec.State != store.ServiceRunning {
		return digest, m.o.DB.SetServiceDigest(ctx, rec.ID, digest)
	}
	if err := driver.StopService(container.ServiceName(rec.ID), stopLimit(rec)); err != nil {
		return "", err
	}
	if err := m.o.DB.SetServiceDigest(ctx, rec.ID, digest); err != nil {
		return "", err
	}
	rec.Digest = digest
	if err := driver.RunService(ctx, m.spec(rec, password)); err != nil {
		return "", err
	}
	if err := m.waitReady(ctx, driver, rec); err != nil {
		return "", err
	}
	return digest, nil
}

// Upgrade is a move to a newer major version that has been looked at, and can be gone ahead with.
type Upgrade struct {
	rec     store.Service
	driver  *container.Driver
	next    store.Service
	version string
}

// PrepareUpgrade finds out whether a service can move to a newer major version, and fetches what it needs for that, so that nothing is stopped for what would be refused. Another
// image than the official one has to be named, as moving to the official one would lose what it was there for.
func (m *Manager) PrepareUpgrade(ctx context.Context, app, env, name, version, image string, log io.Writer) (*Upgrade, error) {
	rec, err := m.o.DB.Service(ctx, app, env, name)
	if err != nil {
		return nil, err
	}
	if rec.Type != manifest.ServicePostgres {
		return nil, &MajorChangeError{"only PostgreSQL has versions to move between"}
	}
	if rec.OldVolume != "" {
		return nil, &MajorChangeError{fmt.Sprintf("it still has the volume from its last upgrade: moor services upgrade %s --finish lets it go", name)}
	}
	if rec.PendingLoad || rec.State != store.ServiceRunning {
		return nil, &MajorChangeError{"it is not running: wake it, or wait for it to start, and try again"}
	}
	if !newerVersion(version, rec.Version) {
		return nil, &MajorChangeError{fmt.Sprintf("it holds PostgreSQL %s, and %s is not newer: a dump from the newer does not load into the older", rec.Version, version)}
	}
	if image == "" {
		if rec.Image != fmt.Sprintf(postgresImage, rec.Version) {
			return nil, &MajorChangeError{fmt.Sprintf("it runs %s, which is not the official image: say which image holds PostgreSQL %s, with --image", rec.Image, version)}
		}
		image = fmt.Sprintf(postgresImage, version)
	} else {
		image = qualified(image)
	}
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return nil, err
	}
	engine := driver.Engine()
	fmt.Fprintf(log, "pulling %s\n", image)
	if err := engine.Pull(ctx, image, log, log); err != nil {
		return nil, err
	}
	info, err := engine.InspectImage(ctx, image)
	if err != nil {
		return nil, err
	}
	if major := postgresMajor(info); major != version {
		return nil, &MajorChangeError{fmt.Sprintf("%s holds PostgreSQL %s", image, major)}
	}
	next := rec
	next.Version, next.Image, next.Digest, next.Mount, next.Volume = version, image, repoDigest(info), dataMount(info, version), container.ServiceUpgradeVolume(rec.ID, version)
	return &Upgrade{rec: rec, driver: driver, next: next, version: version}, nil
}

// Upgrade moves a PostgreSQL service to a newer major version: a dump with the newer version's tools, a new volume, a restore and a check. The images hold only their own major
// version, so that pg_upgrade is not available. The old volume stays until someone says that this was good, and anything going wrong leaves the old one running as it was.
// What connects to it has to be stopped first, or what is written after the dump is lost.
func (m *Manager) Upgrade(ctx context.Context, u *Upgrade, log io.Writer) error {
	rec, next, driver := u.rec, u.next, u.driver
	unlock := m.lock(rec.App + "/" + rec.Env)
	defer unlock()
	password, err := m.password(ctx, rec)
	if err != nil {
		return err
	}
	old := container.ServiceName(rec.ID)
	spec := m.privateSpec(next, password)
	spec.Name += "-upgrade"
	temp := spec.Name
	// What a try that the server did not outlive left, as the record has never named it.
	if err := driver.StopService(temp, 10*time.Second); err != nil {
		return err
	}
	if err := driver.RemoveVolume(next.Volume); err != nil {
		return err
	}
	if err := driver.RunService(ctx, spec); err != nil {
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
	fmt.Fprintf(log, "dumping PostgreSQL %s with the tools of %s, and restoring it\n", rec.Version, u.version)
	limit, cancel := context.WithTimeout(ctx, dataLimit)
	defer cancel()
	if err := moveData(limit, driver, old, temp, password); err != nil {
		return failed(fmt.Errorf("move its data: %w", err))
	}
	had, err := driver.ExecService(limit, old, nil, "psql", "-U", postgresUser, "-d", postgresUser, "-At", "-c", holdsQuery)
	if err != nil {
		return failed(fmt.Errorf("check it: %w", err))
	}
	has, err := driver.ExecService(limit, temp, nil, "psql", "-U", postgresUser, "-d", postgresUser, "-At", "-c", holdsQuery)
	if err != nil {
		return failed(fmt.Errorf("check it: %w", err))
	}
	if strings.TrimSpace(had) != strings.TrimSpace(has) {
		return failed(fmt.Errorf("it had %s, and the new one has %s", strings.TrimSpace(had), strings.TrimSpace(has)))
	}
	fmt.Fprintf(log, "it holds %s, as it did\n", strings.TrimSpace(has))
	if err := driver.StopService(temp, stopLimit(next)); err != nil {
		return failed(err)
	}
	if err := driver.StopService(old, stopLimit(rec)); err != nil {
		driver.RemoveVolume(next.Volume)
		return err
	}
	// The record first: from here the new volume has everything, so that a server stopping now comes back on it.
	if err := m.o.DB.SetServiceImage(ctx, rec.ID, next.Version, next.Image, next.Digest, next.Mount, next.Volume, rec.Volume); err != nil {
		driver.RunService(ctx, m.spec(rec, password))
		driver.RemoveVolume(next.Volume)
		return err
	}
	err = driver.RunService(ctx, m.spec(next, password))
	if err == nil {
		err = m.waitReady(ctx, driver, next)
	}
	if err == nil {
		return nil
	}
	fmt.Fprintf(log, "it did not come up, and goes back: %v\n", err)
	driver.StopService(old, 10*time.Second)
	if undo := m.o.DB.SetServiceImage(ctx, rec.ID, rec.Version, rec.Image, rec.Digest, rec.Mount, rec.Volume, ""); undo != nil {
		return fmt.Errorf("go back to what it was: %w", undo)
	}
	if err := driver.RunService(ctx, m.spec(rec, password)); err != nil {
		return fmt.Errorf("go back to what it was: %w", err)
	}
	driver.RemoveVolume(next.Volume)
	return errors.New("the new version did not come up, and it has gone back to what it was")
}

// holdsQuery says how much a database holds, for what a move has to have moved. It counts, as what PostgreSQL keeps of the number of rows of a large table is an estimate. What it
// says of the other databases is only that they are there.
const holdsQuery = `select (select coalesce(sum((xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', schemaname, tablename), false, true, '')))[1]::text::bigint), 0)
		from pg_tables where schemaname not in ('pg_catalog', 'information_schema'))
	|| ' rows in ' || (select count(*) from pg_tables where schemaname not in ('pg_catalog', 'information_schema'))
	|| ' tables, of databases ' || (select string_agg(datname, ', ' order by datname) from pg_database where not datistemplate)`

// moveData dumps one service into another with the other's tools, on their network.
func moveData(ctx context.Context, driver *container.Driver, from, to, password string) error {
	reader, writer := io.Pipe()
	env := map[string]string{"PGPASSWORD": password}
	dumped := make(chan error, 1)
	go func() {
		err := driver.ExecServiceTo(ctx, to, env, writer, "pg_dumpall", "-h", from, "-U", postgresUser)
		writer.CloseWithError(err)
		dumped <- err
	}()
	filtered := withoutWhatIsThere(reader)
	err := driver.ExecServiceFrom(ctx, to, nil, filtered, "psql", "-U", postgresUser, "-d", postgresUser, "-q", "-v", "ON_ERROR_STOP=1")
	filtered.Close()
	reader.CloseWithError(err)
	return errors.Join(err, <-dumped)
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
	return m.o.DB.SetServiceImage(ctx, rec.ID, rec.Version, rec.Image, rec.Digest, rec.Mount, rec.Volume, "")
}

// CheckRestore says why a service cannot go back to its latest dump, so that nothing need stop to find out.
func (m *Manager) CheckRestore(ctx context.Context, app, env, name string) error {
	rec, err := m.o.DB.Service(ctx, app, env, name)
	if err != nil {
		return err
	}
	if rec.State == store.ServiceSeeding {
		return &MajorChangeError{"it is still being made"}
	}
	if rec.OldVolume != "" {
		return &MajorChangeError{fmt.Sprintf("it still has the volume from before: moor services upgrade %s --finish lets it go", name)}
	}
	path := m.dumpPath(rec)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return &MajorChangeError{"there is no dump of it to go back to, which a backup makes"}
	}
	if err := dumpWhole(path, rec.Type); err != nil {
		return &MajorChangeError{err.Error()}
	}
	return nil
}

// Restore puts a service back to its latest dump, which on the machine that it ran on, a restore of a backup does not do by itself, as what it holds may be newer. A dump can fail
// to load, so that PostgreSQL's goes into a volume of its own first, and what it held is kept until someone says that this was good, as after an upgrade.
func (m *Manager) Restore(ctx context.Context, app, env, name string) error {
	unlock := m.lock(app + "/" + env)
	defer unlock()
	if err := m.CheckRestore(ctx, app, env, name); err != nil {
		return err
	}
	rec, err := m.o.DB.Service(ctx, app, env, name)
	if err != nil {
		return err
	}
	path := m.dumpPath(rec)
	driver, err := m.driverFor(ctx, rec)
	if err != nil {
		return err
	}
	password, err := m.password(ctx, rec)
	if err != nil {
		return err
	}
	if rec.Type == manifest.ServiceS3 {
		if err := driver.StopService(container.ServiceName(rec.ID), stopLimit(rec)); err != nil {
			return err
		}
		if err := m.putBack(ctx, driver, rec, password); err != nil {
			return err
		}
	} else {
		next := rec
		next.Volume = container.ServiceRestoredVolume(rec.ID, time.Now().UnixMilli())
		spec := m.privateSpec(next, password)
		spec.Name += "-restore"
		if err := driver.RunService(ctx, spec); err != nil {
			return err
		}
		defer driver.StopService(spec.Name, 10*time.Second)
		failed := func(err error) error {
			driver.StopService(spec.Name, 10*time.Second)
			driver.RemoveVolume(next.Volume)
			return err
		}
		if err := m.waitReadyNamed(ctx, driver, next, spec.Name); err != nil {
			return failed(err)
		}
		if err := m.load(ctx, driver, next, spec.Name, path, password); err != nil {
			return failed(fmt.Errorf("load its dump, and it goes on as it was: %w", err))
		}
		if err := driver.StopService(spec.Name, stopLimit(next)); err != nil {
			return failed(err)
		}
		if err := driver.StopService(container.ServiceName(rec.ID), stopLimit(rec)); err != nil {
			return failed(err)
		}
		if err := m.o.DB.SetServiceImage(ctx, rec.ID, rec.Version, rec.Image, rec.Digest, rec.Mount, next.Volume, rec.Volume); err != nil {
			return failed(err)
		}
		rec = next
	}
	if rec.State != store.ServiceRunning {
		return nil
	}
	if err := driver.RunService(ctx, m.spec(rec, password)); err != nil {
		return err
	}
	return m.waitReady(ctx, driver, rec)
}

func newerVersion(version, than string) bool {
	a, err := strconv.Atoi(version)
	if err != nil {
		return false
	}
	b, err := strconv.Atoi(than)
	return err == nil && a > b
}
