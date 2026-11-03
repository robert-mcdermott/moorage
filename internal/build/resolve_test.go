package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/manifest"
)

// tree writes files (path → content) under a new directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func resolve(t *testing.T, files map[string]string) manifest.Plan {
	t.Helper()
	plan, _, err := Resolve(tree(t, files), "linux")
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func toolchainOf(p manifest.Plan, name string) (manifest.Toolchain, bool) {
	for _, t := range p.Toolchains {
		if t.Name == name {
			return t, true
		}
	}
	return manifest.Toolchain{}, false
}

func steps(p manifest.Plan) string {
	var s []string
	for _, st := range p.Steps {
		s = append(s, st.Name+": "+st.Command)
	}
	return strings.Join(s, "\n")
}

func TestResolveGo(t *testing.T) {
	plan := resolve(t, map[string]string{
		"go.mod":            "module example.com/acme/hello\n\ngo 1.26.1\n",
		"cmd/hello/main.go": "package main\n\nfunc main() {}\n",
		"internal/x/x.go":   "package x\n",
	})
	if plan.Runtime != "go" || plan.Kind != manifest.KindProcess {
		t.Fatalf("plan: %+v", plan)
	}
	if tc, _ := toolchainOf(plan, "go"); tc.Version != "1.26" || tc.From != "go.mod go line" {
		t.Errorf("toolchain: %+v", tc)
	}
	want := `build: go build -trimpath -ldflags='-s -w' -o "$MOORAGE_BIN/hello" ./cmd/hello`
	if got := steps(plan); got != want {
		t.Errorf("steps:\n%s\nwant\n%s", got, want)
	}
	if plan.Processes["web"].Command != `"$MOORAGE_BIN/hello"` || plan.BuildEnv["CGO_ENABLED"] != "0" {
		t.Errorf("web %q, build env %v", plan.Processes["web"].Command, plan.BuildEnv)
	}

	plan = resolve(t, map[string]string{
		"go.mod":  "module hello\n\ngo 1.25\n\ntoolchain go1.26.2\n",
		"main.go": "package main\n\nfunc main() {}\n",
		moorage:   "[go]\ntags = [\"prod\"]\nbinary = \"srv\"\n[processes.web]\nhealth = { path = \"/healthz\" }\n",
	})
	if tc, _ := toolchainOf(plan, "go"); tc.Version != "1.26.2" {
		t.Errorf("toolchain line: %+v", tc)
	}
	if !strings.Contains(steps(plan), `-tags=prod -o "$MOORAGE_BIN/srv" .`) {
		t.Errorf("steps: %s", steps(plan))
	}
	if web := plan.Processes["web"]; web.Command != `"$MOORAGE_BIN/srv"` || web.Health.Path != "/healthz" {
		t.Errorf("web takes the preset command and keeps its health check: %+v", web)
	}
}

const moorage = "moorage.toml"

func TestResolvePython(t *testing.T) {
	plan := resolve(t, map[string]string{
		"pyproject.toml":  "[project]\nname = \"api\"\nrequires-python = \">=3.12\"\ndependencies = [\"fastapi>=0.110\", \"uvicorn[standard]\"]\n",
		"uv.lock":         "version = 1\n",
		".python-version": "3.13\n",
		"app/__init__.py": "",
		"app/main.py":     "from fastapi import FastAPI\n\napp = FastAPI()\n",
	})
	if tc, _ := toolchainOf(plan, "python"); tc.Version != "3.13" || tc.From != ".python-version" {
		t.Errorf("python: %+v", tc)
	}
	if _, ok := toolchainOf(plan, "uv"); !ok {
		t.Error("no uv")
	}
	if steps(plan) != "install: uv sync --no-dev --locked" {
		t.Errorf("steps: %s", steps(plan))
	}
	if got := plan.Processes["web"].Command; got != "uvicorn app.main:app --host 127.0.0.1 --port $PORT" {
		t.Errorf("web: %q", got)
	}
	if plan.Path[0] != ".venv/bin" || plan.DirEnv["VIRTUAL_ENV"] != ".venv" || plan.RunEnv["PYTHONUNBUFFERED"] != "1" {
		t.Errorf("environment: %v %v %v", plan.Path, plan.DirEnv, plan.RunEnv)
	}

	plan = resolve(t, map[string]string{
		"requirements.txt":  "Django==5.2\ngunicorn\n",
		"site1/settings.py": "",
		"site1/wsgi.py":     "",
	})
	if steps(plan) != "venv: uv venv .venv\ninstall: uv pip install -r requirements.txt" {
		t.Errorf("steps: %s", steps(plan))
	}
	if got := plan.Processes["web"].Command; got != "gunicorn site1.wsgi --bind 127.0.0.1:$PORT" {
		t.Errorf("web: %q", got)
	}

	plan = resolve(t, map[string]string{
		"pyproject.toml": "[project]\nname = \"tool\"\n[project.scripts]\nserve = \"tool:main\"\n[build-system]\nrequires = [\"uv_build\"]\nbuild-backend = \"uv_build\"\n",
	})
	if plan.Processes["web"].Command != "serve" || !strings.Contains(strings.Join(plan.Notes, "\n"), "uv.lock") {
		t.Errorf("web %q, notes %v", plan.Processes["web"].Command, plan.Notes)
	}

	_, _, err := Resolve(tree(t, map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n"}), "linux")
	var u *UnresolvedError
	if err == nil || !strings.Contains(err.Error(), "[processes.web]") {
		t.Errorf("no web command: %v (%T %v)", err, err, u)
	}
}

func TestResolveNode(t *testing.T) {
	plan := resolve(t, map[string]string{
		"package.json":      `{"scripts": {"build": "vite build", "start": "node dist/server.js"}, "engines": {"node": ">=22"}}`,
		"package-lock.json": "{}",
		".nvmrc":            "v24\n",
	})
	if tc, _ := toolchainOf(plan, "node"); tc.Version != "24" || tc.From != ".nvmrc" {
		t.Errorf("node: %+v", tc)
	}
	if steps(plan) != "install: npm ci\nbuild: npm run build\nprune: npm prune --omit=dev" {
		t.Errorf("steps: %s", steps(plan))
	}
	if plan.Processes["web"].Command != "node dist/server.js" || plan.RunEnv["NODE_ENV"] != "production" || plan.Path[0] != "node_modules/.bin" {
		t.Errorf("plan: %+v", plan)
	}

	dir := tree(t, map[string]string{"package.json": `{"scripts": {"build": "x"}}`})
	plan, _, err := Resolve(dir, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.StaticFallback || len(plan.Processes) != 0 {
		t.Fatalf("no start script: %+v", plan)
	}
	if tc, _ := toolchainOf(plan, "node"); tc.Version != "lts" {
		t.Errorf("node: %+v", tc)
	}
	if err := Finish(&plan, dir); err == nil {
		t.Error("finished without an output directory")
	}
	os.MkdirAll(filepath.Join(dir, "dist"), 0o755)
	os.WriteFile(filepath.Join(dir, "dist", "index.html"), []byte("hi"), 0o644)
	if err := Finish(&plan, dir); err != nil || plan.Kind != manifest.KindStatic || plan.Root != "app/dist" {
		t.Errorf("finish: %v %+v", err, plan)
	}

	_, _, err = Resolve(tree(t, map[string]string{"package.json": `{}`, "pnpm-lock.yaml": ""}), "linux")
	if err == nil || !strings.Contains(err.Error(), "pnpm") {
		t.Errorf("pnpm: %v", err)
	}
}

func TestResolveOthers(t *testing.T) {
	plan := resolve(t, map[string]string{"public/index.html": "hi"})
	if plan.Runtime != "static" || plan.Kind != manifest.KindStatic {
		t.Errorf("static: %+v", plan)
	}

	plan = resolve(t, map[string]string{
		moorage: "[app]\nruntime = \"command\"\ntoolchains = [\"go@1.26\", \"java@system\"]\n[build]\ncommand = \"make\"\npre = \"echo pre\"\n[processes]\nweb = \"./server\"\nworker = \"./worker\"\n[release]\ncommand = \"./migrate\"\n",
	})
	if steps(plan) != "pre: echo pre\nbuild: make" || plan.Release.Command != "./migrate" || len(plan.Processes) != 2 {
		t.Errorf("command: %+v", plan)
	}
	if tc, _ := toolchainOf(plan, "java"); tc.Version != "system" {
		t.Errorf("toolchains: %+v", plan.Toolchains)
	}

	plan = resolve(t, map[string]string{"Procfile": "web: ./server --port $PORT\nworker: ./worker\n", "server": ""})
	if plan.Runtime != "binary" || plan.Processes["worker"].Command != "./worker" {
		t.Errorf("Procfile: %+v", plan)
	}

	for files, want := range map[string]string{
		"nothing.txt":   "cannot tell how to build",
		"Dockerfile":    "containers",
		"deno.json":     "not available yet",
		"bad-toolchain": "cannot be installed yet",
	} {
		content := map[string]string{files: ""}
		if files == "bad-toolchain" {
			content = map[string]string{moorage: "[app]\nruntime = \"binary\"\ntoolchains = [\"ruby@3\"]\n[processes]\nweb = \"x\"\n"}
		}
		_, _, err := Resolve(tree(t, content), "linux")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", files, err)
		}
	}
}
