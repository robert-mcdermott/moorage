package store

import (
	"context"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
)

func TestDeployments(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	appID, _, err := db.EnsureApp(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	b := addBuild(t, db, appID)
	dep := Deployment{ID: ids.New(ids.Deployment), App: "shop", Env: "production", SourceType: "source-upload", Status: Queued, BuildID: b, CreatedBy: "admin", CreatedAt: time.Now()}
	if err := db.CreateDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateDeployment(ctx, Deployment{ID: ids.New(ids.Deployment), App: "nope", Env: "production", Status: Queued, CreatedAt: time.Now()}); err == nil {
		t.Error("a deployment of an unknown app was recorded")
	}
	db.SetDeploymentStatus(ctx, dep.ID, Building)
	rel, err := db.CreateRelease(ctx, "shop", "production", b, "deploy", "admin")
	if err != nil {
		t.Fatal(err)
	}
	db.SetDeploymentRelease(ctx, dep.ID, rel.ID)
	if err := db.FinishDeployment(ctx, dep.ID, Building, "", nil); err == nil {
		t.Error("finished with a status that is not final")
	}
	if err := db.FinishDeployment(ctx, dep.ID, Failed, "boom", []string{"line 1", "line 2"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Deployment(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Failed || got.Error != "boom" || len(got.Output) != 2 || got.Version != 1 || got.StartedAt.IsZero() || got.FinishedAt.IsZero() {
		t.Errorf("deployment = %+v", got)
	}
	// Deleting the failed release keeps the deployment's record.
	db.DeleteRelease(ctx, rel.ID)
	if got, _ := db.Deployment(ctx, dep.ID); got.ReleaseID != "" || got.Version != 0 {
		t.Errorf("after the release was deleted: %+v", got)
	}

	running := Deployment{ID: ids.New(ids.Deployment), App: "shop", Env: "production", SourceType: "source-upload", Status: Building, CreatedAt: time.Now()}
	db.CreateDeployment(ctx, running)
	if n, err := db.InterruptDeployments(ctx); err != nil || n != 1 {
		t.Errorf("interrupted %d: %v", n, err)
	}
	list, err := db.Deployments(ctx, "shop", "production", 10)
	if err != nil || len(list) != 2 || list[0].ID != running.ID || list[0].Status != Failed {
		t.Errorf("deployments = %+v %v", list, err)
	}
	if _, err := db.Deployments(ctx, "nope", "production", 10); err == nil {
		t.Error("listed the deployments of an unknown app")
	}
}

func TestCollectableBuilds(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	appID, _, _ := db.EnsureApp(ctx, "shop")
	var builds []string
	for i := 0; i < 4; i++ {
		b := addBuild(t, db, appID)
		builds = append(builds, b)
		if _, err := db.Publish(ctx, "shop", "production", b, "deploy", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	// Roll back to the first build: it is current again, so it stays.
	if _, err := db.Publish(ctx, "shop", "production", builds[0], "rollback", "admin"); err != nil {
		t.Fatal(err)
	}
	spare := addBuild(t, db, appID) // a build no release uses
	active := addBuild(t, db, appID)
	db.CreateDeployment(ctx, Deployment{ID: ids.New(ids.Deployment), App: "shop", Env: "production", SourceType: "source-upload", Status: Building, BuildID: active, CreatedAt: time.Now()})

	got, err := db.CollectableBuilds(ctx, "shop", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, b := range got {
		ids[b.ID] = true
	}
	// Kept: builds[0] (current), builds[3] (v4, within the last 2), the
	// running deployment's build. Collected: builds[1], builds[2], spare.
	if len(got) != 3 || !ids[builds[1]] || !ids[builds[2]] || !ids[spare] {
		t.Errorf("collectable = %v", ids)
	}
	if got, _ := db.CollectableBuilds(ctx, "shop", 2, time.Hour); len(got) != 0 {
		t.Errorf("new builds were collectable: %d", len(got))
	}
	db.MarkCollected(ctx, builds[1])
	if b, _ := db.Build(ctx, builds[1]); !b.Collected {
		t.Error("the build is not marked collected")
	}
	if _, _, err := db.RollbackBuild(ctx, "shop", "production", 2); err == nil {
		t.Error("rolled back to a collected build")
	}
	if got, _ := db.CollectableBuilds(ctx, "shop", 2, 0); len(got) != 2 {
		t.Errorf("a collected build is collectable again: %d", len(got))
	}
}
