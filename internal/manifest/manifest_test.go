package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
