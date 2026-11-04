package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

func TestEnvironmentCommands(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked = append(asked, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/v1")+" "+string(body))
		env := apitypes.Environment{App: "shop", Name: "staging", Kind: "named", Branch: "develop", URL: "http://shop--staging.example.com", Release: &apitypes.Release{Version: 3}, LockedBy: "me"}
		preview := apitypes.Environment{App: "shop", Name: "topic", Kind: "preview", Branch: "topic", URL: "http://shop--topic.example.com", ExpiresAt: time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)}
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/apps/shop/envs") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]apitypes.Environment{env})
		case strings.HasSuffix(r.URL.Path, "/previews") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]apitypes.Environment{preview})
		case strings.HasSuffix(r.URL.Path, "/promote"):
			json.NewEncoder(w).Encode(apitypes.Deployment{ID: "dep_1", App: "shop", URL: "http://shop.example.com", Status: "live", Release: &apitypes.Release{Version: 4}, Warnings: []string{"this build was made with other values"}})
		case strings.HasSuffix(r.URL.Path, "/apps/shop/previews"):
			json.NewEncoder(w).Encode(apitypes.Deployment{ID: "dep_2"})
		default:
			json.NewEncoder(w).Encode(env)
		}
	}))
	defer srv.Close()
	t.Setenv("MOOR_TOKEN", "t")
	t.Setenv("MOOR_APP", "shop")
	t.Setenv("MOOR_SERVER", srv.URL)
	t.Chdir(t.TempDir())

	for _, c := range []struct {
		args   []string
		asked  string
		output string
	}{
		{[]string{"environments"}, "GET /apps/shop/envs ", "shop--staging.example.com"},
		{[]string{"envs", "create", "staging", "--branch", "develop"}, `POST /apps/shop/envs {"name":"staging","branch":"develop"}`, "moor deploy --app shop --env staging"},
		{[]string{"environments", "delete", "staging"}, "DELETE /apps/shop/envs/staging ", "Removed staging"},
		{[]string{"previews"}, "GET /previews ", "2030-01-02"},
		{[]string{"previews", "--app", "other"}, "GET /previews ", "No previews yet"},
		{[]string{"previews", "create", "topic", "--detach"}, `POST /apps/shop/previews {"branch":"topic"}`, "dep_2"},
		{[]string{"previews", "pin", "topic"}, `PATCH /apps/shop/envs/topic {"pinned":true}`, "will not expire"},
		{[]string{"previews", "unpin", "topic"}, `PATCH /apps/shop/envs/topic {"pinned":false}`, ""},
		{[]string{"previews", "destroy", "topic"}, "DELETE /apps/shop/envs/topic ", "Removed topic"},
		{[]string{"promote", "--from", "staging"}, `POST /apps/shop/envs/production/promote {"from":"staging"}`, "as v4"},
		{[]string{"promote", "--from", "topic", "--to", "staging"}, `POST /apps/shop/envs/staging/promote {"from":"topic"}`, "staging of shop now serves what topic runs"},
		{[]string{"lock"}, "POST /apps/shop/envs/production/lock {}", "no deployments"},
		{[]string{"unlock", "--env", "staging"}, "POST /apps/shop/envs/staging/unlock {}", "takes deployments again"},
		{[]string{"env", "set", "KEY=value", "--production-only", "--no-restart"}, `"production_only":true`, ""},
	} {
		asked = nil
		code, out, errOut := run(t, c.args...)
		if code != 0 {
			t.Errorf("%v: exit %d\n%s\n%s", c.args, code, out, errOut)
			continue
		}
		if len(asked) == 0 || !strings.Contains(asked[len(asked)-1], c.asked) {
			t.Errorf("%v asked %q, want %q", c.args, asked, c.asked)
		}
		if !strings.Contains(out, c.output) {
			t.Errorf("%v says %q, want %q", c.args, out, c.output)
		}
	}
	if code, _, errOut := run(t, "promote"); code == 0 || !strings.Contains(errOut, "from") {
		t.Errorf("promote without --from: %d %s", code, errOut)
	}
}
