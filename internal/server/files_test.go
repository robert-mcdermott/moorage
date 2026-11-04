package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func TestDataFiles(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	data := filepath.Join(cfg.DataDir, "data", "shop", "production")

	// What is there is listed, with directories first.
	if err := os.MkdirAll(filepath.Join(data, "uploads", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "notes.txt"), []byte("what the app wrote"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "uploads", "a.bin"), []byte{0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := c.DataFiles(ctx, "shop", "production", "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]apitypes.DataFile{}
	for _, f := range list.Files {
		names[f.Name] = f
	}
	if list.Files[0].Kind != "directory" || names["uploads"].Kind != "directory" || names["notes.txt"].Kind != "file" || names["notes.txt"].Size != 18 || names["notes.txt"].Modified.IsZero() {
		t.Errorf("%+v", list)
	}
	list, err = c.DataFiles(ctx, "shop", "production", "uploads")
	if err != nil || len(list.Files) != 2 || list.Files[0].Name != "deep" || list.Path != "/uploads" {
		t.Errorf("%+v %v", list, err)
	}

	// Reading.
	from, err := c.GetDataFile(ctx, "shop", "production", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(from)
	from.Close()
	if string(got) != "what the app wrote" {
		t.Errorf("%q", got)
	}

	// Writing appears all at once, under the name given, and does not take the place of something by chance.
	if _, err := c.PutDataFile(ctx, "shop", "production", "seed.sql", strings.NewReader("first"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutDataFile(ctx, "shop", "production", "seed.sql", strings.NewReader("second"), false); status(err) != http.StatusConflict {
		t.Errorf("%v", err)
	}
	if put, err := c.PutDataFile(ctx, "shop", "production", "seed.sql", strings.NewReader("second"), true); err != nil || put.Size != 6 {
		t.Errorf("%+v %v", put, err)
	}
	if content, _ := os.ReadFile(filepath.Join(data, "seed.sql")); string(content) != "second" {
		t.Errorf("%q", content)
	}
	if _, err := c.PutDataFile(ctx, "shop", "production", "nowhere/seed.sql", strings.NewReader("x"), false); status(err) != http.StatusNotFound {
		t.Errorf("into a directory that is not there: %v", err)
	}
	if _, err := c.PutDataFile(ctx, "shop", "production", "uploads", strings.NewReader("x"), true); status(err) != http.StatusConflict {
		t.Errorf("over a directory: %v", err)
	}
	entries, _ := os.ReadDir(data)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "moorage-upload") {
			t.Errorf("left behind: %s", entry.Name())
		}
	}

	// Directories, and removing.
	if err := c.MakeDataDir(ctx, "shop", "production", "made"); err != nil {
		t.Fatal(err)
	}
	if err := c.MakeDataDir(ctx, "shop", "production", "made"); status(err) != http.StatusConflict {
		t.Errorf("%v", err)
	}
	if err := c.RemoveData(ctx, "shop", "production", "uploads", false); status(err) != http.StatusConflict {
		t.Errorf("a directory with something in it: %v", err)
	}
	if err := c.RemoveData(ctx, "shop", "production", "uploads", true); err != nil {
		t.Errorf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "uploads")); !os.IsNotExist(err) {
		t.Errorf("%v", err)
	}
	if err := c.RemoveData(ctx, "shop", "production", "", true); status(err) != http.StatusConflict {
		t.Errorf("the data directory itself: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Error(err)
	}

	// Nothing leads out of it, however it is asked.
	secret := filepath.Join(cfg.DataDir, "data", "shop", "outside.txt")
	if err := os.WriteFile(secret, []byte("not for you"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sneaky := range []string{"../outside.txt", "made/../../outside.txt", "..\\outside.txt", "a\x00b"} {
		from, err := c.GetDataFile(ctx, "shop", "production", sneaky)
		if err == nil {
			content, _ := io.ReadAll(from)
			from.Close()
			if strings.Contains(string(content), "not for you") {
				t.Errorf("%q got out", sneaky)
			}
		}
	}
	if _, err := c.PutDataFile(ctx, "shop", "production", "../written.txt", strings.NewReader("x"), true); err == nil {
		if _, stat := os.Stat(filepath.Join(cfg.DataDir, "data", "shop", "written.txt")); stat == nil {
			t.Error("a write got out")
		}
	}
	if err := c.RemoveData(ctx, "shop", "production", "../outside.txt", false); err == nil {
		if _, stat := os.Stat(secret); stat != nil {
			t.Error("a removal got out")
		}
	}

	if runtime.GOOS != "windows" {
		// An app can leave a link to anything, and the server may be able to read it.
		if err := os.Symlink(secret, filepath.Join(data, "link-out")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../outside.txt", filepath.Join(data, "relative-out")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Dir(secret), filepath.Join(data, "dir-out")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("seed.sql", filepath.Join(data, "link-in")); err != nil {
			t.Fatal(err)
		}
		for _, link := range []string{"link-out", "relative-out", "dir-out/outside.txt"} {
			if _, err := c.GetDataFile(ctx, "shop", "production", link); status(err) == 0 {
				t.Errorf("%s was followed out", link)
			}
		}
		if _, err := c.DataFiles(ctx, "shop", "production", "dir-out"); status(err) == 0 {
			t.Error("a link to a directory was followed out")
		}
		if _, err := c.PutDataFile(ctx, "shop", "production", "dir-out/written.txt", strings.NewReader("x"), true); status(err) == 0 {
			t.Error("a write went through a link out")
		}
		if err := c.MakeDataDir(ctx, "shop", "production", "dir-out/made"); status(err) == 0 {
			t.Error("a directory was made through a link out")
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(secret), "written.txt")); err == nil {
			t.Error("something was written outside")
		}
		// One that stays inside works, and replacing or removing a link is about the link.
		from, err := c.GetDataFile(ctx, "shop", "production", "link-in")
		if err != nil {
			t.Errorf("a link that stays inside: %v", err)
		} else {
			from.Close()
		}
		if err := c.RemoveData(ctx, "shop", "production", "link-out", false); err != nil {
			t.Errorf("%v", err)
		}
		if content, _ := os.ReadFile(secret); string(content) != "not for you" {
			t.Error("removing a link reached what it led to")
		}
		if _, err := c.PutDataFile(ctx, "shop", "production", "relative-out", strings.NewReader("over"), true); err != nil {
			t.Errorf("%v", err)
		}
		if content, _ := os.ReadFile(secret); string(content) != "not for you" {
			t.Error("replacing a link wrote through it")
		}
		list, _ := c.DataFiles(ctx, "shop", "production", "")
		for _, f := range list.Files {
			if f.Name == "dir-out" && !f.Link {
				t.Errorf("a link shows as %+v", f)
			}
		}
		fifo := filepath.Join(data, "a-pipe")
		if err := mkfifo(fifo); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetDataFile(ctx, "shop", "production", "a-pipe"); status(err) != http.StatusUnprocessableEntity {
			t.Errorf("reading a pipe: %v", err)
		}
		if err := os.Remove(fifo); err != nil {
			t.Fatal(err)
		}
	}

	// Each environment has its own, and one that is not there has nothing, and is not made ready for what is to come.
	if _, err := c.DataFiles(ctx, "shop", "nothing", ""); status(err) != http.StatusNotFound {
		t.Errorf("an environment that is not there: %v", err)
	}
	if _, err := c.PutDataFile(ctx, "shop", "nothing", "planted.db", strings.NewReader("x"), false); status(err) != http.StatusNotFound {
		t.Errorf("a file for an environment that is not there: %v", err)
	}
	if err := c.MakeDataDir(ctx, "shop", "nothing", "planted"); status(err) != http.StatusNotFound {
		t.Errorf("a directory for an environment that is not there: %v", err)
	}
	if _, err := c.PutDataFile(ctx, "future", "production", "planted.db", strings.NewReader("x"), false); status(err) != http.StatusNotFound {
		t.Errorf("a file for an app that is not there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "data", "future")); !os.IsNotExist(err) {
		t.Errorf("a place was made for an app that is not there: %v", err)
	}

	// A file with another name may be what an app could not read for itself, if the system lets it link that here.
	if runtime.GOOS != "windows" {
		elsewhere := filepath.Join(cfg.DataDir, "data", "shop", "linked-from.txt")
		if err := os.WriteFile(elsewhere, []byte("not the app's"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(elsewhere, filepath.Join(data, "hard-linked")); err != nil {
			t.Fatal(err)
		}
		// Which is only a question for a server that can read what the app cannot: the isolation drill tries that.
		if os.Geteuid() != 0 {
			from, err := c.GetDataFile(ctx, "shop", "production", "hard-linked")
			if err != nil {
				t.Errorf("a file with another name, where apps can read all the server can: %v", err)
			} else {
				from.Close()
			}
		}
	}

	// Who may.
	owner := signIn(t, cfg, addrs)
	if _, err := tokenClient(t, c, tokenOf(t, owner, auth.ScopeRead)).DataFiles(ctx, "shop", "production", ""); status(err) != http.StatusForbidden {
		t.Errorf("a read-only token: %v", err)
	}
	viewer := addTeammate(t, cfg, addrs, owner, "someone@example.com")
	if _, err := c.SetMember(ctx, "default", "someone@example.com", store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if res, _ := viewer.get("/apps/shop/files"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer's page: %d", res.StatusCode)
	}
	if res, _ := viewer.get("/apps/shop/files/download?path=seed.sql"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer's download: %d", res.StatusCode)
	}
	if res, _ := viewer.get("/apps/shop/console"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer's console: %d", res.StatusCode)
	}
	if res, _ := viewer.post("/apps/shop/console/run", url.Values{"csrf": {viewer.token()}, "command": {"true"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer's command: %d", res.StatusCode)
	}
}

func TestFilesInTheConsole(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	deployedSource(t, c, "shop", appManifest(""))
	data := filepath.Join(cfg.DataDir, "data", "shop", "production")
	if err := os.MkdirAll(filepath.Join(data, "uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "page.html"), []byte(`<script>alert("from the app")</script>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "binary"), []byte{0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}

	res, body := owner.get("/apps/shop/files")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "page.html") || !strings.Contains(body, "uploads/") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}

	// What an app wrote is shown as text, and sent only as a download.
	res, body = owner.get("/apps/shop/files?path=page.html")
	if res.StatusCode != http.StatusOK || strings.Contains(body, `<script>alert`) || !strings.Contains(body, "&lt;script&gt;alert") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	res, body = owner.get("/apps/shop/files/download?path=page.html")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") || res.Header.Get("Content-Type") != "application/octet-stream" || res.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(body, "<script>") {
		t.Errorf("%d %v %s", res.StatusCode, res.Header, body)
	}
	res, body = owner.get("/apps/shop/files?path=binary")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "not text") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, _ := owner.get("/apps/shop/files?path=nothing"); res.StatusCode != http.StatusNotFound {
		t.Errorf("%d", res.StatusCode)
	}

	// Putting a file there, whatever name it comes with.
	upload := func(name, into string, more ...string) (*http.Response, string) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.WriteField("csrf", owner.token())
		w.WriteField("path", into)
		for i := 0; i+1 < len(more); i += 2 {
			w.WriteField(more[i], more[i+1])
		}
		part, _ := w.CreateFormFile("file", name)
		part.Write([]byte("uploaded by someone"))
		w.Close()
		return owner.postRaw("/apps/shop/files/upload", w.FormDataContentType(), buf.Bytes())
	}
	if res, body := upload("up.txt", "uploads"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if content, err := os.ReadFile(filepath.Join(data, "uploads", "up.txt")); err != nil || string(content) != "uploaded by someone" {
		t.Errorf("%q %v", content, err)
	}
	if res, body := upload("up.txt", "uploads"); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "already") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, body := upload("up.txt", "uploads", "replace", "1"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, body := upload("../../../sneaky.txt", ""); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(data, "sneaky.txt")); err != nil {
		t.Errorf("the name was not kept to the directory: %v", err)
	}

	// No one who may not do this gets to send it any of a file: it would go to disk. A session is only for reading the rest.
	if res, _ := (&consoleUser{t: t, addr: addrs.HTTP, host: cfg.ConsoleHost}).postRaw("/apps/shop/files/upload", "multipart/form-data; boundary=x", []byte("--x--")); res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "/login") {
		t.Errorf("no one at all: %d %v", res.StatusCode, res.Header)
	}
	if res, _ := owner.get("/apps/shop/files?env=../../keys"); res.StatusCode != http.StatusNotFound {
		t.Errorf("an environment that is a path: %d", res.StatusCode)
	}
	if res, _ := owner.get("/apps/shop/files/download?env=../../keys&path=master.key"); res.StatusCode != http.StatusNotFound {
		t.Errorf("downloading from an environment that is a path: %d", res.StatusCode)
	}
	var noToken bytes.Buffer
	noCSRF := multipart.NewWriter(&noToken)
	part, _ := noCSRF.CreateFormFile("file", "no-token.txt")
	part.Write([]byte("x"))
	noCSRF.Close()
	if res, _ := owner.postRaw("/apps/shop/files/upload", noCSRF.FormDataContentType(), noToken.Bytes()); res.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(data, "no-token.txt")); err == nil {
		t.Error("it was put there with no CSRF token")
	}

	// A directory, and removing.
	if res, body := owner.post("/apps/shop/files/folder", url.Values{"csrf": {owner.token()}, "path": {"uploads"}, "name": {"made"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, body := owner.post("/apps/shop/files/folder", url.Values{"csrf": {owner.token()}, "name": {"a/b"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(data, "uploads", "made")); err != nil {
		t.Error(err)
	}
	if res, body := owner.post("/apps/shop/files/remove", url.Values{"csrf": {owner.token()}, "path": {"uploads"}, "everything": {"1"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(data, "uploads")); !os.IsNotExist(err) {
		t.Errorf("%v", err)
	}
	if res, _ := owner.post("/apps/shop/files/remove", url.Values{"path": {"binary"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("with no CSRF token: %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(data, "binary")); err != nil {
		t.Error(err)
	}
}

func TestLineConsole(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	deployedSource(t, c, "shop", appManifest(""))
	if res, body := owner.get("/apps/shop/console"); res.StatusCode != http.StatusOK || !strings.Contains(body, "Run a command") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	// With no script on the page, it goes to the run's own.
	res, body := owner.post("/apps/shop/console/run", url.Values{"csrf": {owner.token()}, "command": {"echo from-the-console"}})
	if res.StatusCode != http.StatusSeeOther || !regexp.MustCompile(`/apps/shop/runs/run_`).MatchString(res.Header.Get("Location")) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	id := filepath.Base(res.Header.Get("Location"))
	run, said, _ := followedRun(t, c, "shop", id)
	if run.Trigger != "console" || !strings.Contains(said, "from-the-console") {
		t.Errorf("%+v %q", run, said)
	}
	// With one, it is told where to follow it.
	var started map[string]string
	if err := owner.postJSON("/apps/shop/console/run", url.Values{"csrf": {owner.token()}, "command": {"echo again"}}, &started); err != nil {
		t.Fatal(err)
	}
	if started["id"] == "" || !strings.HasPrefix(started["log"], "/api/v1/apps/shop/runs/") || !strings.HasSuffix(started["stop"], "/stop") {
		t.Errorf("%v", started)
	}
	followedRun(t, c, "shop", started["id"])
	// What it cannot do, it says.
	var refused map[string]string
	if err := owner.postJSON("/apps/shop/console/run", url.Values{"csrf": {owner.token()}, "command": {" "}}, &refused); err != nil {
		t.Fatal(err)
	}
	if refused["error"] == "" {
		t.Errorf("%v", refused)
	}
	if res, body := owner.post("/apps/shop/console/run", url.Values{"csrf": {owner.token()}, "command": {" "}}); res.StatusCode != http.StatusConflict || !strings.Contains(body, "no command") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, _ := owner.post("/apps/shop/console/run", url.Values{"command": {"true"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("with no CSRF token: %d", res.StatusCode)
	}
	// What was run is listed, for going back to.
	res, body = owner.get("/apps/shop/console")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "echo from-the-console") || !strings.Contains(body, "Earlier commands") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, body = owner.get("/apps/shop/runs/" + id); res.StatusCode != http.StatusOK || !strings.Contains(body, "from-the-console") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
}
