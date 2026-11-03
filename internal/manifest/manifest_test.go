package manifest

import (
	"os"
	"path/filepath"
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
		"[processes.web]\ninstances = 3\n":                            "processes.web.command is required",
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
