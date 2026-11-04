package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/serverconfig"
)

func itemOf(report apitypes.ConfigReport, what, name string) apitypes.ConfigItem {
	for _, item := range report.Items {
		if item.What == what && item.Name == name {
			return item
		}
	}
	return apitypes.ConfigItem{Action: "nothing said"}
}

func wantAction(t *testing.T, report apitypes.ConfigReport, what, name, action string) {
	t.Helper()
	if got := itemOf(report, what, name); got.Action != action {
		t.Errorf("%s %s: %s (%s), and not %s", what, name, got.Action, got.Note, action)
	}
}

func actions(report apitypes.ConfigReport) map[string]int {
	out := map[string]int{}
	for _, item := range report.Items {
		out[item.Action]++
	}
	return out
}

// settledServer is a server with something of everything set.
func settledServer(t *testing.T) (config.Config, *client.Client) {
	t.Helper()
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	signIn(t, cfg, addrs)
	ctx := context.Background()
	if _, err := c.CreateUser(ctx, apitypes.NewUser{Email: "ada@example.com", Name: "Ada", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateProject(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetMember(ctx, "web", "ada@example.com", "maintainer"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegistryLogin(ctx, "ghcr.io", apitypes.RegistryLogin{Username: "robot", Password: "the-registry-password"}); err != nil {
		t.Fatal(err)
	}
	created, err := c.AddNotificationChannel(ctx, apitypes.NewNotificationChannel{Name: "Ops alerts", Kind: "webhook", URL: "https://hooks.example.com/the-channel-address"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Secret == "" {
		t.Fatal("a webhook has a secret")
	}
	if _, err := c.AddNotificationRule(ctx, apitypes.NewNotificationRule{Channel: "Ops alerts", Events: []string{"deployment.failed"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConfigureBackups(ctx, apitypes.BackupConfig{Destination: "s3://acme/backups", Region: "us-east-1", AccessKey: "AKIAEXAMPLE", SecretKey: "the-backup-secret", Schedule: "@daily", Keep: 9}); err != nil {
		t.Fatal(err)
	}
	return cfg, c
}

// TestServerConfigGoesFromOneServerToAnother is what it is for.
func TestServerConfigGoesFromOneServerToAnother(t *testing.T) {
	ctx := context.Background()
	_, from := settledServer(t)
	f, err := from.ServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// It holds no secrets, in either form.
	body, err := serverconfig.Encode(f, " for a test")
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(f)
	for _, secret := range []string{"the-registry-password", "the-channel-address", "the-backup-secret", "passphrase", "argon2"} {
		if strings.Contains(string(body), secret) || strings.Contains(string(plain), secret) {
			t.Errorf("%q went out in the export:\n%s", secret, body)
		}
	}
	for _, name := range []string{"${MOORAGE_REGISTRY_GHCR_IO_PASSWORD}", "${MOORAGE_CHANNEL_OPS_ALERTS_URL}", "${MOORAGE_CHANNEL_OPS_ALERTS_SECRET}", "${MOORAGE_BACKUP_SECRET_KEY}"} {
		if !strings.Contains(string(body), name) {
			t.Errorf("%s is not named:\n%s", name, body)
		}
	}
	// What it writes, it reads.
	back, err := serverconfig.Parse(body)
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if len(back.Users) != len(f.Users) || back.Projects[len(back.Projects)-1].Name != f.Projects[len(f.Projects)-1].Name || *back.Backups.Keep != 9 {
		t.Errorf("%+v", back)
	}

	// A new server. With nothing set in the environment, what needs a secret is left out, and says what it needs.
	cfg2 := testConfig(t)
	addrs2, _ := start(t, cfg2)
	to := apiClient(t, cfg2, addrs2)
	signIn(t, cfg2, addrs2)

	dry, err := to.ImportServerConfig(ctx, back, true)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun {
		t.Error("it does not say that it was a dry run")
	}
	wantAction(t, dry, "user", "ada@example.com", apitypes.ConfigCreate)
	wantAction(t, dry, "project", "web", apitypes.ConfigCreate)
	wantAction(t, dry, "registry", "ghcr.io", apitypes.ConfigSkip)
	wantAction(t, dry, "channel", "Ops alerts", apitypes.ConfigSkip)
	wantAction(t, dry, "backups", "s3://acme/backups", apitypes.ConfigSkip)
	if note := itemOf(dry, "registry", "ghcr.io").Note; !strings.Contains(note, "MOORAGE_REGISTRY_GHCR_IO_PASSWORD") {
		t.Errorf("%q", note)
	}
	// A dry run changes nothing.
	if users, err := to.Users(ctx); err != nil || len(users) != 1 {
		t.Errorf("%v %v", users, err)
	}
	if projects, err := to.Projects(ctx); err != nil || len(projects) != 1 {
		t.Errorf("%v %v", projects, err)
	}

	// With the secrets, all of it comes.
	serverconfig.Expand(&back, func(name string) string {
		return map[string]string{
			"MOORAGE_REGISTRY_GHCR_IO_PASSWORD": "the-registry-password",
			"MOORAGE_CHANNEL_OPS_ALERTS_URL":    "https://hooks.example.com/the-channel-address",
			"MOORAGE_CHANNEL_OPS_ALERTS_SECRET": "the-signing-secret",
			"MOORAGE_BACKUP_SECRET_KEY":         "the-backup-secret",
		}[name]
	})
	report, err := to.ImportServerConfig(ctx, back, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := actions(report); n[apitypes.ConfigFailed] != 0 || n[apitypes.ConfigSkip] != 0 {
		t.Errorf("%+v", report)
	}
	wantAction(t, report, "user", "owner@example.com", apitypes.ConfigSame)
	if note := itemOf(report, "user", "ada@example.com").Note; !strings.Contains(note, "users invite ada@example.com") {
		t.Errorf("%q", note)
	}
	project, err := to.Project(ctx, "web")
	if err != nil || len(project.Members) != 1 || project.Members[0].Email != "ada@example.com" || project.Members[0].Role != "maintainer" {
		t.Errorf("%+v %v", project, err)
	}

	// The second time, there is nothing to do.
	again, err := to.ImportServerConfig(ctx, back, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range again.Items {
		if item.Action != apitypes.ConfigSame {
			t.Errorf("%+v", item)
		}
	}
	// And what the new server writes out is what the old one did.
	exported, err := to.ServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(f)
	second, _ := json.Marshal(exported)
	if string(first) != string(second) {
		t.Errorf("%s\n%s", first, second)
	}
}

// TestServerConfigImportChangesAndKeeps: it makes the changes it is asked to, keeps the secrets it is not told,
// and removes nothing.
func TestServerConfigImportChangesAndKeeps(t *testing.T) {
	ctx := context.Background()
	cfg, c := settledServer(t)
	f, err := c.ServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Those that hold secrets are all as they are, with the variables not set.
	report, err := c.ImportServerConfig(ctx, f, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range report.Items {
		if item.Action != apitypes.ConfigSame {
			t.Errorf("a server does not match its own export: %+v", item)
		}
	}

	// Change something of each, and leave someone and a project out.
	for i := range f.Users {
		if f.Users[i].Email == "ada@example.com" {
			f.Users[i].Role = "admin"
		}
	}
	f.Registries[0].Username = "another-robot"
	f.Channels[0].URL = "https://hooks.example.com/a-new-address"
	three := 3
	f.Backups.Keep = &three
	f.Projects = nil
	report, err = c.ImportServerConfig(ctx, f, false)
	if err != nil {
		t.Fatal(err)
	}
	wantAction(t, report, "user", "ada@example.com", apitypes.ConfigChange)
	wantAction(t, report, "registry", "ghcr.io", apitypes.ConfigChange)
	wantAction(t, report, "channel", "Ops alerts", apitypes.ConfigChange)
	wantAction(t, report, "backups", "s3://acme/backups", apitypes.ConfigChange)
	if project, err := c.Project(ctx, "web"); err != nil || len(project.Members) != 1 {
		t.Errorf("a project left out of the file went: %+v %v", project, err)
	}

	// The secrets stay: the database has them under the same names, opened with the same key.
	db := testAuth(t, cfg).DB
	if regs, err := db.Registries(ctx); err != nil || len(regs) != 1 || regs[0].Username != "another-robot" || len(regs[0].Sealed) == 0 {
		t.Errorf("%+v %v", regs, err)
	}
	if backups, err := db.BackupConfig(ctx); err != nil || backups.Keep != 3 || len(backups.SealedSecret) == 0 {
		t.Errorf("%+v %v", backups, err)
	}
	if channels, err := db.NotificationChannels(ctx); err != nil || len(channels) != 1 {
		t.Errorf("%+v %v", channels, err)
	}
	if rules, err := db.NotificationRules(ctx); err != nil || len(rules) != 1 {
		t.Errorf("the channel lost its rule: %+v %v", rules, err)
	}

	// The last admin cannot be taken away by a file, which says so and does the rest.
	f, err = c.ServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Users {
		f.Users[i].Role = "member"
	}
	report, err = c.ImportServerConfig(ctx, f, false)
	if err != nil {
		t.Fatal(err)
	}
	failedOnes := 0
	for _, item := range report.Items {
		if item.What == "user" && item.Action == apitypes.ConfigFailed {
			failedOnes++
		}
	}
	if failedOnes != 1 {
		t.Errorf("%+v", report)
	}
}

func TestServerConfigRefusals(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)

	if _, err := serverconfig.Parse([]byte("format = 1\n[[usr]]\nemail = \"a@b.c\"\n")); err == nil || !strings.Contains(err.Error(), "usr") {
		t.Errorf("a mistyped key: %v", err)
	}
	if _, err := serverconfig.Parse([]byte("this is = = not toml")); err == nil {
		t.Error("that is no file")
	}
	_, err := c.ImportServerConfig(ctx, apitypes.ServerConfig{Format: 99}, false)
	if status(err) != http.StatusUnprocessableEntity {
		t.Errorf("%v", err)
	}
	report, err := c.ImportServerConfig(ctx, apitypes.ServerConfig{Format: 1, Users: []apitypes.ConfigUser{
		{Email: "not an address", Role: "member"},
		{Email: "b@example.com", Role: "emperor"},
	}, Projects: []apitypes.ConfigProject{
		{Name: "NOT A NAME"},
		{Name: "web", Members: map[string]string{"nobody@example.com": "viewer"}},
	}, Channels: []apitypes.ConfigChannel{{Name: "x", Kind: "pigeon", URL: "https://example.com"}}, Rules: []apitypes.ConfigRule{{Channel: "x", Events: []string{"*"}}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	wantAction(t, report, "user", "not an address", apitypes.ConfigFailed)
	wantAction(t, report, "user", "b@example.com", apitypes.ConfigFailed)
	wantAction(t, report, "project", "NOT A NAME", apitypes.ConfigFailed)
	wantAction(t, report, "project", "web", apitypes.ConfigFailed)
	wantAction(t, report, "channel", "x", apitypes.ConfigFailed)
	wantAction(t, report, "rule", "x, for the server", apitypes.ConfigSkip)

	// Someone who is not an admin gets neither.
	sam := addTeammate(t, cfg, addrs, owner, "c@example.com")
	teammate := tokenClient(t, c, tokenOf(t, sam, "admin"))
	if _, err := teammate.ServerConfig(ctx); status(err) != http.StatusForbidden {
		t.Errorf("%v", err)
	}
	if _, err := teammate.ImportServerConfig(ctx, apitypes.ServerConfig{Format: 1}, true); status(err) != http.StatusForbidden {
		t.Errorf("%v", err)
	}
}

// TestServerConfigOfManyUsers: an address that sorts after the rest has to end the list like any other.
func TestServerConfigOfManyUsers(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	signIn(t, cfg, addrs)
	for _, email := range []string{"zoe@example.com", "yves@example.com", "adam@example.com"} {
		if _, err := c.CreateUser(ctx, apitypes.NewUser{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	short, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	f, err := c.ServerConfig(short)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Users) != 4 {
		t.Errorf("%+v", f.Users)
	}
}

// TestServerConfigWrittenByHand: what a file does not say stays as it is, setting by setting.
func TestServerConfigWrittenByHand(t *testing.T) {
	ctx := context.Background()
	_, c := settledServer(t)
	f, err := serverconfig.Parse([]byte(`format = 1

[[user]]
email = "sam@example.com"

[[user]]
email = "ada@example.com"

[[project]]
name = "default"
[project.members]
"sam@example.com" = "viewer"

[backups]
schedule = "@hourly"
`))
	if err != nil {
		t.Fatal(err)
	}
	report, err := c.ImportServerConfig(ctx, f, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := actions(report); n[apitypes.ConfigFailed] != 0 {
		t.Errorf("%+v", report)
	}
	project, err := c.Project(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if project.Everyone != "maintainer" || !project.ViewersSeeLogs {
		t.Errorf("a file that did not mention them changed them: %+v", project)
	}
	wantAction(t, report, "user", "ada@example.com", apitypes.ConfigSame)
	wantAction(t, report, "user", "sam@example.com", apitypes.ConfigCreate)
	backups, err := c.Backups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if backups.Keep != 9 || backups.Destination != "s3://acme/backups" || backups.Schedule != "@hourly" {
		t.Errorf("%+v", backups)
	}
	// Saying so does change them.
	none := ""
	see := false
	report, err = c.ImportServerConfig(ctx, apitypes.ServerConfig{Format: 1, Projects: []apitypes.ConfigProject{{Name: "default", EveryoneRole: &none, ViewersSeeLogs: &see}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	wantAction(t, report, "project", "default", apitypes.ConfigChange)
	if project, err = c.Project(ctx, "default"); err != nil || project.Everyone != "" || project.ViewersSeeLogs {
		t.Errorf("%+v %v", project, err)
	}
}

// TestServerConfigDryRunSaysTheSame: what would fail says so before it is done.
func TestServerConfigDryRunSaysTheSame(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	signIn(t, cfg, addrs)
	secret := "https://hooks.example.com/address"
	bad := "not a schedule"
	f := apitypes.ServerConfig{Format: 1,
		Projects: []apitypes.ConfigProject{{Name: "NOT A NAME"}},
		Channels: []apitypes.ConfigChannel{{Name: "ops", Kind: "slack", URL: secret}},
		Rules: []apitypes.ConfigRule{
			{Channel: "ops", Events: []string{"something else"}},
			{Channel: "ops", App: "not-there", Events: []string{"*"}},
			{Channel: "ops", Project: "not-there", Events: []string{"*"}},
			{Channel: "ops", Events: []string{"*"}},
		},
		Backups: &apitypes.ConfigBackups{Schedule: &bad},
	}
	dry, err := c.ImportServerConfig(ctx, f, true)
	if err != nil {
		t.Fatal(err)
	}
	real, err := c.ImportServerConfig(ctx, f, false)
	if err != nil {
		t.Fatal(err)
	}
	if n, m := actions(dry), actions(real); n[apitypes.ConfigFailed] != m[apitypes.ConfigFailed] || n[apitypes.ConfigFailed] != 5 || n[apitypes.ConfigCreate] != m[apitypes.ConfigCreate] {
		t.Errorf("dry %+v, then %+v", dry, real)
	}
}

// TestServerConfigOnlyAsksForItsOwnVariables: a file from someone else cannot ask for any other secret in the
// environment of the command, to have it go wherever the file says.
func TestServerConfigOnlyAsksForItsOwnVariables(t *testing.T) {
	f := apitypes.ServerConfig{Format: 1, Registries: []apitypes.ConfigRegistry{
		{Host: "evil.example.com", Username: "x", Password: "${AWS_SECRET_ACCESS_KEY}"},
		{Host: "ghcr.io", Username: "x", Password: "${MOORAGE_REGISTRY_GHCR_IO_PASSWORD}"},
	}}
	serverconfig.Expand(&f, func(name string) string { return "the value of " + name })
	if f.Registries[0].Password != "${AWS_SECRET_ACCESS_KEY}" {
		t.Errorf("%q", f.Registries[0].Password)
	}
	if f.Registries[1].Password != "the value of MOORAGE_REGISTRY_GHCR_IO_PASSWORD" {
		t.Errorf("%q", f.Registries[1].Password)
	}
}
