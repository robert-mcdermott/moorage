package backup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Stored is a backup at a destination.
type Stored struct {
	Name string
	Size int64
	At   time.Time
}

// Destination is where backups go.
type Destination interface {
	// Put keeps what r gives under name, all of it or none of it.
	Put(ctx context.Context, name string, r io.Reader) error
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// List lists the backups there, newest first.
	List(ctx context.Context) ([]Stored, error)
	Delete(ctx context.Context, name string) error
	String() string
}

// Credentials are an S3-compatible service's.
type Credentials struct {
	Endpoint, Region, AccessKey, SecretKey string
}

// Parse reads a destination: a path, or s3://bucket/prefix.
func Parse(dest string, creds Credentials) (Destination, error) {
	if rest, ok := strings.CutPrefix(dest, "s3://"); ok {
		bucket, prefix, _ := strings.Cut(rest, "/")
		if bucket == "" {
			return nil, &RefusedError{"an S3 destination needs a bucket: s3://bucket/prefix"}
		}
		if creds.AccessKey == "" || creds.SecretKey == "" {
			return nil, &RefusedError{"an S3 destination needs an access key and a secret key"}
		}
		return &S3{Bucket: bucket, Prefix: strings.Trim(prefix, "/"), Credentials: creds}, nil
	}
	if dest == "" {
		return nil, &RefusedError{"no destination has been set"}
	}
	if !filepath.IsAbs(dest) {
		abs, err := filepath.Abs(dest)
		if err != nil {
			return nil, err
		}
		dest = abs
	}
	return &Local{Dir: dest}, nil
}

// Local keeps backups in a directory.
type Local struct{ Dir string }

func (l *Local) String() string { return l.Dir }

// Put writes beside where it goes, and then moves it there.
func (l *Local) Put(_ context.Context, name string, r io.Reader) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(l.Dir, name+".partial-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(l.Dir, name))
}

// Get opens a backup.
func (l *Local) Get(_ context.Context, name string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(l.Dir, name))
}

// List lists the backups there, newest first.
func (l *Local) List(context.Context) ([]Stored, error) {
	entries, err := os.ReadDir(l.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Stored
	for _, e := range entries {
		if !isBackupName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Stored{e.Name(), info.Size(), info.ModTime()})
	}
	sortStored(out)
	return out, nil
}

// Delete removes a file there.
func (l *Local) Delete(_ context.Context, name string) error {
	err := os.Remove(filepath.Join(l.Dir, name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	return nil
}

func sortStored(list []Stored) {
	slices.SortFunc(list, func(a, b Stored) int { return strings.Compare(b.Name, a.Name) })
}

// Open opens a backup by where it is: the path of a file, or an s3:// address. It also says what the
// backup should hash to, if something beside it says.
func Open(ctx context.Context, source string, creds Credentials) (io.ReadCloser, string, error) {
	var dest Destination
	var name string
	if rest, ok := strings.CutPrefix(source, "s3://"); ok {
		cut := strings.LastIndex(rest, "/")
		if cut < 0 {
			return nil, "", &RefusedError{"name a backup in the bucket: s3://bucket/prefix/" + namePrefix + "…" + suffix}
		}
		name = rest[cut+1:]
		var err error
		if dest, err = Parse("s3://"+rest[:cut], creds); err != nil {
			return nil, "", err
		}
	} else {
		abs, err := filepath.Abs(source)
		if err != nil {
			return nil, "", err
		}
		dest, name = &Local{Dir: filepath.Dir(abs)}, filepath.Base(abs)
	}
	r, err := dest.Get(ctx, name)
	if err != nil {
		return nil, "", err
	}
	digest := ""
	if d, err := dest.Get(ctx, name+digestSuffix); err == nil {
		data, _ := io.ReadAll(io.LimitReader(d, 4096))
		d.Close()
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			digest = fields[0]
		}
	}
	return r, digest, nil
}
