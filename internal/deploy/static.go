// Package deploy turns uploaded content into builds and releases
// (docs/04-build-and-deploy.md).
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
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/archive"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/ingress/static"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// partialSuffix marks a build directory still being written.
const partialSuffix = ".partial"

// StaticResult describes a published static deploy.
type StaticResult struct {
	Release    store.Release
	AppCreated bool
}

// Static publishes an uploaded directory (a gzip-compressed tar stream) as
// a new build of app and makes it the current release of env. The app is
// created if it does not exist. Nothing is published unless the whole
// upload is valid.
func Static(ctx context.Context, db *store.DB, dataDir, app, env string, rules manifest.Static, upload io.Reader, by string) (StaticResult, error) {
	if err := rules.Validate(); err != nil {
		return StaticResult{}, err
	}
	if err := names.ValidateApp(app); err != nil {
		return StaticResult{}, err
	}
	buildID := ids.New(ids.Build)
	ref := filepath.ToSlash(filepath.Join("builds", app, buildID))
	final := filepath.Join(dataDir, filepath.FromSlash(ref))
	partial := final + partialSuffix
	siteDir := filepath.Join(partial, "site")
	if err := os.MkdirAll(siteDir, 0o755); err != nil {
		return StaticResult{}, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(partial)
		}
	}()

	files, size, err := archive.Extract(upload, siteDir, archive.DefaultLimits)
	if err != nil {
		return StaticResult{}, &InvalidUploadError{err}
	}
	if files == 0 {
		return StaticResult{}, &InvalidUploadError{errors.New("the upload contains no files")}
	}
	idx, err := static.BuildIndex(siteDir)
	if err != nil {
		return StaticResult{}, err
	}
	if err := idx.WriteIndex(filepath.Join(partial, "index.json")); err != nil {
		return StaticResult{}, err
	}
	if err := makeReadOnly(siteDir); err != nil {
		return StaticResult{}, err
	}
	if err := os.Rename(partial, final); err != nil {
		return StaticResult{}, err
	}
	ok = true

	// The app is created only once the upload is known to be good.
	appID, created, err := db.EnsureApp(ctx, app)
	if err != nil {
		os.RemoveAll(final)
		return StaticResult{}, err
	}
	plan, err := json.Marshal(manifest.Plan{Kind: "static", Static: &rules})
	if err != nil {
		return StaticResult{}, err
	}
	if err := db.CreateBuild(ctx, store.Build{
		ID: buildID, AppID: appID, SourceType: "static-upload", ArtifactKind: "dir", ArtifactRef: ref,
		Plan: plan, Files: files, SizeBytes: size, CreatedAt: time.Now(),
	}); err != nil {
		return StaticResult{}, err
	}
	rel, err := db.Publish(ctx, app, env, buildID, "deploy", by)
	if err != nil {
		return StaticResult{}, err
	}
	return StaticResult{Release: rel, AppCreated: created}, nil
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

// makeReadOnly removes write permission from the files under dir, so a
// published build cannot be changed by accident.
func makeReadOnly(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if err := os.Chmod(p, 0o444); err != nil {
			return fmt.Errorf("make %s read-only: %w", strings.TrimPrefix(p, dir), err)
		}
		return nil
	})
}
