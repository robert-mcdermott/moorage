package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/store"
)

const (
	// MaxDataFile is the largest file that can be put in a data directory from outside it.
	MaxDataFile = 1 << 30
	// maxDataListing is the most of a directory that a listing shows.
	maxDataListing = 5000
)

// FileProblem says why something cannot be done with a data file, in words for whoever asked.
type FileProblem struct {
	Msg string
	// Missing says that there is nothing there, and Conflict that something is.
	Missing, Conflict bool
}

func (e *FileProblem) Error() string { return e.Msg }

// DataFile is something in a data directory.
type DataFile struct {
	Name string
	// Dir says that it is a directory, Link that it is a symbolic link, and Other that it is something else that is
	// not a file, which an app can leave there.
	Dir, Link, Other bool
	Size             int64
	Modified         time.Time
}

// dataPath is where someone means, within a data directory. It keeps to forward slashes, and "." is the top.
func dataPath(p string) (string, error) {
	if strings.IndexByte(p, 0) >= 0 || strings.Contains(p, "\\") {
		return "", &FileProblem{Msg: "That is not a path that can be used."}
	}
	p = path.Clean("/" + p)[1:]
	if p == "" {
		return ".", nil
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", &FileProblem{Msg: "That is not a path within the data directory."}
	}
	return p, nil
}

func (d *Deployer) dataDirOf(app, env string) string {
	return filepath.Join(d.opts.DataDir, "data", app, env)
}

// dataExists says whether an environment is there to have a data directory, which is what keeps a path out of its name, and
// something being put where an app will find it later.
func (d *Deployer) dataExists(ctx context.Context, app, env string) error {
	if _, err := d.db.Environment(ctx, app, env); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &FileProblem{Msg: "There is no environment " + env + " of " + app + ".", Missing: true}
		}
		return err
	}
	return nil
}

// dataRoot opens a data directory so that nothing that an app has left there can lead out of it: what an app writes is
// not to be trusted, and the server may be able to read anything. It answers with nil if there is no such directory yet.
func (d *Deployer) dataRoot(ctx context.Context, app, env string) (*os.Root, error) {
	if err := d.dataExists(ctx, app, env); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(d.dataDirOf(app, env))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return root, err
}

func fileFailure(p string, err error) error {
	var problem *FileProblem
	switch {
	case err == nil:
		return nil
	case errors.As(err, &problem):
		return problem
	case errors.Is(err, fs.ErrNotExist):
		return &FileProblem{Msg: "There is nothing at " + p + ".", Missing: true}
	case strings.Contains(err.Error(), "path escapes"):
		return &FileProblem{Msg: p + " leads out of the data directory, so it is not followed."}
	}
	return err
}

func describe(info fs.FileInfo) DataFile {
	f := DataFile{Name: info.Name(), Modified: info.ModTime().UTC()}
	switch mode := info.Mode(); {
	case mode.IsDir():
		f.Dir = true
	case mode&fs.ModeSymlink != 0:
		f.Link = true
	case mode.IsRegular():
		f.Size = info.Size()
	default:
		f.Other = true
	}
	return f
}

// DataFiles lists a directory within an environment's data directory, with directories first. It says if that is not all.
func (d *Deployer) DataFiles(ctx context.Context, app, env, p string) (files []DataFile, more bool, err error) {
	rel, err := dataPath(p)
	if err != nil {
		return nil, false, err
	}
	root, err := d.dataRoot(ctx, app, env)
	if err != nil {
		return nil, false, err
	}
	if root == nil {
		if rel == "." {
			return nil, false, nil
		}
		return nil, false, &FileProblem{Msg: "There is nothing at " + rel + ".", Missing: true}
	}
	defer root.Close()
	// Only a directory is opened: for a device, opening it can be enough to do something.
	if info, err := root.Stat(rel); err != nil {
		return nil, false, fileFailure(rel, err)
	} else if !info.IsDir() {
		return nil, false, &FileProblem{Msg: rel + " is not a directory."}
	}
	dir, err := root.OpenFile(rel, os.O_RDONLY|platform.OpenNonblock, 0)
	if err != nil {
		return nil, false, fileFailure(rel, err)
	}
	defer dir.Close()
	if info, err := dir.Stat(); err != nil {
		return nil, false, err
	} else if !info.IsDir() {
		return nil, false, &FileProblem{Msg: rel + " is not a directory."}
	}
	entries, err := dir.ReadDir(maxDataListing + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > maxDataListing {
		entries, more = entries[:maxDataListing], true
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue // it has just gone
		}
		f := describe(info)
		if f.Link {
			// A link to a directory is to be gone into, and one that leads away shows as what it is.
			if target, err := root.Stat(path.Join(rel, f.Name)); err == nil && target.IsDir() {
				f.Dir = true
			}
		}
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b DataFile) int {
		switch {
		case a.Dir && !b.Dir:
			return -1
		case b.Dir && !a.Dir:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return files, more, nil
}

// OpenDataFile opens a file within an environment's data directory, for reading. It is only ever a file: an app can leave
// a pipe there, which would wait for ever, a device, or a link to something that it may not read itself. Nor is it someone
// else's with another name, which it can be if the app linked something here that it could not read, on a system that allows it.
func (d *Deployer) OpenDataFile(ctx context.Context, app, env, p string) (*os.File, fs.FileInfo, error) {
	rel, err := dataPath(p)
	if err != nil {
		return nil, nil, err
	}
	root, err := d.dataRoot(ctx, app, env)
	if err != nil {
		return nil, nil, err
	}
	if root == nil {
		return nil, nil, &FileProblem{Msg: "There is nothing at " + rel + ".", Missing: true}
	}
	defer root.Close()
	if info, err := root.Stat(rel); err != nil {
		return nil, nil, fileFailure(rel, err)
	} else if !info.Mode().IsRegular() {
		return nil, nil, &FileProblem{Msg: rel + " is not a file."}
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|platform.OpenNonblock, 0)
	if err != nil {
		return nil, nil, fileFailure(rel, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, &FileProblem{Msg: rel + " is not a file."}
	}
	// Having another name is nothing out of the way for what an app made for itself, such as a snapshot. It only matters where
	// this server can read what the app could not, and of a file that belongs to someone else.
	if platform.Links(info) > 1 {
		if top, err := root.Stat("."); err == nil {
			if uid, _, privileged := platform.OwnedLike(top); privileged {
				if owner, known := platform.FileOwner(info); known && owner != uid {
					f.Close()
					return nil, nil, &FileProblem{Msg: rel + " has another name somewhere, and is not the app's own, so it is not sent."}
				}
			}
		}
	}
	return f, info, nil
}

// DataPathInfo says what is at a path, as it is if it is followed.
func (d *Deployer) DataPathInfo(ctx context.Context, app, env, p string) (DataFile, error) {
	rel, err := dataPath(p)
	if err != nil {
		return DataFile{}, err
	}
	root, err := d.dataRoot(ctx, app, env)
	if err != nil {
		return DataFile{}, err
	}
	if root == nil {
		return DataFile{}, &FileProblem{Msg: "There is nothing at " + rel + ".", Missing: true}
	}
	defer root.Close()
	info, err := root.Stat(rel)
	if err != nil {
		return DataFile{}, fileFailure(rel, err)
	}
	return describe(info), nil
}

// ownedData is the root of a data directory, made if it is not there yet, for writing to.
func (d *Deployer) ownedData(ctx context.Context, app, env string) (*os.Root, error) {
	if err := d.dataExists(ctx, app, env); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d.dataDirOf(app, env), 0o750); err != nil {
		return nil, err
	}
	return os.OpenRoot(d.dataDirOf(app, env))
}

// PutDataFile writes a file within an environment's data directory. It appears all at once, and belongs to whoever the
// directory belongs to, so that the app can do what it likes with it. The directory has to be there already.
func (d *Deployer) PutDataFile(ctx context.Context, app, env, p string, from io.Reader, replace bool) (DataFile, error) {
	rel, err := dataPath(p)
	if err != nil {
		return DataFile{}, err
	}
	if rel == "." {
		return DataFile{}, &FileProblem{Msg: "That is the data directory itself: say what the file is called."}
	}
	root, err := d.ownedData(ctx, app, env)
	if err != nil {
		return DataFile{}, err
	}
	defer root.Close()
	parent := path.Dir(rel)
	above, err := root.Stat(parent)
	if err != nil {
		return DataFile{}, fileFailure(parent, err)
	}
	if !above.IsDir() {
		return DataFile{}, &FileProblem{Msg: parent + " is not a directory."}
	}
	// A directory that anyone may write to is what a container's user is given, who may be anyone. They have to be able
	// to change what is put there for them.
	mode := fs.FileMode(0o644)
	if above.Mode().Perm()&0o002 != 0 {
		mode = 0o666
	}
	if there, err := root.Lstat(rel); err == nil {
		switch {
		case there.IsDir():
			return DataFile{}, &FileProblem{Msg: rel + " is a directory.", Conflict: true}
		case !replace:
			return DataFile{}, &FileProblem{Msg: "There is already something at " + rel + ".", Conflict: true}
		case there.Mode().IsRegular():
			mode = there.Mode().Perm()
		}
	}
	mark := make([]byte, 8)
	if _, err := rand.Read(mark); err != nil {
		return DataFile{}, err
	}
	temporary := path.Join(parent, ".moorage-upload-"+hex.EncodeToString(mark))
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return DataFile{}, fileFailure(parent, err)
	}
	defer root.Remove(temporary)
	written, err := io.Copy(f, io.LimitReader(from, MaxDataFile+1))
	if err == nil && written > MaxDataFile {
		err = &FileProblem{Msg: fmt.Sprintf("That is more than the %d GB that can be put here this way.", MaxDataFile>>30)}
	}
	if err == nil {
		if uid, gid, ok := platform.OwnedLike(above); ok {
			err = f.Chown(uid, gid)
		}
	}
	if err == nil {
		err = f.Chmod(mode) // whatever this process's umask has done to it
	}
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return DataFile{}, err
	}
	if err := root.Rename(temporary, rel); err != nil {
		return DataFile{}, fileFailure(rel, err)
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return DataFile{}, err
	}
	return describe(info), nil
}

// MakeDataDir makes a directory within an environment's data directory.
func (d *Deployer) MakeDataDir(ctx context.Context, app, env, p string) error {
	rel, err := dataPath(p)
	if err != nil {
		return err
	}
	if rel == "." {
		return &FileProblem{Msg: "Say what the directory is called.", Conflict: true}
	}
	root, err := d.ownedData(ctx, app, env)
	if err != nil {
		return err
	}
	defer root.Close()
	above, err := root.Stat(path.Dir(rel))
	if err != nil {
		return fileFailure(path.Dir(rel), err)
	}
	if err := root.Mkdir(rel, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return &FileProblem{Msg: "There is already something at " + rel + ".", Conflict: true}
		}
		return fileFailure(rel, err)
	}
	if above.Mode().Perm()&0o002 != 0 {
		if err := root.Chmod(rel, 0o777); err != nil {
			return err
		}
	}
	if uid, gid, ok := platform.OwnedLike(above); ok {
		return root.Lchown(rel, uid, gid)
	}
	return nil
}

// RemoveData removes something within an environment's data directory. A link goes, and not what it leads to. A directory
// with something in it takes being asked for all of it.
func (d *Deployer) RemoveData(ctx context.Context, app, env, p string, everything bool) error {
	rel, err := dataPath(p)
	if err != nil {
		return err
	}
	if rel == "." {
		return &FileProblem{Msg: "That is the data directory itself, which stays.", Conflict: true}
	}
	root, err := d.dataRoot(ctx, app, env)
	if err != nil {
		return err
	}
	if root == nil {
		return &FileProblem{Msg: "There is nothing at " + rel + ".", Missing: true}
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil {
		return fileFailure(rel, err)
	}
	if info.IsDir() && everything {
		return fileFailure(rel, root.RemoveAll(rel))
	}
	if err := root.Remove(rel); err != nil {
		if info.IsDir() && !errors.Is(err, fs.ErrNotExist) {
			return &FileProblem{Msg: rel + " has something in it.", Conflict: true}
		}
		return fileFailure(rel, err)
	}
	return nil
}
