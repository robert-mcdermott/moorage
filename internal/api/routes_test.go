package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/store"
)

const localToken = "moor_local"

// fixture is a server whose routes' handlers are stubs: it tests the guard.
type fixture struct {
	s      *Server
	mux    *http.ServeMux
	tokens map[string]string
	cookie *http.Cookie
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	svc := &auth.Service{DB: db, LocalToken: auth.Hash(localToken), ClaimFile: filepath.Join(dir, "claim")}
	f := &fixture{s: &Server{db: db, auth: svc}, mux: http.NewServeMux(), tokens: map[string]string{}}
	for _, rt := range f.s.routes() {
		rt.handler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }
		f.mux.HandleFunc(rt.method+" "+basePath+rt.path, f.s.guard(rt))
	}
	if _, _, err := db.EnsureApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.EnsureApp(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	admin := f.addUser(t, "admin@example.com", store.RoleAdmin)
	member := f.addUser(t, "member@example.com", store.RoleMember)
	add := func(name, userID, app string, scopes ...string) {
		secret := auth.NewToken()
		if err := db.CreateToken(ctx, store.APIToken{ID: ids.New(ids.Token), UserID: userID, Name: name, TokenHash: auth.Hex(secret), Scopes: scopes, App: app, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		f.tokens[name] = secret
	}
	add("admin-all", admin.ID, "", auth.ScopeAdmin)
	add("admin-read", admin.ID, "", auth.ScopeRead)
	add("member-all", member.ID, "", auth.ScopeAdmin)
	add("member-deploy", member.ID, "", auth.ScopeDeploy)
	add("member-read", member.ID, "", auth.ScopeRead)
	add("member-gate", member.ID, "", auth.ScopeGate)
	add("shop-all", member.ID, "shop", auth.ScopeAdmin)
	add("other-all", member.ID, "other", auth.ScopeAdmin)
	f.tokens["local"] = localToken

	value, err := svc.StartSession(ctx, member, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	f.cookie = svc.SessionCookie(value)
	return f
}

func (f *fixture) addUser(t *testing.T, email, role string) store.User {
	t.Helper()
	u := store.User{ID: ids.New(ids.User), Email: email, Name: email, Role: role, CreatedAt: time.Now()}
	if err := f.s.db.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *fixture) do(method, path string, prepare func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "127.0.0.1:5000"
	if prepare != nil {
		prepare(r)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

var params = regexp.MustCompile(`\{[a-z]+\}`)

// concrete fills in a route's path: {app} is the app shop.
func concrete(rt route) string {
	return basePath + params.ReplaceAllStringFunc(rt.path, func(p string) string {
		if p == "{app}" {
			return "shop"
		}
		return "x"
	})
}

// TestEveryRouteDeclaresItsPermission enumerates the routes: each declares a
// permission, and each caller gets exactly what that permission allows.
func TestEveryRouteDeclaresItsPermission(t *testing.T) {
	f := newFixture(t)
	// Who may use a route of each permission, for the app shop (or no app).
	allowed := map[permission][]string{
		signedIn: {"local", "admin-all", "admin-read", "member-all", "member-deploy", "member-read", "member-gate", "shop-all", "other-all"},
		// A token for one app passes on routes that name no app: their
		// handlers show it only its own (see TestListsAndRestrictedTokens).
		canRead:     {"local", "admin-all", "admin-read", "member-all", "member-deploy", "member-read", "shop-all", "other-all"},
		canReadLogs: {"local", "admin-all", "admin-read", "member-all", "member-deploy", "member-read", "shop-all", "other-all"},
		canDeploy:   {"local", "admin-all", "member-all", "member-deploy"},
		canManage:   {"local", "admin-all", "member-all"},
		serverAdmin: {"local", "admin-all"},
	}
	count := 0
	for _, rt := range f.s.routes() {
		count++
		if rt.perm < public || rt.perm > serverAdmin {
			t.Errorf("%s %s declares no permission", rt.method, rt.path)
			continue
		}
		if rt.summary == "" {
			t.Errorf("%s %s has no summary", rt.method, rt.path)
		}
		path := concrete(rt)
		if rt.perm == public {
			if w := f.do(rt.method, path, nil); w.Code != http.StatusTeapot {
				t.Errorf("%s %s is public, but anonymously: %d", rt.method, rt.path, w.Code)
			}
			continue
		}
		if w := f.do(rt.method, path, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without credentials: %d", rt.method, rt.path, w.Code)
		}
		if w := f.do(rt.method, path, func(r *http.Request) { r.Header.Set("Authorization", "Bearer moor_wrong") }); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token: %d", rt.method, rt.path, w.Code)
		}
		for name, secret := range f.tokens {
			want := http.StatusForbidden
			for _, who := range allowed[rt.perm] {
				if who == name {
					want = http.StatusTeapot
				}
			}
			// A token for one app can use its app's routes with the admin
			// scope, and reaches no other app's.
			if strings.Contains(rt.path, "{app}") {
				switch name {
				case "shop-all":
					want = http.StatusTeapot
				case "other-all":
					want = http.StatusForbidden
				}
			}
			w := f.do(rt.method, path, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) })
			if w.Code != want {
				t.Errorf("%s %s as %s: %d, want %d: %s", rt.method, rt.path, name, w.Code, want, w.Body)
			}
		}
	}
	if count < 30 {
		t.Errorf("only %d routes", count)
	}
}

// TestRolesInAProject checks that a role in an app's project decides what its routes
// allow, whatever a token's scope, and that those with no role cannot tell the app exists.
func TestRolesInAProject(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	db := f.s.db
	if _, err := db.CreateProject(ctx, "secure"); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveApp(ctx, "shop", "secure"); err != nil {
		t.Fatal(err)
	}
	member, err := db.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	seeLogs := true
	for _, tc := range []struct {
		role                  string
		see, logs, act, admin int
		noLogs                int
	}{
		{"", http.StatusNotFound, http.StatusNotFound, http.StatusNotFound, http.StatusNotFound, http.StatusNotFound},
		{store.RoleViewer, http.StatusTeapot, http.StatusTeapot, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden},
		{store.RoleDeployer, http.StatusTeapot, http.StatusTeapot, http.StatusTeapot, http.StatusForbidden, http.StatusTeapot},
		{store.RoleMaintainer, http.StatusTeapot, http.StatusTeapot, http.StatusTeapot, http.StatusTeapot, http.StatusTeapot},
	} {
		if tc.role != "" {
			if err := db.SetMember(ctx, "secure", member.ID, tc.role); err != nil {
				t.Fatal(err)
			}
		}
		as := func(token string) func(*http.Request) {
			return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.tokens[token]) }
		}
		for _, check := range []struct {
			method, path string
			want         int
		}{
			{"GET", "/apps/shop/envs/production", tc.see},
			{"GET", "/apps/shop/envs/production/logs", tc.logs},
			{"POST", "/apps/shop/envs/production/restart", tc.act},
			{"PATCH", "/apps/shop/envs/production/vars", tc.admin},
		} {
			if w := f.do(check.method, basePath+check.path, as("member-all")); w.Code != check.want {
				t.Errorf("as a %q: %s %s: %d, want %d: %s", tc.role, check.method, check.path, w.Code, check.want, w.Body)
			}
		}
		// Viewers may be kept from the logs; those who deploy may not.
		if err := db.SetProject(ctx, "secure", "", false); err != nil {
			t.Fatal(err)
		}
		if w := f.do("GET", basePath+"/apps/shop/envs/production/logs", as("member-all")); w.Code != tc.noLogs {
			t.Errorf("as a %q with logs kept from viewers: %d, want %d", tc.role, w.Code, tc.noLogs)
		}
		if err := db.SetProject(ctx, "secure", "", seeLogs); err != nil {
			t.Fatal(err)
		}
		// A token's scope narrows a role, whatever the role.
		if tc.role == store.RoleMaintainer {
			if w := f.do("POST", basePath+"/apps/shop/envs/production/restart", as("member-read")); w.Code != http.StatusForbidden {
				t.Errorf("a read token as a maintainer: %d", w.Code)
			}
			if w := f.do("PATCH", basePath+"/apps/shop/envs/production/vars", as("member-deploy")); w.Code != http.StatusForbidden {
				t.Errorf("a deploy token as a maintainer: %d", w.Code)
			}
		}
	}
	// Administrators and the local token have no need of a role.
	for _, token := range []string{"admin-all", "local"} {
		w := f.do("PATCH", basePath+"/apps/shop/envs/production/vars", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.tokens[token]) })
		if w.Code != http.StatusTeapot {
			t.Errorf("%s: %d", token, w.Code)
		}
	}
	// Everyone can be given a role, and it is a member's own that counts.
	if err := db.RemoveMember(ctx, "secure", member.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProject(ctx, "secure", store.RoleViewer, true); err != nil {
		t.Fatal(err)
	}
	do := func(method, path string) int {
		return f.do(method, basePath+path, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.tokens["member-all"]) }).Code
	}
	if code := do("GET", "/apps/shop/envs/production"); code != http.StatusTeapot {
		t.Errorf("everyone a viewer: %d", code)
	}
	if code := do("POST", "/apps/shop/envs/production/restart"); code != http.StatusForbidden {
		t.Errorf("everyone a viewer: %d", code)
	}
	if err := db.SetMember(ctx, "secure", member.ID, store.RoleDeployer); err != nil {
		t.Fatal(err)
	}
	if code := do("POST", "/apps/shop/envs/production/restart"); code != http.StatusTeapot {
		t.Errorf("a member's own role: %d", code)
	}

	// An app that does not exist yet would be in the default project, as its first
	// deployment makes it.
	if err := db.SetProject(ctx, store.DefaultProject, "", true); err != nil {
		t.Fatal(err)
	}
	if code := do("POST", "/apps/brand-new/envs/production/deployments"); code != http.StatusNotFound {
		t.Errorf("no role in the default project: %d", code)
	}
}

func TestChangesAreAudited(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	as := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.tokens["member-read"]) }
	f.do("GET", basePath+"/apps/shop/envs/production", as)
	if list, err := f.s.db.Audit(ctx, store.AuditFilter{}); err != nil || len(list) != 0 {
		t.Errorf("a read is no change: %+v %v", list, err)
	}
	f.do("POST", basePath+"/apps/shop/envs/production/restart", as)
	list, err := f.s.db.Audit(ctx, store.AuditFilter{})
	if err != nil || len(list) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	e := list[0]
	if e.Actor != "member@example.com" || e.Action != "POST /apps/{app}/envs/{env}/restart" || e.Target != basePath+"/apps/shop/envs/production/restart" || e.Result != "refused" || e.IP != "127.0.0.1" {
		t.Errorf("%+v", e)
	}
	f.do("POST", basePath+"/apps/shop/envs/production/restart", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.tokens["member-all"]) })
	// The stand-in for the route's handler answers 418, which is a failure to record.
	if list, _ := f.s.db.Audit(ctx, store.AuditFilter{Actor: "member@example.com", Action: "restart", Limit: 1}); len(list) != 1 || list[0].Result != "failed" || list[0].Data["status"] != float64(http.StatusTeapot) {
		t.Errorf("%+v", list)
	}
	if list, _ := f.s.db.Audit(ctx, store.AuditFilter{Actor: "nobody"}); len(list) != 0 {
		t.Errorf("%+v", list)
	}
}

func TestCookieRequestsNeedCSRF(t *testing.T) {
	f := newFixture(t)
	csrf := auth.CSRFToken(f.cookie.Value)
	const path = basePath + "/me/tokens"
	withCookie := func(r *http.Request) { r.AddCookie(f.cookie) }

	if w := f.do("GET", path, withCookie); w.Code != http.StatusTeapot {
		t.Errorf("a read with the cookie: %d", w.Code)
	}
	if w := f.do("POST", path, withCookie); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"csrf"`) {
		t.Errorf("a change without the CSRF token: %d %s", w.Code, w.Body)
	}
	if w := f.do("POST", path, func(r *http.Request) {
		withCookie(r)
		r.Header.Set("X-CSRF-Token", "wrong")
	}); w.Code != http.StatusForbidden {
		t.Errorf("a wrong CSRF token: %d", w.Code)
	}
	if w := f.do("POST", path, func(r *http.Request) {
		withCookie(r)
		r.Header.Set("X-CSRF-Token", csrf)
	}); w.Code != http.StatusTeapot {
		t.Errorf("the right CSRF token: %d %s", w.Code, w.Body)
	}
	// An app on a sibling hostname is same-site, not same-origin.
	if w := f.do("POST", path, func(r *http.Request) {
		withCookie(r)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Sec-Fetch-Site", "same-site")
	}); w.Code != http.StatusForbidden {
		t.Errorf("from a sibling hostname: %d", w.Code)
	}
	// A bearer token needs no CSRF token.
	if w := f.do("POST", path, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.tokens["member-all"]) }); w.Code != http.StatusTeapot {
		t.Errorf("a bearer token: %d", w.Code)
	}
	// A session is a member's: it cannot manage users.
	if w := f.do("GET", basePath+"/users", withCookie); w.Code != http.StatusForbidden {
		t.Errorf("a member's session on an admin's route: %d", w.Code)
	}
}

func TestLocalTokenFromAnotherMachine(t *testing.T) {
	f := newFixture(t) // the fixture has users: the server is claimed
	w := f.do("GET", basePath+"/me", func(r *http.Request) {
		r.RemoteAddr = "192.0.2.1:5000"
		r.Header.Set("Authorization", "Bearer "+localToken)
	})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "local_only") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// TestOpenAPIDocument checks the checked-in document against the route table
// and the API's types. UPDATE_OPENAPI=1 writes it.
func TestOpenAPIDocument(t *testing.T) {
	s := &Server{}
	routes := s.routes()
	doc := openAPIDocument(routes)
	const file = "../../docs/openapi.json"
	if os.Getenv("UPDATE_OPENAPI") != "" {
		if err := os.WriteFile(file, doc, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	current, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// git on Windows may check the file out with CRLF line endings.
	if !bytes.Equal(bytes.ReplaceAll(current, []byte("\r\n"), []byte("\n")), doc) {
		t.Errorf("docs/openapi.json differs from the routes: run UPDATE_OPENAPI=1 go test ./internal/api -run TestOpenAPIDocument")
	}

	var parsed struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]map[string]struct {
			OperationID string         `json:"operationId"`
			Summary     string         `json:"summary"`
			Responses   map[string]any `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.OpenAPI != "3.1.0" {
		t.Errorf("openapi: %q", parsed.OpenAPI)
	}
	seen := map[string]bool{}
	for _, rt := range routes {
		op, ok := parsed.Paths[rt.path][strings.ToLower(rt.method)]
		if !ok {
			t.Errorf("%s %s is not in the document", rt.method, rt.path)
			continue
		}
		if seen[op.OperationID] {
			t.Errorf("the operation ID %s is used twice", op.OperationID)
		}
		seen[op.OperationID] = true
		if op.Summary == "" || len(op.Responses) < 2 {
			t.Errorf("%s %s: %+v", rt.method, rt.path, op)
		}
	}
	// Every $ref resolves.
	for _, m := range regexp.MustCompile(`"#/components/schemas/([A-Za-z]+)"`).FindAllSubmatch(doc, -1) {
		if _, ok := parsed.Components.Schemas[string(m[1])]; !ok {
			t.Errorf("no schema %s", m[1])
		}
	}
	for _, name := range []string{"Problem", "AppPage", "DeploymentPage", "CreatedToken", "Me"} {
		if _, ok := parsed.Components.Schemas[name]; !ok {
			t.Errorf("no schema %s", name)
		}
	}
}
