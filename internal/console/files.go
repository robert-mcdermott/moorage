package console

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// maxFilePreview is the most of a file that a page shows.
const maxFilePreview = 256 << 10

// filesView is a page of an app's data directory.
type filesView struct {
	// Path is where this is, with "" for the top.
	Path   string
	Crumbs []pathCrumb
	Rows   []fileRow
	More   bool
	// File is the file that this page is about, if it is about one.
	File *fileView
	// Top is the address of the top, and Download of the file.
	Top, Download string
}

type pathCrumb struct{ Name, Href string }

type fileRow struct {
	Name, Path, Href, Download string
	Dir, Link, Other           bool
	Size                       int64
	Modified                   time.Time
}

type fileView struct {
	Name     string
	Size     int64
	Modified time.Time
	// Preview is what is in it, if it is short and is text.
	Preview    string
	HasPreview bool
}

func filesHref(app, env, at string) string {
	if at == "" {
		return appAddress(app, env, "/files", nil)
	}
	return appAddress(app, env, "/files", url.Values{"path": {at}})
}

func filesDownload(app, env, at string) string {
	return appAddress(app, env, "/files/download", url.Values{"path": {at}})
}

func (c *Console) filesPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	c.showFiles(w, r, p, app, http.StatusOK, nil)
}

func (c *Console) showFiles(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, failed *failure) {
	d, err := c.appData(r, p, app, "apps", "Files of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !d.Access.Deploy {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "It takes being able to deploy to this app."})
		return
	}
	env := envOf(r)
	d.Crumbs = []crumb{{"Apps", "/"}, {app, appAddress(app, env, "", nil)}}
	d.Tab = "files"
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, env)}
	d.Failure = failed
	d.Notice = map[string]string{"uploaded": "The file is there.", "made": "The directory is there.", "removed": "It is gone."}[r.URL.Query().Get("done")]
	at := path.Clean("/" + r.PostFormValue("path"))[1:]
	if r.Method == http.MethodGet {
		at = path.Clean("/" + r.URL.Query().Get("path"))[1:]
	}
	view := &filesView{Path: at, Top: filesHref(app, env, "")}
	d.Files = view
	if c.deployer == nil {
		c.render(w, code, "app-files", d)
		return
	}
	if at != "" {
		walked := ""
		for _, name := range strings.Split(at, "/") {
			walked = path.Join(walked, name)
			view.Crumbs = append(view.Crumbs, pathCrumb{name, filesHref(app, env, walked)})
		}
	}
	missing := func(err error) bool {
		var problem *deploy.FileProblem
		if !errors.As(err, &problem) {
			c.internal(w, err)
			return true
		}
		if code == http.StatusOK {
			code = http.StatusNotFound
			if !problem.Missing {
				code = http.StatusUnprocessableEntity
			}
		}
		d.Failure = &failure{Message: problem.Msg}
		c.render(w, code, "app-files", d)
		return true
	}
	if at != "" {
		info, err := c.deployer.DataPathInfo(app, env, at)
		if err != nil {
			missing(err)
			return
		}
		if !info.Dir {
			f, opened, err := c.deployer.OpenDataFile(app, env, at)
			if err != nil {
				missing(err)
				return
			}
			defer f.Close()
			file := &fileView{Name: opened.Name(), Size: opened.Size(), Modified: opened.ModTime()}
			if opened.Size() <= maxFilePreview {
				if content, err := io.ReadAll(io.LimitReader(f, maxFilePreview)); err == nil && utf8.Valid(content) && !strings.ContainsRune(string(content), 0) {
					file.Preview, file.HasPreview = string(content), true
				}
			}
			view.File, view.Download = file, filesDownload(app, env, at)
			c.render(w, code, "app-files", d)
			return
		}
	}
	files, more, err := c.deployer.DataFiles(app, env, at)
	if err != nil {
		missing(err)
		return
	}
	view.More = more
	for _, f := range files {
		at := path.Join(at, f.Name)
		row := fileRow{Name: f.Name, Path: at, Dir: f.Dir, Link: f.Link, Other: f.Other, Size: f.Size, Modified: f.Modified, Href: filesHref(app, env, at)}
		if !f.Dir && !f.Link && !f.Other {
			row.Download = filesDownload(app, env, at)
		}
		view.Rows = append(view.Rows, row)
	}
	c.render(w, code, "app-files", d)
}

// downloadFile sends a file, as a download and nothing else: what an app wrote must not run with the console's address.
func (c *Console) downloadFile(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	access, err := c.auth.AppAccess(r.Context(), p, app)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !access.Deploy {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "It takes being able to deploy to this app."})
		return
	}
	env, at := envOf(r), r.URL.Query().Get("path")
	f, info, err := c.deployer.OpenDataFile(app, env, at)
	if err != nil {
		var problem *deploy.FileProblem
		if errors.As(err, &problem) {
			c.message(w, http.StatusNotFound, data{Title: "Not found", Heading: "That is not there", Message: problem.Msg})
			return
		}
		c.internal(w, err)
		return
	}
	defer f.Close()
	c.auth.Audit(r.Context(), store.AuditEntry{Actor: p.Name, IP: c.auth.ClientIP(r), Action: "file.download", Target: app + "/" + env + ":" + at, Result: "ok"})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func writeJSONTo(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (c *Console) fileActionFailed(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, err error) {
	var problem *deploy.FileProblem
	if errors.As(err, &problem) {
		c.showFiles(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: problem.Msg})
		return
	}
	c.internal(w, err)
}

func (c *Console) uploadFile(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	file, header, err := r.FormFile("file")
	if err != nil {
		c.showFiles(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: "Choose a file to put here."})
		return
	}
	defer file.Close()
	defer r.MultipartForm.RemoveAll()
	dir, env := r.PostFormValue("path"), envOf(r)
	if _, err := c.deployer.PutDataFile(app, env, path.Join(dir, path.Base(header.Filename)), file, r.PostFormValue("replace") != ""); err != nil {
		c.fileActionFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, appAddress(app, env, "/files", url.Values{"path": {dir}, "done": {"uploaded"}}), http.StatusSeeOther)
}

func (c *Console) makeDirectory(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	dir, env, name := r.PostFormValue("path"), envOf(r), strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || strings.ContainsAny(name, `/\`) {
		c.showFiles(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: "A directory's name has no slashes in it."})
		return
	}
	if err := c.deployer.MakeDataDir(app, env, path.Join(dir, name)); err != nil {
		c.fileActionFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, appAddress(app, env, "/files", url.Values{"path": {dir}, "done": {"made"}}), http.StatusSeeOther)
}

func (c *Console) removeFile(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	at, env := r.PostFormValue("path"), envOf(r)
	if err := c.deployer.RemoveData(app, env, at, r.PostFormValue("everything") != ""); err != nil {
		c.fileActionFailed(w, r, p, app, err)
		return
	}
	http.Redirect(w, r, appAddress(app, env, "/files", url.Values{"path": {path.Dir(path.Clean("/" + at))[1:]}, "done": {"removed"}}), http.StatusSeeOther)
}

// lineConsoleView is the page for running commands one at a time.
type lineConsoleView struct {
	Recent []runRow
	// History is what has been run here, newest first, to go back through.
	History []string
}

func (c *Console) consolePage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	c.showConsole(w, r, p, app, http.StatusOK, nil)
}

func (c *Console) showConsole(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, failed *failure) {
	d, err := c.appData(r, p, app, "apps", "Console of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	if !d.Access.Deploy {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "It takes being able to deploy to this app."})
		return
	}
	env := envOf(r)
	d.Crumbs = []crumb{{"Apps", "/"}, {app, appAddress(app, env, "", nil)}}
	d.Tab = "console"
	d.App = appView{Name: app, URL: c.cfg.EnvURL(app, env)}
	d.Failure = failed
	view := &lineConsoleView{}
	runs, err := c.db.Runs(r.Context(), app, env, "", 100)
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, run := range runs {
		if run.Job != "" {
			continue
		}
		if len(view.Recent) < runsShown {
			view.Recent = append(view.Recent, runRowOf(run))
		}
		view.History = append(view.History, run.Command)
	}
	d.LineConsole = view
	c.render(w, code, "app-console", d)
}

// consoleRun starts a command. A page with its script on follows it where it is. One without goes to the run's own page.
func (c *Console) consoleRun(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	env := envOf(r)
	run, err := c.deployer.StartRun(lasting(r), deploy.RunRequest{App: app, Env: env, By: p.Name, Command: r.PostFormValue("command"), Trigger: "console"})
	fetched := strings.Contains(r.Header.Get("Accept"), "application/json")
	if err != nil {
		var refused *deploy.NotRunnableError
		switch {
		case errors.As(err, &refused) && fetched:
			writeJSONTo(w, http.StatusConflict, map[string]string{"error": sentence(refused.Msg)})
		case errors.As(err, &refused):
			c.showConsole(w, r, p, app, http.StatusConflict, &failure{Message: sentence(refused.Msg)})
		default:
			c.internal(w, err)
		}
		return
	}
	address := "/apps/" + url.PathEscape(app) + "/runs/" + url.PathEscape(run.ID)
	if fetched {
		base := "/api/v1/apps/" + url.PathEscape(app) + "/runs/" + url.PathEscape(run.ID)
		writeJSONTo(w, http.StatusAccepted, map[string]string{"id": run.ID, "page": address, "log": base + "/log?follow=true", "status": base, "stop": address + "/stop"})
		return
	}
	http.Redirect(w, r, address, http.StatusSeeOther)
}
