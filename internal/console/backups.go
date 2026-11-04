package console

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// backupsView is the page of backups.
type backupsView struct {
	Items []backupRow
	// What is set.
	Kind, Path, Bucket, Endpoint, Region, AccessKey, Schedule string
	HasSecret                                                 bool
	Keep                                                      int
	Default                                                   string
	Next                                                      time.Time
	Going                                                     bool
	KeyPath                                                   string
}

type backupRow struct {
	store.Backup
	Size string
	Took string
}

func (c *Console) showBackups(w http.ResponseWriter, r *http.Request, code int, form *backup.Form, failed *failure) {
	_, d, ok := c.adminData(w, r, "backups", "Backups")
	if !ok {
		return
	}
	cfg, err := c.db.BackupConfig(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	view := backupsView{Kind: "path", Path: cfg.Destination, Endpoint: cfg.Endpoint, Region: cfg.Region, AccessKey: cfg.AccessKey, Schedule: cfg.Schedule, HasSecret: len(cfg.SealedSecret) > 0, Keep: cfg.Keep, Default: c.backups.DefaultDestination(), KeyPath: c.cfg.DataDir + "/keys/master.key"}
	if form != nil {
		view.Path, view.Endpoint, view.Region, view.AccessKey, view.Schedule, view.Keep = form.Destination, form.Endpoint, form.Region, form.AccessKey, form.Schedule, form.Keep
	}
	if strings.HasPrefix(view.Path, "s3://") {
		view.Kind, view.Bucket, view.Path = "s3", view.Path, ""
	}
	view.Next, _ = c.backups.Next(r.Context())
	list, err := c.db.Backups(r.Context(), 30)
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, b := range list {
		row := backupRow{Backup: b}
		if b.SizeBytes > 0 {
			row.Size = humanSizeOf(b.SizeBytes)
		}
		if !b.FinishedAt.IsZero() {
			row.Took = tookText(b.FinishedAt.Sub(b.StartedAt))
		}
		if b.Status == store.BackupRunning {
			view.Going = true
		}
		view.Items = append(view.Items, row)
	}
	d.Backups = view
	d.Poll = view.Going
	d.Failure = failed
	c.render(w, code, "settings-backups", d)
}

func (c *Console) backupsPage(w http.ResponseWriter, r *http.Request) {
	c.showBackups(w, r, http.StatusOK, nil, nil)
}

func (c *Console) startBackup(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	_, err := c.backups.Start(r.Context(), "manual", p.Name)
	var refused *backup.RefusedError
	switch {
	case errors.Is(err, backup.ErrAlreadyRunning):
		c.showBackups(w, r, http.StatusConflict, nil, &failure{Message: "A backup is already going on."})
	case errors.As(err, &refused):
		c.showBackups(w, r, http.StatusUnprocessableEntity, nil, &failure{Message: sentence(refused.Msg)})
	case err != nil:
		c.internal(w, err)
	default:
		http.Redirect(w, r, "/settings/backups?done=backup", http.StatusSeeOther)
	}
}

func (c *Console) configureBackups(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	keep, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("keep")))
	if err != nil {
		c.showBackups(w, r, http.StatusUnprocessableEntity, nil, &failure{Message: "How many to keep is a number."})
		return
	}
	form := backup.Form{Schedule: r.PostFormValue("schedule"), Keep: keep}
	if r.PostFormValue("kind") == "s3" {
		form.Destination = strings.TrimSpace(r.PostFormValue("bucket"))
		form.Endpoint, form.Region, form.AccessKey, form.SecretKey = r.PostFormValue("endpoint"), r.PostFormValue("region"), r.PostFormValue("access_key"), r.PostFormValue("secret_key")
		if !strings.HasPrefix(form.Destination, "s3://") {
			c.showBackups(w, r, http.StatusUnprocessableEntity, &form, &failure{Message: "A bucket is named as s3://bucket/prefix."})
			return
		}
	} else {
		form.Destination = r.PostFormValue("path")
	}
	err = c.backups.Configure(r.Context(), form, p.Name)
	var refused *backup.RefusedError
	switch {
	case errors.As(err, &refused):
		c.showBackups(w, r, http.StatusUnprocessableEntity, &form, &failure{Message: sentence(refused.Msg)})
	case err != nil:
		c.internal(w, err)
	default:
		http.Redirect(w, r, "/settings/backups?done=backups-set", http.StatusSeeOther)
	}
}
