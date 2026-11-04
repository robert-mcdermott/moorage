// Package upgrade replaces a server's binary with a newer release, once it has shown that it is what
// the project published, and puts everything back if the new version does not come up.
package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"crypto/ed25519"

	"github.com/robert-mcdermott/moorage/internal/store"
)

// RefusedError says why nothing was done, in a way that is meant to be read.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return e.Reason }

// Service is what starts and stops the server, if anything does.
type Service interface {
	Running() bool
	Start() error
	Stop() error
	Restart() error
}

// Options say what to upgrade.
type Options struct {
	// Executable is the binary to replace, and Current the version it is.
	Executable, Current string
	// DataDir and Database are the server's.
	DataDir, Database string
	// Source is where the release comes from, and Version which one: empty is the newest.
	Source  Source
	Version string
	// Keys are those a release has to be signed with. With none, it only has to match its checksum.
	Keys []ed25519.PublicKey
	// Service runs the server, or is nil, when ServerUp says whether one is running by other means.
	Service  Service
	ServerUp func() bool
	// Healthy waits for the server to answer.
	Healthy func(wait time.Duration) bool
	// Wait is how long a new version has to come up.
	Wait time.Duration
	// Check only says what would happen, and Force goes back even when that costs what has happened since.
	Check, Force bool
	// GOOS and GOARCH say which binary, and Say tells someone how it is going.
	GOOS, GOARCH string
	Say          func(format string, args ...any)
	Now          func() time.Time
}

func (o *Options) defaults() {
	if o.GOOS == "" {
		o.GOOS, o.GOARCH = runtime.GOOS, runtime.GOARCH
	}
	if o.Say == nil {
		o.Say = func(string, ...any) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Wait == 0 {
		o.Wait = 2 * time.Minute
	}
}

// Result says what happened.
type Result struct {
	From, To string
	// Available is what Check found: a newer version, if To is set.
	Available bool
	// Current says that this is already the version asked for.
	Current bool
	// Signed says that the release's signature was checked.
	Signed bool
	// Restarted says that the server now runs the new version, and Stopped that it was not running.
	Restarted, Stopped bool
	// Restored says that the database went back, and Aside is where it was put.
	Restored bool
	Aside    string
}

// state is what is kept to go back with.
type state struct {
	From       string    `json:"from"`
	To         string    `json:"to"`
	At         time.Time `json:"at"`
	Executable string    `json:"executable"`
	Previous   string    `json:"previous"`
	// Snapshot is the database just before, and Schema the version of its schema.
	Snapshot string `json:"snapshot,omitempty"`
	Schema   int    `json:"schema,omitempty"`
}

func stateDir(dataDir string) string  { return filepath.Join(dataDir, "upgrades") }
func statePath(dataDir string) string { return filepath.Join(stateDir(dataDir), "last.json") }

func previousPath(exe string) string {
	if strings.HasSuffix(exe, ".exe") {
		return strings.TrimSuffix(exe, ".exe") + ".previous.exe"
	}
	return exe + ".previous"
}

func writeState(dataDir string, st state) error {
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dataDir))
}

func readState(dataDir string) (state, error) {
	var st state
	body, err := os.ReadFile(statePath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return st, &RefusedError{"there is nothing to go back to: this command has not upgraded this server, or has already gone back"}
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(body, &st)
}

// Upgrade brings in a release and has the server run it. If the server does not come up on it, it goes
// back by itself, and says so in the error.
func Upgrade(ctx context.Context, o Options) (Result, error) {
	o.defaults()
	res := Result{From: o.Current}
	if err := o.Source.check(); err != nil {
		return res, err
	}
	want := o.Version
	if want == "" {
		latest, err := o.Source.Latest(ctx)
		if err != nil {
			return res, err
		}
		want = latest
	}
	if want != "" {
		res.To = want
		if done, err := o.compare(&res, want); done || err != nil {
			return res, err
		}
		if o.Check {
			res.Available = true
			return res, nil
		}
	}

	// Find out that it cannot be replaced before downloading anything.
	info, err := os.Stat(o.Executable)
	if err != nil {
		return res, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(o.Executable), ".moor-upgrade-")
	if errors.Is(err, fs.ErrPermission) {
		return res, &RefusedError{fmt.Sprintf("this user cannot replace %s: run this as a user who can, as with sudo", o.Executable)}
	}
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(staging)

	new, err := o.Source.stage(ctx, staging, want, o.GOOS, o.GOARCH, o.Keys, info.Mode().Perm(), o.Say)
	if err != nil {
		return res, err
	}
	res.To, res.Signed = new.Version, new.Signed
	if done, err := o.compare(&res, new.Version); done || err != nil {
		return res, err
	}
	if o.Check {
		res.Available = true
		return res, nil
	}

	st := state{From: o.Current, To: new.Version, At: o.Now(), Executable: o.Executable, Previous: previousPath(o.Executable)}
	if err := os.MkdirAll(stateDir(o.DataDir), 0o700); err != nil {
		return res, err
	}
	if err := o.snapshot(ctx, &st); err != nil {
		return res, err
	}
	if err := swapIn(o.Executable, new.Path, st.Previous); err != nil {
		return res, fmt.Errorf("put the new binary in place: %w", err)
	}
	if err := writeState(o.DataDir, st); err != nil {
		// Nothing can go back without this, so it does not go forward without it.
		if back := swapBack(o.Executable, st.Previous); back != nil {
			return res, fmt.Errorf("%w; and the previous binary could not be put back: it is at %s", err, st.Previous)
		}
		return res, err
	}

	if o.Service == nil {
		res.Stopped = o.ServerUp == nil || !o.ServerUp()
		return res, nil
	}
	if !o.Service.Running() {
		res.Stopped = true
		return res, nil
	}
	o.Say("restarting the server")
	restarted := o.Service.Restart()
	if restarted == nil && o.Healthy(o.Wait) {
		res.Restarted = true
		return res, nil
	}
	reason := "it did not answer"
	if restarted != nil {
		reason = restarted.Error()
	}
	o.Say("the server did not come up on %s (%s): going back to %s", new.Version, reason, o.Current)
	backed, err := goBack(ctx, o, st, true, true)
	if err != nil {
		return res, fmt.Errorf("the server did not come up on %s (%s), and going back failed too: %w", new.Version, reason, err)
	}
	res.Restored, res.Aside = backed.Restored, backed.Aside
	return res, &RefusedError{fmt.Sprintf("the server did not come up on %s (%s), so it is back on %s", new.Version, reason, o.Current)}
}

// compare ends an upgrade that has nowhere to go.
func (o Options) compare(res *Result, to string) (bool, error) {
	order, err := CompareVersions(to, o.Current)
	if err != nil {
		return false, err
	}
	switch {
	case order == 0:
		res.Current = true
		return true, nil
	case order < 0:
		return true, &RefusedError{fmt.Sprintf("%s is older than this, %s: a database moves forward with each version, and only --rollback can take it back", to, o.Current)}
	}
	return false, nil
}

// snapshot keeps the database as it is now, in a place that only this upgrade uses. The one from the
// last upgrade goes only when this one has worked.
func (o Options) snapshot(ctx context.Context, st *state) error {
	if _, err := os.Stat(o.Database); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	path := filepath.Join(stateDir(o.DataDir), "moorage-before-"+st.To+".db")
	_ = os.Remove(path) // from going to this version before
	schema, err := store.SnapshotDatabase(ctx, o.Database, path)
	if err != nil {
		return fmt.Errorf("keep a copy of the database: %w", err)
	}
	st.Snapshot, st.Schema = path, schema
	o.Say("kept a copy of the database")
	old, _ := filepath.Glob(filepath.Join(stateDir(o.DataDir), "moorage-before-*.db"))
	for _, name := range old {
		if name != path {
			_ = os.Remove(name)
		}
	}
	return nil
}

// Rollback puts back the binary from before the last upgrade, and the database only if that binary
// could not read it as it is now, since that loses what has happened since.
func Rollback(ctx context.Context, o Options) (Result, error) {
	o.defaults()
	st, err := readState(o.DataDir)
	if err != nil {
		return Result{}, err
	}
	if st.Executable != o.Executable {
		return Result{}, &RefusedError{fmt.Sprintf("the last upgrade replaced %s, and this is %s: run that one", st.Executable, o.Executable)}
	}
	if _, err := os.Stat(st.Previous); err != nil {
		return Result{}, &RefusedError{fmt.Sprintf("the previous binary is not at %s, so there is nothing to go back to: run the installer with the version you want", st.Previous)}
	}
	return goBack(ctx, o, st, o.Force, false)
}

// goBack puts things back. A version that has just failed leaves a server that is not running, by no choice of
// anyone's, so that coming back from one starts it whatever state it is in. Otherwise a server is left as it was.
func goBack(ctx context.Context, o Options, st state, force, start bool) (Result, error) {
	res := Result{From: st.To, To: st.From}
	restore := false
	if st.Snapshot != "" {
		// A database that has moved on cannot be read by the version before.
		schema, err := store.SchemaOf(ctx, o.Database)
		if err != nil {
			return res, err
		}
		restore = schema > st.Schema
	}
	if restore {
		if _, err := os.Stat(st.Snapshot); err != nil {
			return res, &RefusedError{fmt.Sprintf("the database has moved on since the upgrade, and its copy from before is gone from %s: restore a backup", st.Snapshot)}
		}
		if since := o.Now().Sub(st.At); since > 10*time.Minute && !force {
			return res, &RefusedError{fmt.Sprintf("%s changed the database, which has to go back to how it was at %s, so that everything since then is lost from it: --force if that is what you want", st.To, st.At.Format(time.RFC1123))}
		}
	}

	if o.Service != nil {
		running := o.Service.Running()
		switch {
		case running:
			o.Say("stopping the server")
			if err := o.Service.Stop(); err != nil {
				return res, err
			}
			start = true
		case start:
			// It may be starting again and again, which should not go on while its files change.
			_ = o.Service.Stop()
		}
	} else if o.ServerUp != nil && o.ServerUp() {
		return res, &RefusedError{"the server is running: stop it, and do this again"}
	}

	if restore {
		aside, err := restoreDatabase(o.Database, st, o.Now())
		if err != nil {
			return res, err
		}
		res.Restored, res.Aside = true, aside
	}
	if err := swapBack(o.Executable, st.Previous); err != nil {
		return res, fmt.Errorf("put the previous binary back: %w", err)
	}
	if err := os.Remove(statePath(o.DataDir)); err != nil {
		return res, err
	}
	if !start || o.Service == nil {
		res.Stopped = true
		return res, nil
	}
	if err := o.Service.Start(); err != nil {
		return res, err
	}
	// The version that was running before has no reason to be slow, and this is no time to give up on it.
	if !o.Healthy(max(o.Wait, time.Minute)) {
		return res, fmt.Errorf("the previous binary is back, but the server does not answer")
	}
	res.Restarted = true
	return res, nil
}

// restoreDatabase moves the database as it is to one side, for anyone who wants what is in it, and puts
// the copy from before the upgrade in its place.
func restoreDatabase(database string, st state, at time.Time) (string, error) {
	aside, err := os.MkdirTemp(filepath.Dir(st.Snapshot), "rolled-back-"+at.UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return "", err
	}
	copied := filepath.Join(aside, ".incoming")
	if err := copyFile(st.Snapshot, copied); err != nil {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm", ""} {
		err := os.Rename(database+suffix, filepath.Join(aside, filepath.Base(database)+suffix))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	if err := os.Rename(copied, database); err != nil {
		return "", err
	}
	return aside, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if closed := out.Close(); err == nil {
		err = closed
	}
	return err
}

// swapIn puts a new binary in place and keeps the old one. Where it can, this is done so that there is
// never a moment with no binary there, in case something starts it just then.
func swapIn(exe, staged, previous string) error {
	if err := clearPrevious(previous); err != nil {
		return err
	}
	if runtime.GOOS != "windows" && os.Link(exe, previous) == nil {
		if err := os.Rename(staged, exe); err != nil {
			_ = os.Remove(previous)
			return err
		}
		return nil
	}
	if err := os.Rename(exe, previous); err != nil {
		return err
	}
	if err := os.Rename(staged, exe); err != nil {
		if back := os.Rename(previous, exe); back != nil {
			return fmt.Errorf("%w; and the previous binary could not be put back: it is at %s", err, previous)
		}
		return err
	}
	return nil
}

// clearPrevious makes room. A binary can still be running on Windows, where it cannot be removed, but
// can be moved, and the installer clears away what it finds with this name.
func clearPrevious(previous string) error {
	err := os.Remove(previous)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if runtime.GOOS != "windows" {
		return err
	}
	base := strings.TrimSuffix(previous, ".exe")
	return os.Rename(previous, fmt.Sprintf("%s.%d.exe", base, time.Now().UnixNano()))
}

// swapBack puts the previous binary where the current one is.
func swapBack(exe, previous string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(previous, exe)
	}
	// The one going away may be running, which it can go on doing under another name.
	aside := fmt.Sprintf("%s.%d.exe", strings.TrimSuffix(previous, ".exe"), time.Now().UnixNano())
	if err := os.Rename(exe, aside); err != nil {
		return err
	}
	if err := os.Rename(previous, exe); err != nil {
		_ = os.Rename(aside, exe)
		return err
	}
	_ = os.Remove(aside)
	return nil
}
