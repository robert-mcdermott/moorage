// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
