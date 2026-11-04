package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/secrets"
	"github.com/robert-mcdermott/moorage/internal/testroles"
)

func finishedBackup(t *testing.T, c *client.Client, id string) apitypes.Backup {
	t.Helper()
	var found apitypes.Backup
	waitUntil(t, "the backup to finish", func() bool {
		list, err := c.Backups(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range list.Items {
			if b.ID == id && b.Status != "running" {
				found = b
				return true
			}
		}
		return false
	})
	return found
}

// TestBackupsThroughARestore backs up a running server, loses it, and has it all back: its apps and
// their data and secrets, and those who can sign in.
func TestBackupsThroughARestore(t *testing.T) {
	cfg := testConfig(t)
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "THE_SECRET", Value: "hunter2", Secret: true}}}); err != nil {
		t.Fatal(err)
	}
	deploySite(t, c, "docs")
	data := filepath.Join(cfg.DataDir, "data", "shop", "production", "notes.txt")
	if err := os.WriteFile(data, []byte("written by the app"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantSecret := func(c *client.Client) {
		t.Helper()
		// Its value is kept out of what is logged, so that what shows that it is there is its length.
		started, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: `echo "its length is ${#THE_SECRET}"`})
		if err != nil {
			t.Fatal(err)
		}
		if run, out, _ := followedRun(t, c, "shop", started.ID); run.Status != "succeeded" || !strings.Contains(out, "its length is 7") {
			t.Errorf("%+v %q", run, out)
		}
	}
	wantSecret(c)

	// To a directory, from the API, and it says how it went.
	where := filepath.Join(t.TempDir(), "backups")
	if _, err := c.ConfigureBackups(ctx, apitypes.BackupConfig{Destination: where, Keep: 3}); err != nil {
		t.Fatal(err)
	}
	b, err := c.StartBackup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := finishedBackup(t, c, b.ID)
	if done.Status != "done" || done.Size == 0 || done.Name == "" || done.Trigger != "manual" {
		t.Fatalf("%+v", done)
	}
	archive := filepath.Join(where, done.Name)

	// What goes wrong afterwards goes away with the restore.
	if err := os.WriteFile(data, []byte("changed since"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	testroles.StopInstances(cfg.DataDir)

	// On a machine with nothing but the master key, kept apart.
	master, err := os.ReadFile(filepath.Join(cfg.DataDir, "keys", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	fresh := testConfig(t)
	fresh.DataDir = cfg.DataDir
	for _, name := range []string{"moorage.db", "moorage.db-wal", "moorage.db-shm", "data", "builds", "repos", "certs", "keys", "run"} {
		if err := os.RemoveAll(filepath.Join(cfg.DataDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	r, digest, err := backup.Open(ctx, archive, backup.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := backup.Restore(ctx, r, digest, backup.RestoreOptions{DataDir: cfg.DataDir}); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Fatalf("with no key: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "keys", "master.key"), master, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := secrets.LoadMasterKey(filepath.Join(cfg.DataDir, "keys", "master.key"), os.Getenv, false)
	if err != nil {
		t.Fatal(err)
	}
	r2, digest, err := backup.Open(ctx, archive, backup.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := backup.Restore(ctx, r2, digest, backup.RestoreOptions{DataDir: cfg.DataDir, KeyFingerprint: backup.Fingerprint(key)}); err != nil {
		t.Fatal(err)
	}

	addrs, _ = start(t, fresh)
	c = apiClient(t, fresh, addrs)
	waitUntil(t, "the app to serve again", func() bool {
		res, _ := get(t, addrs.HTTP, "shop.localhost", "/health")
		return res.StatusCode == http.StatusOK
	})
	wantServedAs := func(host, want string) {
		t.Helper()
		if res, body := get(t, addrs.HTTP, host, "/"); res.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d %q", host, res.StatusCode, body)
		}
	}
	wantServedAs("docs.localhost", "docs")
	if got, err := os.ReadFile(data); err != nil || string(got) != "written by the app" {
		t.Errorf("%q %v", got, err)
	}
	wantSecret(c)
	// Those with accounts still have them, and the sessions they were signed in with.
	owner.addr = addrs.HTTP
	if res, page := owner.get("/settings/backups"); res.StatusCode != http.StatusOK || !strings.Contains(page, done.Name) {
		t.Errorf("signed in as before: %d\n%s", res.StatusCode, page)
	}
}

// TestBackupSchedule makes backups as the schedule says, keeps as many as it says, and tells of one
// that fails.
func TestBackupSchedule(t *testing.T) {
	cfg := testConfig(t)
	addrs := startWith(t, cfg, func(o *Options) { o.BackupTick = 100 * time.Millisecond })
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	where := filepath.Join(t.TempDir(), "backups")
	got, err := c.ConfigureBackups(ctx, apitypes.BackupConfig{Destination: where, Schedule: "@every 1s", Keep: 2})
	if err != nil || got.Schedule != "@every 1s" || got.NextAt == nil {
		t.Fatalf("%+v %v", got, err)
	}
	waitUntil(t, "several backups, of which two are kept", func() bool {
		list, err := c.Backups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		finished := 0
		for _, b := range list.Items {
			if b.Status == "done" && b.Trigger == "schedule" {
				finished++
			}
		}
		archives, _ := filepath.Glob(filepath.Join(where, "moorage-*.tar.gz"))
		return finished >= 4 && len(archives) == 2
	})

	// Somewhere that cannot be written to fails, and people are told.
	blocked := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConfigureBackups(ctx, apitypes.BackupConfig{Destination: filepath.Join(blocked, "below"), Schedule: "@every 1s", Keep: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "a backup to fail", func() bool {
		list, err := c.Backups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(list.Items) > 0 && list.Items[0].Status == "failed" && list.Items[0].Error != ""
	})
	waitUntil(t, "the administrator to be told", func() bool {
		_, page := owner.get("/notifications")
		return strings.Contains(page, "A backup failed")
	})

	// It comes right, and says so.
	if _, err := c.ConfigureBackups(ctx, apitypes.BackupConfig{Destination: where, Schedule: "@every 1s", Keep: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the word that it recovered", func() bool {
		_, page := owner.get("/notifications")
		return strings.Contains(page, "A backup worked after one that failed")
	})
}

// TestBackupSettings tries what can be said about backups, and who can say it.
func TestBackupSettings(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()

	got, err := c.Backups(ctx)
	if err != nil || !strings.HasPrefix(got.Destination, cfg.DataDir) || got.Keep != 7 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, in := range []apitypes.BackupConfig{
		{Destination: "not/a/full/path", Keep: 1},
		{Destination: "s3://bucket", AccessKey: "a", Keep: 1},
		{Destination: "/tmp/x", Schedule: "whenever", Keep: 1},
		{Destination: "/tmp/x", Keep: -1},
	} {
		if _, err := c.ConfigureBackups(ctx, in); status(err) != http.StatusUnprocessableEntity {
			t.Errorf("%+v: %v", in, err)
		}
	}

	// A secret key is sealed, and is not given back. Left out, it stays.
	got, err = c.ConfigureBackups(ctx, apitypes.BackupConfig{Destination: "s3://bucket/x", Endpoint: "http://127.0.0.1:1", AccessKey: "access", SecretKey: "a-secret-key", Keep: 1})
	if err != nil || !got.HasSecretKey || got.AccessKey != "access" {
		t.Fatalf("%+v %v", got, err)
	}
	var sealed []byte
	if err := testAuth(t, cfg).DB.Reader.QueryRowContext(ctx, `SELECT sealed_secret FROM backup_config`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "a-secret-key") {
		t.Error("the secret key is kept in the clear")
	}
	if got, err = c.ConfigureBackups(ctx, apitypes.BackupConfig{Destination: "s3://bucket/y", Endpoint: "http://127.0.0.1:1", AccessKey: "access", Keep: 1}); err != nil || !got.HasSecretKey {
		t.Errorf("%+v %v", got, err)
	}
	if _, page := owner.get("/settings/backups"); strings.Contains(page, "a-secret-key") || !strings.Contains(page, "s3://bucket/y") {
		t.Errorf("%s", page)
	}

	// The page sets it too, and starts one.
	where := filepath.Join(t.TempDir(), "backups")
	token := owner.token()
	if res, page := owner.post("/settings/backups/config", url.Values{"csrf": {token}, "kind": {"path"}, "path": {where}, "keep": {"5"}, "schedule": {""}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	if res, page := owner.post("/settings/backups/config", url.Values{"csrf": {token}, "kind": {"s3"}, "bucket": {"not a bucket"}, "keep": {"5"}}); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, "s3://bucket/prefix") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if res, page := owner.post("/settings/backups/now", url.Values{"csrf": {token}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d\n%s", res.StatusCode, page)
	}
	waitUntil(t, "it to finish", func() bool {
		archives, _ := filepath.Glob(filepath.Join(where, "moorage-*.tar.gz"))
		return len(archives) == 1
	})
	waitUntil(t, "the page to say so", func() bool {
		_, page := owner.get("/settings/backups")
		return strings.Contains(page, `badge ok">Done`) && strings.Contains(page, "moorage-")
	})

	// It is for administrators.
	sam := addTeammate(t, cfg, addrs, owner, "sam@example.com")
	if res, _ := sam.get("/settings/backups"); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}
	if res, _ := sam.post("/settings/backups/now", url.Values{"csrf": {sam.token()}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}
	samClient := tokenClient(t, c, tokenOf(t, sam, "admin"))
	if _, err := samClient.StartBackup(ctx); status(err) != http.StatusForbidden {
		t.Errorf("%v", err)
	}
	var apiErr *client.APIError
	if _, err := samClient.Backups(ctx); !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Errorf("%v", err)
	}
}
