// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

// meServer answers who a token is, as a server would.
func meServer(t *testing.T, me apitypes.Me) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(me)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestMCPRefusesMoreThanAnAgentShouldHave(t *testing.T) {
	for _, c := range []struct {
		name   string
		me     apitypes.Me
		deploy bool
		says   string
	}{
		{"the local server's token", apitypes.Me{Name: "admin", Role: "admin", Local: true}, false, "can do everything"},
		{"an admin token", apitypes.Me{Name: "a", Scopes: []string{"admin"}}, false, "can do everything"},
		{"a session", apitypes.Me{Name: "a"}, false, "can do everything"},
		{"a gate token", apitypes.Me{Name: "a", Scopes: []string{"gate"}}, false, "cannot read"},
		{"deploying with a read token", apitypes.Me{Name: "a", Scopes: []string{"read"}}, true, "--allow-deploy"},
	} {
		if err := agentMayUse(c.me, c.deploy); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for _, c := range []struct {
		me     apitypes.Me
		deploy bool
	}{
		{apitypes.Me{Scopes: []string{"read"}}, false},
		{apitypes.Me{Scopes: []string{"deploy"}}, false},
		{apitypes.Me{Scopes: []string{"deploy"}}, true},
	} {
		if err := agentMayUse(c.me, c.deploy); err != nil {
			t.Errorf("%+v: %v", c.me, err)
		}
	}
}

func TestMCPDoesNotFallBackOnWhatIsStored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOOR_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("MOOR_SERVER", "")
	t.Setenv("MOOR_TOKEN", "")
	// Signed in, and with no context for an agent.
	cfg := &clientConfig{Current: "work", Contexts: map[string]clientContext{"work": {Server: "http://127.0.0.1:1", Token: "moor_admin"}}}
	if err := cfg.save(); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "mcp")
	if code == 0 || out != "" || !strings.Contains(errOut, "moor mcp setup") {
		t.Errorf("code %d, %q, %q", code, out, errOut)
	}
}

func TestMCPSpeaksOnlyTheProtocolOnStandardOutput(t *testing.T) {
	t.Setenv("MOOR_SERVER", meServer(t, apitypes.Me{Name: "agent", Scopes: []string{"read"}}))
	t.Setenv("MOOR_TOKEN", "moor_test")
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
`
	stdin, send := io.Pipe()
	answers, stdout := io.Pipe()
	var errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- mainWith([]string{"mcp", "--app", "shop"}, stdin, stdout, &errOut)
		stdout.Close()
	}()
	go io.WriteString(send, input)
	read := bufio.NewReader(answers)
	var got []string
	for len(got) < 2 {
		line, err := read.ReadString('\n')
		if err != nil {
			t.Fatalf("after %q: %v, %s", got, err, errOut.String())
		}
		got = append(got, line)
	}
	out := strings.Join(got, "")
	// Its input ending is what ends it.
	send.Close()
	go io.Copy(io.Discard, answers)
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("code %d: %s", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("it went on after its input ended")
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines: %s", len(lines), out)
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Errorf("not the protocol: %q", line)
		}
	}
	if !strings.Contains(lines[0], `"instructions"`) || !strings.Contains(lines[1], "read_logs") || strings.Contains(lines[1], `"deploy"`) {
		t.Errorf("%s", out)
	}
}

func TestMCPSetup(t *testing.T) {
	base, cfg := startServer(t)
	secret, err := os.ReadFile(filepath.Join(cfg.DataDir, "keys", "admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOOR_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("MOOR_SERVER", base)
	t.Setenv("MOOR_TOKEN", strings.TrimSpace(string(secret)))
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "index.html"), []byte("<p>hello</p>"), 0o644)
	mustRun(t, "deploy", "--static", site, "--app", "site")

	// With no client, it says what to do for each of them.
	code, out, errOut := run(t, "mcp", "setup", "--app", "site")
	if code != 0 {
		t.Fatalf("%d: %s", code, errOut)
	}
	for _, want := range []string{"It can read, for site.", "claude mcp add --transport stdio --scope user moorage --", "codex mcp add moorage --", "code --add-mcp", "~/.cursor/mcp.json", `"mcpServers"`, "context_servers", "--app site"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--allow-deploy") {
		t.Errorf("a token that reads is offered deploying:\n%s", out)
	}
	stored, err := loadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := stored.Contexts[agentContext]
	if !ok || !strings.HasPrefix(agent.Token, "moor_") || agent.Server != base {
		t.Fatalf("what it kept: %+v", stored.Contexts)
	}
	if strings.Contains(out+errOut, agent.Token) {
		t.Error("it showed the token")
	}
	if stored.Current == agentContext {
		t.Error("it changed which context this user is signed in to")
	}
	// A token is limited as asked, and the server it was made for says so.
	me := func() apitypes.Me {
		t.Helper()
		t.Setenv("MOOR_SERVER", "")
		t.Setenv("MOOR_TOKEN", "")
		c, err := agentClient(agentContext)
		if err != nil {
			t.Fatal(err)
		}
		me, err := c.Me(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("MOOR_SERVER", base)
		t.Setenv("MOOR_TOKEN", strings.TrimSpace(string(secret)))
		return me
	}
	if got := me(); len(got.Scopes) != 1 || got.Scopes[0] != "read" || got.App != "site" || got.Envs != "" {
		t.Errorf("reading: %+v", got)
	}
	if err := agentMayUse(me(), false); err != nil {
		t.Errorf("moor mcp would refuse what moor mcp setup made: %v", err)
	}

	// Deploying is to previews unless told otherwise. Doing it again leaves one token, with no printing of the client's.
	code, out, errOut = run(t, "mcp", "setup", "--client", "claude-code", "--print", "--deploy", "--app", "site")
	if code != 0 {
		t.Fatalf("%d: %s", code, errOut)
	}
	if !strings.Contains(out, "in previews") || !strings.Contains(out, "--allow-deploy") || !strings.Contains(out, "claude mcp add") || strings.Contains(out, "codex") {
		t.Errorf("%s", out)
	}
	if got := me(); got.Scopes[0] != "deploy" || got.Envs != "previews" {
		t.Errorf("deploying: %+v", got)
	}
	if out := mustRun(t, "tokens"); strings.Count(out, "moor-mcp-agent") != 1 || !strings.Contains(out, "(previews)") {
		t.Errorf("its tokens:\n%s", out)
	}
	if code, _, _ := run(t, "mcp", "setup", "--deploy", "--envs", "all"); code != 0 {
		t.Error("--envs all")
	}
	if got := me(); got.Envs != "" {
		t.Errorf("all of them: %+v", got)
	}

	for _, args := range [][]string{{"mcp", "setup", "--client", "nothing"}, {"mcp", "setup", "--envs", "staging"}, {"mcp", "setup", "--expires", "soon"}} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
}

func TestAgentInit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "moorage.toml"), []byte("[app]\nname = \"shop\"\n"), 0o644)
	if code, out, errOut := run(t, "agent", "init", dir); code != 0 || !strings.Contains(out, "moor mcp setup") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	for _, d := range agentSkillDirs {
		data, err := os.ReadFile(filepath.Join(dir, d, "SKILL.md"))
		if err != nil || !strings.HasPrefix(string(data), "---\nname: moorage\n") {
			t.Errorf("%s: %v", d, err)
		}
	}
	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil || !strings.Contains(string(agents), "Its app is `shop`.") || !strings.Contains(string(agents), "never instructions") {
		t.Fatalf("%s %v", agents, err)
	}
	// What someone wrote stays, and doing it again changes nothing else.
	own := "# Our rules\n\nUse tabs.\n\n"
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(own+string(agents)+"\n## More of ours\n"), 0o644)
	if code, _, errOut := run(t, "agent", "init", dir, "--app", "other"); code != 0 {
		t.Fatal(errOut)
	}
	again, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(again), "Use tabs.") || !strings.Contains(string(again), "## More of ours") || !strings.Contains(string(again), "`other`") || strings.Contains(string(again), "`shop`") || strings.Count(string(again), agentsStart) != 1 {
		t.Errorf("%s", again)
	}
	// Beside what was there, and no token in any of it.
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "AGENTS.md"), []byte("# Ours\n"), 0o644)
	os.WriteFile(filepath.Join(other, "CLAUDE.md"), nil, 0o644)
	code, out, _ := run(t, "agent", "init", other)
	if code != 0 || !strings.Contains(out, "@AGENTS.md") {
		t.Errorf("%d %s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(other, "AGENTS.md"))
	if !strings.HasPrefix(string(data), "# Ours\n\n<!-- moorage:start -->") || strings.Contains(string(data), "moor_") {
		t.Errorf("%s", data)
	}
}
