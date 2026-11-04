package api

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"strconv"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (s *Server) fileFailed(w http.ResponseWriter, err error) bool {
	var problemWith *deploy.FileProblem
	switch {
	case err == nil:
		return false
	case errors.As(err, &problemWith) && problemWith.Missing:
		problem(w, http.StatusNotFound, "not_found", problemWith.Msg)
	case errors.As(err, &problemWith) && problemWith.Conflict:
		problem(w, http.StatusConflict, "conflict", problemWith.Msg)
	case errors.As(err, &problemWith):
		problem(w, http.StatusUnprocessableEntity, "invalid_path", problemWith.Msg)
	default:
		s.internal(w, err)
	}
	return true
}

func dataFileDoc(f deploy.DataFile) apitypes.DataFile {
	doc := apitypes.DataFile{Name: f.Name, Kind: "file", Link: f.Link, Size: f.Size, Modified: f.Modified}
	switch {
	case f.Dir:
		doc.Kind = "directory"
	case f.Link:
		doc.Kind = "link"
	case f.Other:
		doc.Kind = "other"
	}
	return doc
}

func (s *Server) dataFiles(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	if _, err := s.db.Environment(r.Context(), app, env); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			problem(w, http.StatusNotFound, "not_found", "There is no environment "+env+" of "+app+".")
			return
		}
		s.internal(w, err)
		return
	}
	at := r.URL.Query().Get("path")
	files, more, err := s.deployer.DataFiles(app, env, at)
	if s.fileFailed(w, err) {
		return
	}
	doc := apitypes.DataListing{Path: path.Clean("/" + at), Files: make([]apitypes.DataFile, 0, len(files)), More: more}
	for _, f := range files {
		doc.Files = append(doc.Files, dataFileDoc(f))
	}
	writeJSON(w, http.StatusOK, doc)
}

// dataFileContent sends a file. It is never shown as a page: what an app wrote must not run with the console's address.
func (s *Server) dataFileContent(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	at := r.URL.Query().Get("path")
	f, info, err := s.deployer.OpenDataFile(app, env, at)
	if s.fileFailed(w, err) {
		return
	}
	defer f.Close()
	s.auth.Audit(r.Context(), store.AuditEntry{Actor: who(r).Name, IP: s.auth.ClientIP(r), Action: "file.download", Target: app + "/" + env + ":" + at, Result: "ok"})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()}))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (s *Server) putDataFile(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	// What would not fit is told so as it arrives, or before.
	if r.ContentLength > deploy.MaxDataFile {
		problem(w, http.StatusRequestEntityTooLarge, "too_large", "That is more than can be put here this way.")
		return
	}
	f, err := s.deployer.PutDataFile(app, env, q.Get("path"), r.Body, q.Get("replace") == "true")
	if s.fileFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, dataFileDoc(f))
}

func (s *Server) makeDataDir(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	if s.fileFailed(w, s.deployer.MakeDataDir(app, env, r.URL.Query().Get("path"))) {
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) removeData(w http.ResponseWriter, r *http.Request) {
	app, env, ok := s.target(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if s.fileFailed(w, s.deployer.RemoveData(app, env, q.Get("path"), q.Get("recursive") == "true")) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
