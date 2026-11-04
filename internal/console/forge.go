package console

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/forge"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// forgeView is the Repository section of an app's settings.
type forgeView struct {
	Connected bool
	Conn      store.ForgeConnection
	HookURL   string
	HasToken  bool
	Pulls     []pullView
	// A connection just made, with what signs what it sends, shown this once.
	NewSecret string
	// What someone had typed, when it was not right.
	Form forge.Connection
}

type pullView struct {
	store.ForgePull
	Short      string
	Status     status
	PreviewURL string
	// Approvable says that a button can do something: it is waiting, or could be tried again.
	Approvable bool
	Action     string
}

func (c *Console) hookURL(hook string) string {
	return c.cfg.PublicURL(c.cfg.ConsoleHost) + "/api/v1/hooks/" + hook
}

func (c *Console) forgeData(r *http.Request, app string, d *data) error {
	if c.forges == nil {
		return nil
	}
	view := &forgeView{}
	d.Forge = view
	conn, err := c.db.ForgeOf(r.Context(), app)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	view.Connected, view.Conn, view.HookURL, view.HasToken = true, conn, c.hookURL(conn.HookID), len(conn.SealedToken) > 0
	pulls, err := c.db.ForgePulls(r.Context(), conn.AppID)
	if err != nil {
		return err
	}
	for _, p := range pulls {
		row := pullView{ForgePull: p, Short: p.HeadSHA}
		if len(row.Short) > 8 {
			row.Short = row.Short[:8]
		}
		switch p.State {
		case store.PullWaiting:
			row.Status, row.Approvable, row.Action = status{"Waiting for approval", "warn"}, true, "Approve and build"
		case store.PullBuilding:
			row.Status = status{"Building", "info pulse"}
		case store.PullLive:
			row.Status, row.PreviewURL = status{"Live", "ok"}, c.cfg.EnvURL(app, p.Env)
		case store.PullFailed:
			row.Status, row.Approvable, row.Action = status{"Failed", "fail"}, true, "Build it again"
		default:
			row.Status = status{"No preview", ""}
		}
		view.Pulls = append(view.Pulls, row)
	}
	return nil
}

func (c *Console) forgeFailed(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, form forge.Connection, err error) {
	var invalid *forge.InvalidError
	switch {
	case errors.As(err, &invalid):
		c.showForge(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: invalid.Msg}, form, "")
	case errors.Is(err, forge.ErrNoConnection), errors.Is(err, store.ErrNotFound):
		c.showForge(w, r, p, app, http.StatusNotFound, &failure{Message: "That is not there any more."}, form, "")
	default:
		c.internal(w, err)
	}
}

func (c *Console) showForge(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, failed *failure, form forge.Connection, secret string) {
	c.showAppSettingsWith(w, r, p, app, code, failed, func(d *data) {
		if d.Forge != nil {
			d.Forge.Form, d.Forge.NewSecret = form, secret
		}
	})
}

func (c *Console) connectForge(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	form := forge.Connection{
		URL: r.PostFormValue("url"), Kind: r.PostFormValue("kind"), Token: r.PostFormValue("token"), DefaultBranch: r.PostFormValue("branch"),
		NoPreviews: r.PostFormValue("previews") == "", Poll: r.PostFormValue("poll") != "",
	}
	appID, err := c.db.AppID(r.Context(), app)
	if err != nil {
		c.internal(w, err)
		return
	}
	_, secret, err := c.forges.Connect(r.Context(), appID, app, form, p.Name)
	if err != nil {
		form.Token = ""
		c.forgeFailed(w, r, p, app, form, err)
		return
	}
	c.showForge(w, r, p, app, http.StatusCreated, nil, forge.Connection{}, secret)
}

func (c *Console) disconnectForge(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	if err := c.forges.Disconnect(r.Context(), app); err != nil {
		c.forgeFailed(w, r, p, app, forge.Connection{}, err)
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/settings?done=disconnected#repository", http.StatusSeeOther)
}

func (c *Console) syncForge(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	if err := c.forges.Sync(r.Context(), app); err != nil {
		c.forgeFailed(w, r, p, app, forge.Connection{}, err)
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/settings?done=looking#repository", http.StatusSeeOther)
}

func (c *Console) approveForgePull(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := c.forges.Approve(r.Context(), app, number, p.Name); err != nil {
		c.forgeFailed(w, r, p, app, forge.Connection{}, err)
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/settings?done=approved#repository", http.StatusSeeOther)
}
