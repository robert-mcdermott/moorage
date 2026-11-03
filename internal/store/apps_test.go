package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
)

func migrated(t *testing.T) *DB {
	t.Helper()
	db, dir := open(t)
	if _, err := db.Migrate(context.Background(), filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	return db
}

func addBuild(t *testing.T, db *DB, appID string) string {
	t.Helper()
	id := ids.New(ids.Build)
	if err := db.CreateBuild(context.Background(), Build{
		ID: id, AppID: appID, SourceType: "test", ArtifactKind: "dir", ArtifactRef: "builds/x/" + id,
		Plan: json.RawMessage(`{"kind":"static"}`), Files: 1, SizeBytes: 10, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReleasesAndRollback(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	appID, created, err := db.EnsureApp(ctx, "docs")
	if err != nil || !created {
		t.Fatalf("EnsureApp: %v %v", created, err)
	}
	if again, created, _ := db.EnsureApp(ctx, "docs"); again != appID || created {
		t.Error("EnsureApp created the app twice")
	}
	if _, _, err := db.EnsureApp(ctx, "Bad Name"); err == nil {
		t.Error("EnsureApp accepted an invalid name")
	}
	if _, err := db.Rollback(ctx, "docs", "production", 0, ""); err == nil || !strings.Contains(err.Error(), "nothing deployed") {
		t.Errorf("rollback with nothing deployed: %v", err)
	}

	b1, b2 := addBuild(t, db, appID), addBuild(t, db, appID)
	r1, err := db.Publish(ctx, "docs", "production", b1, "deploy", "admin")
	if err != nil || r1.Version != 1 {
		t.Fatalf("first release: %+v %v", r1, err)
	}
	if _, err := db.Rollback(ctx, "docs", "production", 0, ""); err == nil || !strings.Contains(err.Error(), "no release before v1") {
		t.Errorf("rollback from v1: %v", err)
	}
	if r2, err := db.Publish(ctx, "docs", "production", b2, "deploy", "admin"); err != nil || r2.Version != 2 {
		t.Fatalf("second release: %+v %v", r2, err)
	}
	if _, err := db.Rollback(ctx, "docs", "production", 2, ""); err == nil || !strings.Contains(err.Error(), "already the current") {
		t.Errorf("rollback to the current release: %v", err)
	}
	r3, err := db.Rollback(ctx, "docs", "production", 1, "admin")
	if err != nil || r3.Version != 3 || r3.BuildID != b1 || r3.Reason != "rollback" {
		t.Fatalf("rollback to v1: %+v %v", r3, err)
	}

	rels, err := db.Releases(ctx, "docs", "production")
	if err != nil || len(rels) != 3 || rels[0].Version != 3 || !rels[0].Current || rels[2].Current {
		t.Errorf("releases: %+v %v", rels, err)
	}
	routes, err := db.Routes(ctx)
	if err != nil || len(routes) != 1 || routes[0].Build.ID != b1 || routes[0].Version != 3 {
		t.Errorf("routes: %+v %v", routes, err)
	}
	apps, err := db.Apps(ctx)
	if err != nil || len(apps) != 1 || apps[0].Production == nil || apps[0].Production.Version != 3 {
		t.Errorf("apps: %+v %v", apps, err)
	}
	if _, err := db.Releases(ctx, "nope", "production"); err == nil {
		t.Error("releases of a missing app")
	}
}

func TestSettings(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	var v struct{ A int }
	if found, err := db.Setting(ctx, "x", &v); found || err != nil {
		t.Errorf("missing setting: %v %v", found, err)
	}
	if err := db.PutSetting(ctx, "x", struct{ A int }{1}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSetting(ctx, "x", struct{ A int }{2}, "test"); err != nil {
		t.Fatal(err)
	}
	if found, err := db.Setting(ctx, "x", &v); !found || err != nil || v.A != 2 {
		t.Errorf("setting: %v %v %+v", found, err, v)
	}
}
