package platform

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
)

// WalkRoot goes through a directory and all that is in it, for acting on what may belong to someone who is still
// running. Each thing is handed over with a handle on the directory it is in, and its name there, so that nothing is
// reached by a path that could be changed on the way: a directory swapped for a link goes nowhere. What goes away
// during the walk is passed over, as an app's temporary files do, and names need not be valid text.
//
// visit gets "." for the directory itself, and may return fs.SkipDir to leave a directory out.
func WalkRoot(dir string, visit func(at *os.Root, name, rel string, isDir bool) error) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer root.Close()
	if err := visit(root, ".", ".", true); err != nil {
		if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return walkBelow(root, "", visit)
}

func walkBelow(at *os.Root, rel string, visit func(at *os.Root, name, rel string, isDir bool) error) error {
	f, err := at.Open(".")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		below := path.Join(rel, name)
		if err := visit(at, name, below, e.IsDir()); err != nil {
			if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if !e.IsDir() {
			continue
		}
		sub, err := at.OpenRoot(name)
		if err != nil {
			// Gone, or no longer a directory: it was swapped after being listed, and there is nothing to go into.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if info, statErr := at.Lstat(name); statErr != nil || !info.IsDir() {
				continue
			}
			return err
		}
		err = walkBelow(sub, below, visit)
		sub.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
