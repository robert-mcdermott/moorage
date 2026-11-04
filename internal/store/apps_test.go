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

func TestReleaseLifecycleAndInstances(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	appID, _, _ := db.EnsureApp(ctx, "shop")
	b1, b2 := addBuild(t, db, appID), addBuild(t, db, appID)
	r1, err := db.Publish(ctx, "shop", "production", b1, "deploy", "")
	if err != nil {
		t.Fatal(err)
	}
	// A new release is not current until made so.
	r2, err := db.CreateRelease(ctx, "shop", "production", b2, "deploy", "")
	if err != nil || r2.Version != 2 || r2.Current {
		t.Fatalf("CreateRelease: %+v %v", r2, err)
	}
	if cur, _, err := db.CurrentRelease(ctx, "shop", "production"); err != nil || cur.ID != r1.ID {
		t.Errorf("current before MakeCurrent: %+v %v", cur, err)
	}
	envID, err := db.EnvID(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	in := Instance{ID: ids.New(ids.Instance), EnvID: envID, ReleaseID: r2.ID, Process: "web", Index: 1, Driver: "native", Port: 24001, CreatedAt: time.Now()}
	if err := db.CreateInstance(ctx, in); err != nil {
		t.Fatal(err)
	}
	dup := in
	dup.ID = ids.New(ids.Instance)
	if err := db.CreateInstance(ctx, dup); err == nil {
		t.Error("two live instances got the same port")
	}
	if ports, _ := db.LivePorts(ctx); !ports[24001] {
		t.Errorf("live ports = %v", ports)
	}
	live, err := db.LiveInstances(ctx)
	if err != nil || len(live) != 1 || live[0].App != "shop" || live[0].Version != 2 || live[0].Build.ID != b2 {
		t.Errorf("live instances = %+v %v", live, err)
	}
	// A failed rollout: the release and its instances go away.
	if err := db.DeleteRelease(ctx, r2.ID); err != nil {
		t.Fatal(err)
	}
	if live, _ := db.LiveInstances(ctx); len(live) != 0 {
		t.Errorf("instances of a deleted release remain: %+v", live)
	}
	if err := db.DeleteRelease(ctx, r1.ID); err != nil {
		t.Fatal(err)
	}
	if cur, _, err := db.CurrentRelease(ctx, "shop", "production"); err != nil || cur.ID != r1.ID {
		t.Errorf("the current release was deleted: %+v %v", cur, err)
	}

	r3, _ := db.CreateRelease(ctx, "shop", "production", b2, "deploy", "")
	if err := db.MakeCurrent(ctx, r3.ID); err != nil {
		t.Fatal(err)
	}
	if cur, b, _ := db.CurrentRelease(ctx, "shop", "production"); cur.ID != r3.ID || b.ID != b2 {
		t.Errorf("current after MakeCurrent: %+v", cur)
	}
	in2 := Instance{ID: ids.New(ids.Instance), EnvID: envID, ReleaseID: r3.ID, Process: "web", Index: 1, Driver: "native", Port: 24001, CreatedAt: time.Now()}
	if err := db.CreateInstance(ctx, in2); err != nil {
		t.Fatal(err)
	}
	if err := db.StopInstance(ctx, in2.ID); err != nil {
		t.Fatal(err)
	}
	if ports, _ := db.LivePorts(ctx); ports[24001] {
		t.Error("a stopped instance still reserves its port")
	}
}

// TestPruneRuns keeps a job's runs, so that those it skipped do not push out what actually ran.
func TestPruneRuns(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	appID, _, err := db.EnsureApp(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	buildID := addBuild(t, db, appID)
	if _, err := db.CreateReleaseWith(ctx, "shop", "production", buildID, "deploy", "", nil); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).UTC()
	add := func(status string) string {
		t.Helper()
		at = at.Add(time.Second)
		id := ids.New(ids.Run)
		run := Run{ID: id, App: "shop", Env: "production", Job: "nightly", Command: "x", Trigger: "schedule", Status: RunRunning, StartedAt: at}
		if err := db.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if status != RunRunning {
			if err := db.FinishRun(ctx, id, status, nil, "", at); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	oldest := add(RunSucceeded)
	kept := add(RunFailed)
	last := add(RunSucceeded)
	var skipped []string
	for range 5 {
		skipped = append(skipped, add(RunSkipped))
	}
	going := add(RunRunning)
	gone, err := db.PruneRuns(ctx, "shop", "production", "nightly", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{oldest: true, skipped[0]: true, skipped[1]: true, skipped[2]: true}
	if len(gone) != len(want) {
		t.Fatalf("let go of %d: %v", len(gone), gone)
	}
	for _, id := range gone {
		if !want[id] {
			t.Errorf("let go of %s", id)
		}
	}
	for _, id := range []string{kept, last, skipped[3], skipped[4], going} {
		if _, err := db.Run(ctx, "shop", id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}
