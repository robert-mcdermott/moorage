// Package deploy turns uploads into builds and builds into releases: it
// stores uploaded sites and artifacts, builds uploaded source, and runs the
// deployments that publish them (spec/04-build-and-deploy.md).
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/archive"
	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/ingress/static"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// partialSuffix marks a build directory still being written.
const partialSuffix = ".partial"

// Content directories inside a build: a static site's files, or an app's.
const (
	SiteDir = "site"
	AppDir  = "app"
)

// Upload stores an uploaded directory (a gzip-compressed tar stream) as a
// new build of app, following plan, and creates the app if it does not
// exist. Nothing is stored unless the whole upload is valid. It reports
// whether the app was created.
func Upload(ctx context.Context, db *store.DB, dataDir, app string, plan manifest.Plan, upload io.Reader) (store.Build, bool, error) {
	return storeBuild(ctx, db, dataDir, app, &plan, func(contentDir string) (int, int64, error) {
		return archive.Extract(upload, contentDir, archive.DefaultLimits)
	})
}

// storeBuild keeps what fill writes to a directory as a new build of app, as Upload says. fill may
// change the plan.
func storeBuild(ctx context.Context, db *store.DB, dataDir, app string, planned *manifest.Plan, fill func(contentDir string) (files int, size int64, err error)) (store.Build, bool, error) {
	plan := *planned
	if err := names.ValidateApp(app); err != nil {
		return store.Build{}, false, err
	}
	if err := plan.Validate(); err != nil {
		return store.Build{}, false, &InvalidUploadError{err}
	}
	content := AppDir
	if plan.Kind == manifest.KindStatic {
		content = SiteDir
	}
	buildID := ids.New(ids.Build)
	ref := filepath.ToSlash(filepath.Join("builds", app, buildID))
	final := filepath.Join(dataDir, filepath.FromSlash(ref))
	partial := final + partialSuffix
	contentDir := filepath.Join(partial, content)
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return store.Build{}, false, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(partial)
		}
	}()

	files, size, err := fill(contentDir)
	if err != nil {
		return store.Build{}, false, &InvalidUploadError{err}
	}
	plan = *planned
	if err := plan.Validate(); err != nil {
		return store.Build{}, false, &InvalidUploadError{err}
	}
	if files == 0 {
		return store.Build{}, false, &InvalidUploadError{errors.New("the upload contains no files")}
	}
	if plan.Kind == manifest.KindStatic {
		idx, err := static.BuildIndex(contentDir)
		if err != nil {
			return store.Build{}, false, err
		}
		if err := idx.WriteIndex(filepath.Join(partial, "index.json")); err != nil {
			return store.Build{}, false, err
		}
	}
	if err := build.MakeReadOnly(contentDir); err != nil {
		return store.Build{}, false, err
	}
	if err := os.Rename(partial, final); err != nil {
		return store.Build{}, false, err
	}
	ok = true

	// The app is created only once the upload is known to be good.
	appID, created, err := db.EnsureApp(ctx, app)
	if err != nil {
		os.RemoveAll(final)
		return store.Build{}, false, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return store.Build{}, false, err
	}
	b := store.Build{
		ID: buildID, AppID: appID, SourceType: plan.Kind + "-upload", ArtifactKind: "dir", ArtifactRef: ref,
		Plan: planJSON, Files: files, SizeBytes: size, CreatedAt: time.Now(),
	}
	if err := db.CreateBuild(ctx, b); err != nil {
		return store.Build{}, false, err
	}
	return b, created, nil
}

// InvalidUploadError reports an upload that cannot be published.
type InvalidUploadError struct{ Err error }

func (e *InvalidUploadError) Error() string { return "invalid upload: " + e.Err.Error() }
func (e *InvalidUploadError) Unwrap() error { return e.Err }

// CleanPartial removes build directories left half-written by an
// interrupted deploy.
func CleanPartial(dataDir string) error {
	matches, err := filepath.Glob(filepath.Join(dataDir, "builds", "*", "*"+partialSuffix))
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SiteFile is one file of a folder being uploaded.
type SiteFile struct {
	Path    string
	Content io.Reader
}

// SiteUpload is a folder kept as a build.
type SiteUpload struct {
	Build store.Build
	// Created says that the app is new.
	Created bool
	Plan    manifest.Plan
	// Warnings say what the deployment's log should.
	Warnings []string
}

// leftOutOfSite says whether a file or directory of that name stays out of a folder dropped into the
// console. Whoever drops a folder sees a folder, and not the hidden files that are also in it.
func leftOutOfSite(name string) bool {
	switch name {
	case ".git", ".DS_Store", "Thumbs.db", "desktop.ini":
		return true
	}
	return name == ".env" || strings.HasPrefix(name, ".env.")
}

// UploadSite keeps the files next gives, until it says io.EOF, as a static site. A folder is what
// is meant by everything being in one folder, and a moorage.toml at its top says how to serve it.
func UploadSite(ctx context.Context, db *store.DB, dataDir, app string, next func() (SiteFile, error)) (SiteUpload, error) {
	up := SiteUpload{Plan: manifest.Plan{Kind: manifest.KindStatic, Static: &manifest.Static{}}}
	build, created, err := storeBuild(ctx, db, dataDir, app, &up.Plan, func(contentDir string) (int, int64, error) {
		raw := filepath.Join(filepath.Dir(contentDir), "upload")
		defer os.RemoveAll(raw)
		if err := os.MkdirAll(raw, 0o755); err != nil {
			return 0, 0, err
		}
		left, err := writeSiteFiles(raw, next)
		if err != nil {
			return 0, 0, err
		}
		root := soleFolder(raw)
		rules, err := siteRules(root)
		if err != nil {
			return 0, 0, err
		}
		if rules.Dir != "" {
			sub := filepath.FromSlash(rules.Dir)
			if !filepath.IsLocal(sub) {
				return 0, 0, errors.New("[static] dir in moorage.toml is not in this folder")
			}
			root = filepath.Join(root, sub)
			if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
				return 0, 0, errors.New("[static] dir in moorage.toml is not in this folder")
			}
			rules.Dir = ""
		}
		up.Plan.Static = &rules
		if err := os.Remove(contentDir); err != nil {
			return 0, 0, err
		}
		if err := os.Rename(root, contentDir); err != nil {
			return 0, 0, err
		}
		files, size, err := measureTree(contentDir)
		if err != nil {
			return 0, 0, err
		}
		if len(left) > 0 {
			slices.Sort(left)
			up.Warnings = append(up.Warnings, "left out, as not meant to be served: "+strings.Join(left, ", "))
		}
		if _, err := os.Stat(filepath.Join(contentDir, "index.html")); errors.Is(err, fs.ErrNotExist) && files > 0 && !rules.Listing {
			up.Warnings = append(up.Warnings, "there is no index.html at the top of this site, so that its address finds nothing")
		}
		return files, size, nil
	})
	if err != nil {
		return up, err
	}
	up.Build, up.Created = build, created
	return up, nil
}

// writeSiteFiles writes the files into dir, and says what it left out.
func writeSiteFiles(dir string, next func() (SiteFile, error)) (left []string, err error) {
	lim := archive.DefaultLimits
	seen := map[string]bool{}
	files, size := 0, int64(0)
	for {
		f, err := next()
		if errors.Is(err, io.EOF) {
			return left, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the upload did not finish: %w", err)
		}
		rel, err := archive.CleanName(f.Path)
		if err != nil {
			return nil, err
		}
		if out := leftOutPart(rel); out != "" {
			if !seen[out] {
				seen[out] = true
				left = append(left, out)
			}
			continue
		}
		if files++; files > lim.MaxFiles {
			return nil, fmt.Errorf("it has more than %d files", lim.MaxFiles)
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("%s cannot be kept: a file is where a folder should be", rel)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%s is there twice", rel)
		}
		if err != nil {
			return nil, fmt.Errorf("%s cannot be kept under that name", rel)
		}
		n, err := io.Copy(out, io.LimitReader(f.Content, lim.MaxBytes-size+1))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("the upload did not finish: %w", err)
		}
		if size += n; size > lim.MaxBytes {
			return nil, fmt.Errorf("it has more than %d bytes", lim.MaxBytes)
		}
	}
}

// leftOutPart is the part of a path that is left out of a site, if any is.
func leftOutPart(rel string) string {
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if leftOutOfSite(part) {
			return strings.Join(parts[:i+1], "/")
		}
	}
	return ""
}

// soleFolder is the folder that holds everything, or dir itself if nothing does.
func soleFolder(dir string) string {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dir, entries[0].Name())
	}
	return dir
}

// siteRules reads how to serve a folder from its moorage.toml, which is then not one of its files.
// A folder for an app that builds or runs refuses: those files are source, and not to be served.
func siteRules(root string) (manifest.Static, error) {
	m, err := manifest.Load(root)
	if err != nil {
		return manifest.Static{}, err
	}
	if m == nil {
		return manifest.Static{}, nil
	}
	if len(m.RawProcesses) > 0 || m.Build != nil || (m.App.Runtime != "" && m.App.Runtime != "static") {
		return manifest.Static{}, errors.New("this folder has a moorage.toml for an app to build or run, and would serve its source: upload it as source instead")
	}
	if err := os.Remove(filepath.Join(root, manifest.FileName)); err != nil {
		return manifest.Static{}, err
	}
	if m.Static == nil {
		return manifest.Static{}, nil
	}
	return *m.Static, nil
}

func measureTree(dir string) (files int, size int64, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		size += info.Size()
		return nil
	})
	return files, size, err
}
