package manifest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseStatic(t *testing.T) {
	data := []byte(`
[app]
name = "docs"
runtime = "static"

[static]
dir = "public"
spa = true
not_found = "404.html"

[[static.headers]]
for = "/assets/*"
values = { "Cache-Control" = "public, max-age=31536000, immutable" }

[[static.redirects]]
from = "/blog/:year/*"
to = "/posts/:year/:splat"

[processes.web]
command = "ignored until processes arrive"
`)
	m, err := Parse(data, "moorage.toml")
	if err != nil {
		t.Fatal(err)
	}
	if m.App.Name != "docs" || m.Static == nil || m.Static.Dir != "public" || !m.Static.SPA {
		t.Fatalf("parsed %+v / %+v", m.App, m.Static)
	}
	m.Static.Defaults()
	if !*m.Static.CleanURLs || m.Static.TrailingSlash != "auto" || m.Static.Redirects[0].Status != 301 {
		t.Errorf("defaults: %+v", m.Static)
	}
	if m.Static.Headers[0].Values["Cache-Control"] == "" {
		t.Errorf("header rule lost: %+v", m.Static.Headers)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	for _, data := range []string{
		"[static]\nspaa = true\n",
		"[app]\nnmae = \"docs\"\n",
		"[apps]\nname = \"docs\"\n",
	} {
		_, err := Parse([]byte(data), "moorage.toml")
		if err == nil || !strings.Contains(err.Error(), "moorage.toml:") || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("Parse(%q): %v", data, err)
		}
	}
}

func TestStaticValidate(t *testing.T) {
	s := Static{
		Dir:           "../outside",
		TrailingSlash: "sometimes",
		NotFound:      "/404.html",
		Headers:       []HeaderRule{{For: "assets/*", Values: map[string]string{"X Bad": "1"}}},
		Redirects: []Redirect{
			{From: "old", To: "/new"},
			{From: "/a/*/b", To: "/b"},
			{From: "/x", To: "/y", Status: 404},
			{From: "/r", To: "https://example.com/", Status: 200},
			{From: "/n"},
		},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("invalid rules accepted")
	}
	for _, want := range []string{"static.dir", "trailing_slash", "not_found", "headers[0].for", "invalid header name",
		"redirects[0].from", "* only at the end", "redirects[2].status", "a rewrite", "redirects[4].to"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("problems do not mention %q:\n%v", want, err)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if m, err := Load(dir); m != nil || err != nil {
		t.Errorf("Load without a manifest = %v, %v", m, err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("Load of version 2: %v", err)
	}
}

func TestParseProcesses(t *testing.T) {
	data := []byte(`
[processes]
worker = "python -m app.worker"

[processes.web]
command = "./server --port $PORT"
instances = 2
health = { path = "/healthz", interval = "5s", startup = "2m" }
stop_timeout = "45s"

[deploy]
strategy = "recreate"
drain = "1d"

[env]
LOG_LEVEL = "info"
`)
	m, err := Parse(data, "moorage.toml")
	if err != nil {
		t.Fatal(err)
	}
	web, worker := m.Processes["web"], m.Processes["worker"]
	if web.Command != "./server --port $PORT" || *web.Instances != 2 || web.Health.Path != "/healthz" ||
		web.Health.Interval.Std() != 5*time.Second || web.Health.Startup.Std() != 2*time.Minute || web.StopTimeout.Std() != 45*time.Second {
		t.Errorf("web = %+v", web)
	}
	if worker.Command != "python -m app.worker" {
		t.Errorf("worker = %+v", worker)
	}
	if m.Deploy.Strategy != "recreate" || m.Deploy.Drain.Std() != 24*time.Hour || m.Env["LOG_LEVEL"] != "info" {
		t.Errorf("deploy %+v, env %v", m.Deploy, m.Env)
	}
	worker.Defaults("worker")
	web.Defaults("web")
	if *worker.Expose || !*web.Expose || worker.Restart != "always" || worker.Health.Threshold != 3 || *worker.Instances != 1 {
		t.Errorf("defaults: web %+v, worker %+v", web, worker)
	}
}

func TestParseProcessErrors(t *testing.T) {
	for data, want := range map[string]string{
		"[processes.web]\ncomand = \"x\"\n":                           "unknown key comand",
		"[processes.worker]\ninstances = 3\n":                         "processes.worker.command is required",
		"[processes]\nWeb = \"x\"\n":                                  "process name",
		"[processes.web]\ncommand = \"x\"\nrestart = \"sometimes\"\n": "restart",
		"[processes.web]\ncommand = \"x\"\nstop_timeout = \"soon\"\n": "duration",
		"[processes]\nweb = 3\n":                                      "command string or a table",
		"[deploy]\nstrategy = \"blue-green\"\n":                       "deploy.strategy",
		"[env]\n\"BAD-NAME\" = \"x\"\n":                               "not a valid variable name",
	} {
		_, err := Parse([]byte(data), "moorage.toml")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q): %v, want an error mentioning %q", data, err, want)
		}
	}
}

func TestParseFullManifest(t *testing.T) {
	data := []byte(`
version = 1

[app]
name = "acme-portal"
runtime = "python"
toolchains = ["node@24"]

[python]
version = "3.13"
groups = ["web"]

[build]
post = "npm --prefix frontend ci"
timeout = "15m"
exclude = ["tests/**"]
env = { VITE_MODE = "production" }

[processes.web]
command = "uvicorn app.main:app --port $PORT"
health = { path = "/api/health" }

[processes]
worker = "python -m app.worker"

[release]
command = "alembic upgrade head"
on_create = "python -m app.seed"

[[cron]]
name = "cleanup"
schedule = "0 3 * * *"
command = "python -m app.cleanup"

[[routes]]
path = "/api/"
process = "web"

[vars.SECRET_KEY]
generate = "secret"

[resources]
memory = "512MiB"
cpu = 0.5

[previews]
ttl = "5d"
access = "login"

[environments.staging]
branch = "develop"
processes.web.instances = 2
sleep_after = "off"

[services.db]
type = "postgres"
version = "17"

[x-editor]
anything = { goes = true }
`)
	m, err := Parse(data, "moorage.toml")
	if err != nil {
		t.Fatal(err)
	}
	if m.Python.Version != "3.13" || m.Build.Timeout.Std() != 15*time.Minute || m.Release.OnCreate == "" {
		t.Errorf("tables: %+v %+v %+v", m.Python, m.Build, m.Release)
	}
	if m.Resources.Memory != 512<<20 || len(m.Cron) != 1 || len(m.Routes) != 1 || m.Vars["SECRET_KEY"].Generate != "secret" {
		t.Errorf("tables: %+v %+v %+v %+v", m.Resources, m.Cron, m.Routes, m.Vars)
	}
	if *m.Environments["staging"].Processes["web"].Instances != 2 || m.Services["db"].Type != "postgres" {
		t.Errorf("environments and services: %+v %+v", m.Environments, m.Services)
	}
	if len(m.Processes) != 2 {
		t.Errorf("processes: %+v", m.Processes)
	}
	warnings := strings.Join(Lint(m), "\n")
	for _, want := range []string{"[[routes]]"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("no warning about %s:\n%s", want, warnings)
		}
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	data := []byte(`
[app]
runtime = "cobol"
toolchains = ["node"]

[python]
manager = "pip"

[[cron]]
name = "x"
schedule = "every day"
command = "true"

[[routes]]
path = "api"

[env]
"BAD-NAME" = "1"
`)
	_, err := Parse(data, "moorage.toml")
	if err == nil {
		t.Fatal("parsed")
	}
	for _, want := range []string{"app.runtime", "app.toolchains", "python.manager", "schedule", "must start with /", "give either", "BAD-NAME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no problem about %s in:\n%v", want, err)
		}
	}
}

func TestLintSecrets(t *testing.T) {
	m, err := Parse([]byte(`
[env]
LOG_LEVEL = "info"
API_TOKEN = "abc"
OPAQUE = "q8Zr2LxP0vN4mKs7TbW1yHd9"
FROM_SERVER = "$DATABASE_URL"
`), "moorage.toml")
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(Lint(m), "\n")
	if !strings.Contains(w, "env.API_TOKEN") || !strings.Contains(w, "env.OPAQUE") {
		t.Errorf("warnings:\n%s", w)
	}
	if strings.Contains(w, "LOG_LEVEL") || strings.Contains(w, "FROM_SERVER") {
		t.Errorf("false positives:\n%s", w)
	}
}

// TestStaticPlan keeps what a manifest says of who may reach a site, and how many releases to keep.
func TestStaticPlan(t *testing.T) {
	var none *Manifest
	if plan := none.StaticPlan(); plan.Kind != KindStatic || plan.Static == nil {
		t.Errorf("%+v", plan)
	}
	keep := 9
	m := &Manifest{
		Static:       &Static{Dir: "public", SPA: true},
		Deploy:       Deploy{KeepReleases: keep},
		Environments: map[string]Environment{"production": {Access: "login"}},
		Previews:     &Previews{Access: "share"},
	}
	plan := m.StaticPlan()
	if plan.Static.Dir != "" || !plan.Static.SPA || plan.Deploy.KeepReleases != keep {
		t.Errorf("%+v", plan)
	}
	if got := plan.AccessFor("production", "production"); got != "login" {
		t.Errorf("production's gate: %q", got)
	}
	if got := plan.AccessFor("pr-1", "preview"); got != "share" {
		t.Errorf("a preview's gate: %q", got)
	}
	if m.Static.Dir != "public" {
		t.Error("the manifest was changed")
	}
}

func TestCronTimeoutLimit(t *testing.T) {
	m := &Manifest{Cron: []Cron{{Name: "weekly", Schedule: "@daily", Command: "x", Timeout: Duration(48 * time.Hour)}}}
	if problems := m.validateTables(); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "a day at the most") }) {
		t.Errorf("%v", problems)
	}
	m.Cron[0].Timeout = Duration(24 * time.Hour)
	if problems := m.validateTables(); slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "a day at the most") }) {
		t.Errorf("%v", problems)
	}
}

func TestProtocolAndSticky(t *testing.T) {
	one := 1
	for _, c := range []struct {
		name string
		proc Process
		says string
	}{
		{"web", Process{Command: "x", Protocol: "h2c", Sticky: true, Instances: &one}, ""},
		{"web", Process{Command: "x", Protocol: "http"}, ""},
		{"web", Process{Command: "x", Protocol: "spdy"}, "must be http or h2c"},
		{"worker", Process{Command: "x", Protocol: "h2c"}, "for web"},
		{"worker", Process{Command: "x", Sticky: true}, "for web"},
		{"worker", Process{Command: "x", Protocol: "http"}, ""},
	} {
		err := validateProcesses(map[string]Process{c.name: c.proc}, false)
		switch {
		case c.says == "" && err != nil:
			t.Errorf("%s %+v: %v", c.name, c.proc, err)
		case c.says != "" && (err == nil || !strings.Contains(err.Error(), c.says)):
			t.Errorf("%s %+v: %v, and not %q", c.name, c.proc, err, c.says)
		}
	}
}

func TestServices(t *testing.T) {
	good := `[app]
runtime = "command"

[processes.web]
command = "./serve"

[services.db]
type = "postgres"

[services.analytics]
type = "postgres"
version = "16"
env = "ANALYTICS_DATABASE_URL"
memory = "512MiB"
previews = "none"

[services.vectors]
type = "postgres"
image = "pgvector/pgvector:pg18"
env = "VECTORS_URL"
`
	m, err := Parse([]byte(good), "moorage.toml")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Services["db"].Plan(); got.Version != "" || got.Env != "DATABASE_URL" || got.Previews != "own" || got.Type != ServicePostgres {
		t.Errorf("the default: %+v", got)
	}
	if got := m.Services["analytics"].Plan(); got.Version != "16" || got.Env != "ANALYTICS_DATABASE_URL" || got.Memory != 512<<20 || got.Previews != "none" {
		t.Errorf("one that says: %+v", got)
	}
	if got := m.Services["vectors"].Plan(); got.Version != "" || got.Image != "pgvector/pgvector:pg18" {
		t.Errorf("another image: %+v", got)
	}

	for name, try := range map[string]struct{ table, want string }{
		"a type":            {"[services.db]\ntype = \"mysql\"\n", "must be postgres or s3"},
		"a version":         {"[services.db]\ntype = \"postgres\"\nversion = \"9\"\n", "must be one of"},
		"a name":            {"[services.DB_1]\ntype = \"postgres\"\n", "a service's name"},
		"two of a type":     {"[services.a]\ntype = \"postgres\"\n\n[services.b]\ntype = \"postgres\"\n", "would both set DATABASE_URL"},
		"latest":            {"[services.db]\ntype = \"postgres\"\nimage = \"postgres:latest\"\n", "latest would change"},
		"no tag":            {"[services.db]\ntype = \"postgres\"\nimage = \"example.com:5000/postgres\"\n", "needs a tag"},
		"a variable":        {"[services.db]\ntype = \"postgres\"\nenv = \"not-a-name\"\n", "not a valid variable name"},
		"previews":          {"[services.db]\ntype = \"postgres\"\npreviews = \"shared\"\n", "must be own or none"},
		"a seed, no word":   {"[services.db]\ntype = \"postgres\"\nseed = \"production\"\n", "say sanitize"},
		"s3 sanitized":      {"[services.files]\ntype = \"s3\"\nseed = \"production\"\nsanitize = \"a.sql\"\n", "can only be none"},
		"a seed":            {"[services.db]\ntype = \"postgres\"\nseed = \"staging\"\nsanitize = \"none\"\n", "must be production"},
		"sanitize, no seed": {"[services.db]\ntype = \"postgres\"\nsanitize = \"a.sql\"\n", "there is no seed"},
		"sanitize, outside": {"[services.db]\ntype = \"postgres\"\nseed = \"production\"\nsanitize = \"../a.sql\"\n", "a path in the app"},
		"s3 version":        {"[services.files]\ntype = \"s3\"\nversion = \"1\"\n", "no version"},
		"s3 image":          {"[services.files]\ntype = \"s3\"\nimage = \"a/b:1\"\n", "cannot replace"},
	} {
		_, err := Parse([]byte("[app]\nruntime = \"command\"\n\n[processes.web]\ncommand = \"./serve\"\n\n"+try.table), "moorage.toml")
		if err == nil || !strings.Contains(err.Error(), try.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s3 := Service{Type: ServiceS3}.Plan()
	if got := strings.Join(s3.Variables(), " "); !strings.Contains(got, "S3_ENDPOINT S3_BUCKET S3_REGION S3_ACCESS_KEY_ID S3_SECRET_ACCESS_KEY") || !strings.Contains(got, "AWS_SECRET_ACCESS_KEY") {
		t.Error(got)
	}
	if got := strings.Join(Service{Type: ServiceS3, Env: "UPLOADS"}.Plan().Variables(), " "); !strings.Contains(got, "UPLOADS_ENDPOINT") || strings.Contains(got, "AWS") || strings.Contains(got, "S3_") {
		t.Error(got)
	}
	if got := imageTag("localhost:5000/a/b:1.2"); got != "1.2" {
		t.Error(got)
	}
	if got := imageTag("a/b@sha256:abc"); got != "sha256:abc" {
		t.Error(got)
	}
}
