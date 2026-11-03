// Package deploy turns uploads into builds and builds into releases: it
// stores uploaded sites and artifacts, builds uploaded source, and runs the
// deployments that publish them (docs/04-build-and-deploy.md).
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

	files, size, err := archive.Extract(upload, contentDir, archive.DefaultLimits)
	if err != nil {
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
