package store

import (
	"context"
	"testing"
)

func TestVarsAndReleaseConfig(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	appID, _, _ := db.EnsureApp(ctx, "shop")
	if _, err := db.ChangeVars(ctx, "nope", "production", VarChange{}); err == nil {
		t.Error("changed the variables of an unknown app")
	}
	_, err := db.ChangeVars(ctx, "shop", "production", VarChange{AllEnvs: true, By: "admin", Set: []Var{
		{Name: "LOG_LEVEL", Value: "info"},
		{Name: "REGION", Value: "eu"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ChangeVars(ctx, "shop", "production", VarChange{By: "admin", Set: []Var{
		{Name: "LOG_LEVEL", Value: "debug", Build: true},
		{Name: "API_KEY", Secret: true, Sealed: []byte{1, 2, 3}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	vars, err := db.Vars(ctx, "shop", "production")
	if err != nil || len(vars) != 4 {
		t.Fatalf("vars: %+v %v", vars, err)
	}
	if vars[0].Name != "API_KEY" || !vars[0].Secret || vars[0].Value != "" || vars[1].Name != "LOG_LEVEL" || vars[1].AllEnvs || !vars[2].AllEnvs {
		t.Errorf("vars: %+v", vars)
	}

	b := addBuild(t, db, appID)
	rel, err := db.CreateRelease(ctx, "shop", "production", b, "deploy", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if rel.AppID != appID || len(rel.Config) != 3 {
		t.Fatalf("release config: %+v", rel.Config)
	}
	got := map[string]ConfigVar{}
	for _, v := range rel.Config {
		got[v.Name] = v
	}
	if got["LOG_LEVEL"].Value != "debug" || !got["LOG_LEVEL"].Build || got["LOG_LEVEL"].Scope == "" {
		t.Errorf("the environment's own value should win: %+v", got["LOG_LEVEL"])
	}
	if got["REGION"].Scope != "" || got["API_KEY"].Value != "" || string(got["API_KEY"].Sealed) != "\x01\x02\x03" {
		t.Errorf("config: %+v", got)
	}
	db.MakeCurrent(ctx, rel.ID)

	// Unsetting reports what was not set; the release keeps its snapshot.
	missing, err := db.ChangeVars(ctx, "shop", "production", VarChange{Unset: []string{"LOG_LEVEL", "NOPE"}})
	if err != nil || len(missing) != 1 || missing[0] != "NOPE" {
		t.Errorf("unset: %v %v", missing, err)
	}
	cfg, _ := db.Config(ctx, "shop", "production")
	for _, v := range cfg {
		if v.Name == "LOG_LEVEL" && v.Value != "info" {
			t.Errorf("after unsetting the environment's value, the app's shows: %+v", v)
		}
	}
	current, _, err := db.CurrentRelease(ctx, "shop", "production")
	if err != nil || len(current.Config) != 3 || current.AppID != appID {
		t.Errorf("current release: %+v %v", current, err)
	}
	old, err := db.ReleaseConfig(ctx, "shop", "production", 1)
	if err != nil || len(old) != 3 {
		t.Errorf("release config: %+v %v", old, err)
	}
	r2, err := db.CreateReleaseWith(ctx, "shop", "production", b, "rollback", "admin", old)
	if err != nil || len(r2.Config) != 3 {
		t.Errorf("release with an earlier configuration: %+v %v", r2, err)
	}

	if has, _ := db.HasDataKeys(ctx); has {
		t.Error("data keys before any was added")
	}
	k1, _ := db.AddDataKey(ctx, appID, []byte("first"))
	k2, _ := db.AddDataKey(ctx, appID, []byte("second"))
	if string(k1) != "first" || string(k2) != "first" {
		t.Errorf("data keys: %q %q", k1, k2)
	}
}
