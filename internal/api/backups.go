package api

import (
	"errors"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func backupDoc(b store.Backup) apitypes.Backup {
	doc := apitypes.Backup{ID: b.ID, StartedAt: b.StartedAt, Status: b.Status, Trigger: b.Trigger, CreatedBy: b.CreatedBy, Destination: b.Destination, Name: b.Name, Size: b.SizeBytes, Error: b.Error}
	if !b.FinishedAt.IsZero() {
		at := b.FinishedAt
		doc.FinishedAt = &at
	}
	return doc
}

func (s *Server) backups(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.BackupConfig(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	list, err := s.db.Backups(r.Context(), 50)
	if err != nil {
		s.internal(w, err)
		return
	}
	doc := apitypes.Backups{Destination: cfg.Destination, Endpoint: cfg.Endpoint, Region: cfg.Region, AccessKey: cfg.AccessKey, HasSecretKey: len(cfg.SealedSecret) > 0, Schedule: cfg.Schedule, Keep: cfg.Keep, Items: []apitypes.Backup{}}
	if doc.Destination == "" {
		doc.Destination = s.backupsService.DefaultDestination()
	}
	if next, err := s.backupsService.Next(r.Context()); err == nil && !next.IsZero() {
		doc.NextAt = &next
	}
	for _, b := range list {
		doc.Items = append(doc.Items, backupDoc(b))
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) backupFailed(w http.ResponseWriter, err error) bool {
	var refused *backup.RefusedError
	switch {
	case err == nil:
		return false
	case errors.As(err, &refused):
		problem(w, http.StatusUnprocessableEntity, "invalid_backup", refused.Msg)
	case errors.Is(err, backup.ErrAlreadyRunning):
		problem(w, http.StatusConflict, "backup_running", sentenceOf(err))
	default:
		s.internal(w, err)
	}
	return true
}

func (s *Server) startBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.backupsService.Start(r.Context(), "manual", who(r).Name)
	if s.backupFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, backupDoc(b))
}

func (s *Server) configureBackups(w http.ResponseWriter, r *http.Request) {
	var req apitypes.BackupConfig
	if !decode(w, r, &req) {
		return
	}
	err := s.backupsService.Configure(r.Context(), backup.Form{Destination: req.Destination, Endpoint: req.Endpoint, Region: req.Region, AccessKey: req.AccessKey, SecretKey: req.SecretKey, Schedule: req.Schedule, Keep: req.Keep}, who(r).Name)
	if s.backupFailed(w, err) {
		return
	}
	s.backups(w, r)
}
