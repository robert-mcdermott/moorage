package forge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/notify"
)

func header(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerified(t *testing.T) {
	const secret, body = "the secret", `{"a":1}`
	good := sign(secret, body)
	cases := []struct {
		name, kind string
		header     http.Header
		want       bool
	}{
		{"github", GitHub, header("X-Hub-Signature-256", "sha256="+good), true},
		{"github, wrong", GitHub, header("X-Hub-Signature-256", "sha256="+sign("another", body)), false},
		{"github, without its prefix", GitHub, header("X-Hub-Signature-256", good), false},
		{"github, signed the old way only", GitHub, header("X-Hub-Signature", "sha1=abc"), false},
		{"github, nothing", GitHub, header(), false},
		{"gitea", Gitea, header("X-Gitea-Signature", good), true},
		{"gitea, wrong", Gitea, header("X-Gitea-Signature", sign("another", body)), false},
		{"gitlab", GitLab, header("X-Gitlab-Token", secret), true},
		{"gitlab, wrong", GitLab, header("X-Gitlab-Token", "another"), false},
		{"gitlab, empty", GitLab, header("X-Gitlab-Token", ""), false},
		{"generic, as github does", Generic, header("X-Hub-Signature-256", "sha256="+good), true},
		{"generic, plainly", Generic, header("X-Moorage-Signature", good), true},
		{"generic, wrong", Generic, header("X-Moorage-Signature", "abc"), false},
		{"another forge's header", Gitea, header("X-Hub-Signature-256", "sha256="+good), false},
	}
	for _, c := range cases {
		if got := Verified(c.kind, secret, c.header, []byte(body)); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

const commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestParseEvent(t *testing.T) {
	zeros := strings.Repeat("0", 40)
	cases := []struct {
		name, kind string
		header     http.Header
		body       string
		want       Event
	}{
		{"a github push", GitHub, header("X-GitHub-Event", "push"), `{"ref":"refs/heads/main","after":"` + commitA + `","sender":{"login":"ann"},"repository":{"default_branch":"trunk"}}`,
			Event{Kind: Push, Branch: "main", Commit: commitA, Author: "ann", DefaultBranch: "trunk"}},
		{"a branch with a slash", GitHub, header("X-GitHub-Event", "push"), `{"ref":"refs/heads/fix/it","after":"` + commitA + `"}`,
			Event{Kind: Push, Branch: "fix/it", Commit: commitA}},
		{"a branch deleted", GitHub, header("X-GitHub-Event", "push"), `{"ref":"refs/heads/old","after":"` + zeros + `","deleted":true}`,
			Event{Kind: Push, Branch: "old"}},
		{"a tag is kept", GitHub, header("X-GitHub-Event", "push"), `{"ref":"refs/tags/v1","after":"` + commitA + `"}`, Event{}},
		{"a ref that could not be one", GitHub, header("X-GitHub-Event", "push"), `{"ref":"refs/heads/../../../etc","after":"` + commitA + `"}`, Event{}},
		{"a ping", GitHub, header("X-GitHub-Event", "ping"), `{"zen":"Keep it logically awesome."}`, Event{}},
		{"a member's pull request", GitHub, header("X-GitHub-Event", "pull_request"),
			`{"action":"opened","number":5,"pull_request":{"title":"Fix it","user":{"login":"ann"},"author_association":"MEMBER","head":{"sha":"` + commitA + `","repo":{"full_name":"acme/shop"}},"base":{"repo":{"full_name":"acme/shop"}}}}`,
			Event{Kind: PullChanged, Number: 5, Title: "Fix it", Author: "ann", Head: commitA, Member: true}},
		{"from a fork, though a member's", GitHub, header("X-GitHub-Event", "pull_request"),
			`{"action":"synchronize","number":5,"pull_request":{"author_association":"OWNER","user":{"login":"ann"},"head":{"sha":"` + commitA + `","repo":{"full_name":"ann/shop"}},"base":{"repo":{"full_name":"acme/shop"}}}}`,
			Event{Kind: PullChanged, Number: 5, Author: "ann", Head: commitA, Member: true, FromFork: true}},
		{"someone no one knows, in the repository", GitHub, header("X-GitHub-Event", "pull_request"),
			`{"action":"opened","number":5,"pull_request":{"author_association":"FIRST_TIME_CONTRIBUTOR","head":{"sha":"` + commitA + `","repo":{"full_name":"acme/shop"}},"base":{"repo":{"full_name":"acme/shop"}}}}`,
			Event{Kind: PullChanged, Number: 5, Head: commitA}},
		{"its fork has gone", GitHub, header("X-GitHub-Event", "pull_request"),
			`{"action":"reopened","number":5,"pull_request":{"author_association":"MEMBER","head":{"sha":"` + commitA + `","repo":null},"base":{"repo":{"full_name":"acme/shop"}}}}`,
			Event{Kind: PullChanged, Number: 5, Head: commitA, Member: true, FromFork: true}},
		{"closed", GitHub, header("X-GitHub-Event", "pull_request"), `{"action":"closed","number":5,"pull_request":{"merged":true}}`,
			Event{Kind: PullClosed, Number: 5, FromFork: true}},
		{"its title changed", GitHub, header("X-GitHub-Event", "pull_request"), `{"action":"edited","number":5,"pull_request":{}}`,
			Event{Number: 5, FromFork: true}},
		{"a gitea push", Gitea, header("X-Gitea-Event", "push"), `{"ref":"refs/heads/main","after":"` + commitA + `","sender":{"login":"ann"}}`,
			Event{Kind: Push, Branch: "main", Commit: commitA, Author: "ann"}},
		{"a gitea pull request", Gitea, header("X-Gitea-Event", "pull_request"),
			`{"action":"synchronized","number":6,"pull_request":{"title":"T","user":{"login":"ann"},"head":{"sha":"` + commitA + `","repo":{"full_name":"acme/shop"}},"base":{"repo":{"full_name":"acme/shop"}}}}`,
			Event{Kind: PullChanged, Number: 6, Title: "T", Author: "ann", Head: commitA, Member: true}},
		{"a gitea fork", Gitea, header("X-Gitea-Event", "pull_request"),
			`{"action":"opened","number":6,"pull_request":{"head":{"sha":"` + commitA + `","repo":{"full_name":"ann/shop"}},"base":{"repo":{"full_name":"acme/shop"}}}}`,
			Event{Kind: PullChanged, Number: 6, Head: commitA, FromFork: true}},
		{"gitea, saying nothing of where it is from", Gitea, header("X-Gitea-Event", "pull_request"),
			`{"action":"opened","number":6,"pull_request":{"head":{"sha":"` + commitA + `"},"base":{}}}`,
			Event{Kind: PullChanged, Number: 6, Head: commitA, FromFork: true}},
		{"a gitlab push", GitLab, header("X-Gitlab-Event", "Push Hook"), `{"ref":"refs/heads/main","after":"` + commitA + `","user_username":"ann","project":{"default_branch":"main"}}`,
			Event{Kind: Push, Branch: "main", Commit: commitA, Author: "ann", DefaultBranch: "main"}},
		{"a gitlab branch deleted", GitLab, header("X-Gitlab-Event", "Push Hook"), `{"ref":"refs/heads/old","after":"` + zeros + `"}`,
			Event{Kind: Push, Branch: "old"}},
		{"a gitlab merge request", GitLab, header("X-Gitlab-Event", "Merge Request Hook"),
			`{"user":{"username":"ann"},"object_attributes":{"iid":7,"title":"T","action":"open","source_project_id":3,"target_project_id":3,"last_commit":{"id":"` + commitA + `"}}}`,
			Event{Kind: PullChanged, Number: 7, Title: "T", Author: "ann", Head: commitA, Member: true}},
		{"a gitlab fork", GitLab, header("X-Gitlab-Event", "Merge Request Hook"),
			`{"object_attributes":{"iid":7,"action":"update","source_project_id":9,"target_project_id":3,"last_commit":{"id":"` + commitA + `"}}}`,
			Event{Kind: PullChanged, Number: 7, Head: commitA, FromFork: true}},
		{"gitlab, saying nothing of where it is from", GitLab, header("X-Gitlab-Event", "Merge Request Hook"),
			`{"object_attributes":{"iid":7,"action":"open","last_commit":{"id":"` + commitA + `"}}}`,
			Event{Kind: PullChanged, Number: 7, Head: commitA, FromFork: true}},
		{"gitlab, merged", GitLab, header("X-Gitlab-Event", "Merge Request Hook"), `{"object_attributes":{"iid":7,"action":"merge"}}`,
			Event{Kind: PullClosed, Number: 7, FromFork: true}},
		{"generic", Generic, header(), `{"ref":"refs/heads/main","commit":"` + commitA + `"}`,
			Event{Kind: Push, Branch: "main", Commit: commitA}},
	}
	for _, c := range cases {
		got, err := ParseEvent(c.kind, c.header, []byte(c.body))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
	if _, err := ParseEvent(GitHub, header("X-GitHub-Event", "push"), []byte("not json")); err == nil {
		t.Error("what is not JSON was read")
	}
}

func TestWhere(t *testing.T) {
	cases := []struct {
		kind, url, api              string
		wantKind, wantAPI, wantRepo string
	}{
		{"", "https://github.com/acme/shop.git", "", GitHub, "https://api.github.com", "acme/shop"},
		{"", "https://github.com/acme/shop", "", GitHub, "https://api.github.com", "acme/shop"},
		{"", "https://gitlab.com/acme/team/shop.git", "", GitLab, "https://gitlab.com/api/v4", "acme/team/shop"},
		{GitHub, "https://git.example.com/acme/shop.git", "", GitHub, "https://git.example.com/api/v3", "acme/shop"},
		{GitLab, "https://git.example.com/acme/shop.git", "", GitLab, "https://git.example.com/api/v4", "acme/shop"},
		{Gitea, "https://git.example.com:3000/acme/shop.git", "", Gitea, "https://git.example.com:3000/api/v1", "acme/shop"},
		{Gitea, "https://git.example.com/acme/shop.git", "https://api.example.com/v1/", Gitea, "https://api.example.com/v1", "acme/shop"},
		{Generic, "https://git.example.com/shop.git", "", Generic, "", "shop"},
	}
	for _, c := range cases {
		got, err := Where(c.kind, c.url, c.api)
		if err != nil {
			t.Errorf("%s: %v", c.url, err)
			continue
		}
		if got.Kind != c.wantKind || got.APIURL != c.wantAPI || got.Repo != c.wantRepo {
			t.Errorf("%s: %+v", c.url, got)
		}
	}
	for _, bad := range []struct{ kind, url string }{
		{"", "https://git.example.com/acme/shop.git"}, // it cannot tell
		{"sourcehut", "https://git.example.com/shop.git"},
		{GitHub, "git@github.com:acme/shop.git"},
		{GitHub, "ssh://git@github.com/acme/shop.git"},
		{GitHub, "file:///etc/shop"},
		{GitHub, "https://user:token@github.com/acme/shop.git"},
		{GitHub, "not an address"},
	} {
		if _, err := Where(bad.kind, bad.url, ""); err == nil {
			t.Errorf("%s %s was taken", bad.kind, bad.url)
		}
	}
}

func TestSentSafely(t *testing.T) {
	for address, want := range map[string]bool{
		"https://github.com/a/b.git":         true,
		"http://github.com/a/b.git":          false,
		"http://git.example.com/a/b.git":     false,
		"http://8.8.8.8/a/b.git":             false,
		"http://127.0.0.1:3000/a/b.git":      true,
		"http://192.168.1.20/a/b.git":        true,
		"http://10.0.0.5:3000/a/b.git":       true,
		"http://[fd00::1]/a/b.git":           true,
		"http://gitea/a/b.git":               true,
		"http://gitea.lan/a/b.git":           true,
		"http://gitea.home.arpa/a/b.git":     true,
		"http://gitea.internal.evil.com/a/b": false,
	} {
		if got := notify.SafeForSecrets(address); got != want {
			t.Errorf("%s: %v", address, got)
		}
	}
}

// heard is a request that a forge's API was sent.
type heard struct {
	Method, Path, Auth, Private string
	Body                        map[string]string
}

func listening(t *testing.T, answer string) (*httptest.Server, func() []heard) {
	var mu sync.Mutex
	var got []heard
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h := heard{Method: r.Method, Path: r.URL.EscapedPath(), Auth: r.Header.Get("Authorization"), Private: r.Header.Get("PRIVATE-TOKEN")}
		json.Unmarshal(body, &h.Body)
		mu.Lock()
		got = append(got, h)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []heard {
		mu.Lock()
		defer mu.Unlock()
		return append([]heard(nil), got...)
	}
}

func TestReporting(t *testing.T) {
	ctx := context.Background()
	srv, requests := listening(t, `{"id": 77}`)

	github := Remote{Kind: GitHub, APIURL: srv.URL, Repo: "acme/shop", Token: "tok"}
	if err := github.SetStatus(ctx, commitA, StateFailure, "https://x/", strings.Repeat("long ", 40)); err != nil {
		t.Fatal(err)
	}
	if id, err := github.Comment(ctx, 5, "", "hello"); err != nil || id != "77" {
		t.Fatal(id, err)
	}
	if id, err := github.Comment(ctx, 5, "77", "again"); err != nil || id != "77" {
		t.Fatal(id, err)
	}
	gitea := Remote{Kind: Gitea, APIURL: srv.URL, Repo: "acme/shop", Token: "tok"}
	gitea.SetStatus(ctx, commitA, StateSuccess, "https://x/", "ok")
	gitlab := Remote{Kind: GitLab, APIURL: srv.URL, Repo: "acme/team/shop", Token: "tok"}
	gitlab.SetStatus(ctx, commitA, StateFailure, "https://x/", "no")
	gitlab.Comment(ctx, 7, "", "hello")
	gitlab.Comment(ctx, 7, "77", "again")

	got := requests()
	if len(got) != 7 {
		t.Fatalf("%+v", got)
	}
	for i, want := range []struct{ method, path string }{
		{"POST", "/repos/acme/shop/statuses/" + commitA},
		{"POST", "/repos/acme/shop/issues/5/comments"},
		{"PATCH", "/repos/acme/shop/issues/comments/77"},
		{"POST", "/repos/acme/shop/statuses/" + commitA},
		{"POST", "/projects/acme%2Fteam%2Fshop/statuses/" + commitA},
		{"POST", "/projects/acme%2Fteam%2Fshop/merge_requests/7/notes"},
		{"PUT", "/projects/acme%2Fteam%2Fshop/merge_requests/7/notes/77"},
	} {
		if got[i].Method != want.method || got[i].Path != want.path {
			t.Errorf("request %d: %s %s, not %s %s", i, got[i].Method, got[i].Path, want.method, want.path)
		}
	}
	if got[0].Auth != "Bearer tok" || got[3].Auth != "token tok" || got[4].Private != "tok" {
		t.Errorf("how they were told who this is: %q %q %q", got[0].Auth, got[3].Auth, got[4].Private)
	}
	if got[0].Body["state"] != "failure" || got[0].Body["context"] != "moorage" || len(got[0].Body["description"]) > 120 {
		t.Errorf("github's status: %+v", got[0].Body)
	}
	if got[4].Body["state"] != "failed" || got[4].Body["name"] != "moorage" {
		t.Errorf("gitlab's status: %+v", got[4].Body)
	}

	// With no token, or nowhere to say it, nothing is said.
	before := len(requests())
	quiet := Remote{Kind: GitHub, APIURL: srv.URL, Repo: "acme/shop"}
	quiet.SetStatus(ctx, commitA, StateSuccess, "", "")
	quiet.Comment(ctx, 5, "", "hello")
	generic := Remote{Kind: Generic, APIURL: srv.URL, Repo: "acme/shop", Token: "tok"}
	generic.SetStatus(ctx, commitA, StateSuccess, "", "")
	if len(requests()) != before {
		t.Error("it said something with nothing to say it with")
	}

	// An answer that is not a success is an error, which does not repeat the token.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer refusing.Close()
	err := Remote{Kind: GitHub, APIURL: refusing.URL, Repo: "acme/shop", Token: "tok"}.SetStatus(ctx, commitA, StateSuccess, "", "")
	if err == nil || strings.Contains(err.Error(), "tok") {
		t.Errorf("%v", err)
	}
}
