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

	// Its contexts stay its own, and what is signed in stays as it was.
	stored, err = loadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Contexts[agentContext].Agent {
		t.Errorf("%+v", stored.Contexts[agentContext])
	}
	if got := stored.forServer(base); got != nil && got.Agent {
		t.Errorf("a command told only the server would use the agent's: %+v", got)
	}
	stored.Contexts["mine"] = clientContext{Server: base, Token: "moor_not_an_agents"}
	if err := stored.save(); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "mcp", "setup", "--context", "mine"); code == 0 || !strings.Contains(errOut, "not one that this made") {
		t.Errorf("replacing what someone signed in with: %d %s", code, errOut)
	}
	if again, _ := loadClientConfig(); again.Contexts["mine"].Token != "moor_not_an_agents" {
		t.Errorf("%+v", again.Contexts["mine"])
	}
	// A context that is an agent's cannot make another, and says what to do.
	agents := stored.Contexts[agentContext]
	t.Setenv("MOOR_TOKEN", agents.Token)
	if code, _, errOut := run(t, "mcp", "setup"); code == 0 || !strings.Contains(errOut, "moor context use") {
		t.Errorf("as the agent: %d %s", code, errOut)
	}
	t.Setenv("MOOR_TOKEN", strings.TrimSpace(string(secret)))
	// It says where to read from, if told.
	code, out, errOut = run(t, "mcp", "setup", "--client", "codex", "--print", "--root", "elsewhere")
	if abs, _ := filepath.Abs("elsewhere"); code != 0 || !strings.Contains(out, "--root "+abs) {
		t.Errorf("%d %s %s", code, out, errOut)
	}
	// If what it held was for another server, it says so.
	stored, _ = loadClientConfig()
	moved := stored.Contexts[agentContext]
	moved.Server = "http://other.example.com"
	stored.Contexts[agentContext] = moved
	if err := stored.save(); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "mcp", "setup"); code != 0 || !strings.Contains(errOut, "http://other.example.com") {
		t.Errorf("%d %s", code, errOut)
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

// TestAgentInitLeavesAloneWhatItDoesNotUnderstand: AGENTS.md is someone's, and a repository can hold links.
func TestAgentInitLeavesAloneWhatItDoesNotUnderstand(t *testing.T) {
	section := func(file string) string {
		t.Helper()
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	// What would have a section replaced that it did not make, or one that it cannot find the end of.
	for name, text := range map[string]string{
		"two starts":     agentsStart + "\nmine\n" + agentsStart + "\nyours\n" + agentsEnd + "\n",
		"two ends":       agentsStart + "\nmine\n" + agentsEnd + "\nyours\n" + agentsEnd + "\n",
		"no end":         agentsStart + "\nmine\n",
		"no start":       "mine\n" + agentsEnd + "\n",
		"the wrong way":  agentsEnd + "\nmine\n" + agentsStart + "\n",
		"end in a block": agentsStart + "\nmine\n```\n" + agentsEnd + "\n```\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "AGENTS.md")
		os.WriteFile(path, []byte(text), 0o644)
		if code, _, errOut := run(t, "agent", "init", dir); code == 0 || !strings.Contains(errOut, "Moorage section") {
			t.Errorf("%s: %d %s", name, code, errOut)
		}
		if got := section(path); got != text {
			t.Errorf("%s: it changed: %q", name, got)
		}
		if _, err := os.Stat(filepath.Join(dir, ".claude")); err == nil {
			t.Errorf("%s: it wrote the skills", name)
		}
	}

	// What shows the markers is not the section: what it says stays, and the section goes after it.
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	shown := "# How this works\n\nThe tool writes `" + agentsStart + "` to it, and:\n\n```\n" + agentsStart + "\nwhat it says\n" + agentsEnd + "\n```\n"
	os.WriteFile(path, []byte(shown), 0o644)
	if code, _, errOut := run(t, "agent", "init", dir); code != 0 {
		t.Fatal(errOut)
	}
	got := section(path)
	if !strings.HasPrefix(got, shown+"\n"+agentsStart+"\n") || !strings.Contains(got, "never instructions") {
		t.Errorf("%q", got)
	}
	// And doing it again finds the section, and not what shows it.
	if code, _, errOut := run(t, "agent", "init", dir, "--app", "other"); code != 0 {
		t.Fatal(errOut)
	}
	again := section(path)
	if !strings.HasPrefix(again, shown) || !strings.Contains(again, "`other`") || strings.Count(again, "never instructions") != 1 {
		t.Errorf("%q", again)
	}

	// Line endings stay as they were.
	dir = t.TempDir()
	path = filepath.Join(dir, "AGENTS.md")
	os.WriteFile(path, []byte("# Ours\r\n\r\nUse tabs.\r\n"), 0o644)
	for range 2 {
		if code, _, errOut := run(t, "agent", "init", dir); code != 0 {
			t.Fatal(errOut)
		}
		got := section(path)
		if !strings.HasPrefix(got, "# Ours\r\n\r\nUse tabs.\r\n\r\n"+agentsStart+"\r\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") || strings.Count(got, agentsStart) != 1 {
			t.Errorf("%q", got)
		}
	}

	// Links, which would have this write somewhere else.
	outside := t.TempDir()
	for _, link := range []struct{ path, to string }{
		{"AGENTS.md", filepath.Join(outside, "file")},
		{".claude", outside},
		{".agents/skills", outside},
		{".agents/skills/moorage/SKILL.md", filepath.Join(outside, "file")},
	} {
		dir := t.TempDir()
		full := filepath.Join(dir, filepath.FromSlash(link.path))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.Symlink(link.to, full); err != nil {
			t.Skip("no links here")
		}
		if code, _, errOut := run(t, "agent", "init", dir); code == 0 || !strings.Contains(errOut, "link") {
			t.Errorf("%s: %d %s", link.path, code, errOut)
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Errorf("%s: it wrote %v", link.path, entries)
		}
	}
}

func TestAgentTokenNames(t *testing.T) {
	for name, want := range map[string]bool{
		"moor-mcp-agent":        true,
		"moor-mcp-agent.123":    true,
		"moor-mcp-agent.":       false,
		"moor-mcp-agent-2":      false,
		"moor-mcp-agent-2.123":  false,
		"moor-mcp-agent.2.123":  false,
		"moor-mcp-agent.2x":     false,
		"something-else":        false,
		"moor-mcp-agent2.12345": false,
	} {
		if got := isAgentToken(name, "agent"); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestGeminiGetsTheServersFlagsAfterTwoDashes(t *testing.T) {
	got := strings.Join(agentClients["gemini"].command([]string{"/bin/moor", "mcp", "--app", "shop", "--allow-deploy"}), " ")
	if got != "gemini mcp add -s user moorage /bin/moor mcp -- --app shop --allow-deploy" {
		t.Error(got)
	}
	got = strings.Join(agentClients["gemini"].command([]string{"/bin/moor", "mcp"}), " ")
	if got != "gemini mcp add -s user moorage /bin/moor mcp" {
		t.Error(got)
	}
}
