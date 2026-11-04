package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/dotenv"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// secretMask stands for a secret's value in the text editor: secrets cannot be read back.
const secretMask = "••••••••"

// varsView is the variables page.
type varsView struct {
	Vars []apitypes.Var
	// HasRelease says that changes can be released; Unreleased lists those that have not.
	HasRelease bool
	Unreleased []apitypes.VarDiff
	// Editor holds the text being edited, and Preview what it would change.
	Editor  string
	Preview *varsPreview
	// SecretNew and BuildNew say how to treat the variables the text adds.
	SecretNew, BuildNew bool
	// Released is the deployment that released a change.
	Released string
	// BuildChanged says that a variable builds use was changed.
	BuildChanged bool
}

// varsPreview says what a text would change.
type varsPreview struct {
	Rows                []varChangeRow
	Unchanged           int
	Added, Changed, Off int
}

type varChangeRow struct {
	Name, Change  string
	Secret        bool
	Before, After string
}

// Empty says that it would change nothing.
func (p varsPreview) Empty() bool { return len(p.Rows) == 0 }

func (c *Console) showVars(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, failed *failure) {
	d, _, err := c.shellData(r, p, "apps", "Variables of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}, {app, "/apps/" + url.PathEscape(app)}}
	d.Tab = "variables"
	d.App = appView{Name: app, URL: c.appURL(app)}
	view, err := c.varsView(r.Context(), app)
	if err != nil {
		c.internal(w, err)
		return
	}
	query := r.URL.Query()
	view.Released = query.Get("released")
	view.BuildChanged = query.Get("build") == "1"
	d.Variables = view
	d.Notice = varNotices[query.Get("done")]
	d.Failure = failed
	c.render(w, code, "variables", d)
}

var varNotices = map[string]string{
	"released": "The change is released: it runs now.",
	"saved":    "The change is saved. The next deployment or release will run with it.",
	"none":     "Nothing changed.",
}

func (c *Console) varsView(ctx context.Context, app string) (varsView, error) {
	var view varsView
	var err error
	if view.Vars, err = deploy.ListVars(ctx, c.db, app, names.Production); err != nil {
		return view, err
	}
	_, _, err = c.db.CurrentRelease(ctx, app, names.Production)
	switch {
	case err == nil:
		view.HasRelease = true
	case !errors.Is(err, store.ErrNotFound):
		return view, err
	}
	if view.Unreleased, err = deploy.UnreleasedVars(ctx, c.db, app, names.Production); err != nil {
		return view, err
	}
	return view, nil
}

func (c *Console) variablesPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	if !p.Can(auth.ScopeDeploy) {
		c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not see that", Message: "Your account cannot see variables.", Link: "/", LinkText: "Back to the start"})
		return
	}
	c.showVars(w, r, p, app, http.StatusOK, nil)
}

// configure applies a change of variables and goes back to the page, saying what it did.
func (c *Console) configure(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, ch deploy.ConfigChange) {
	ch.By = p.Name
	res, err := c.deployer.Configure(lasting(r), app, names.Production, ch)
	var invalid *deploy.InvalidConfigError
	var rollout *reconcile.RolloutError
	switch {
	case errors.As(err, &invalid):
		c.showVars(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(err.Error())})
		return
	case errors.As(err, &rollout):
		c.showVars(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(rollout.Message) + " The change is saved, and the previous release keeps serving.", Output: rollout.Output})
		return
	case err != nil:
		c.internal(w, err)
		return
	}
	next := url.Values{}
	if res.Release != nil {
		next.Set("done", "released")
		next.Set("released", res.Deployment.ID)
	} else {
		next.Set("done", "saved")
	}
	for _, v := range ch.Set {
		if v.Build {
			next.Set("build", "1")
		}
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/variables?"+next.Encode(), http.StatusSeeOther)
}

func checked(r *http.Request, name string) bool { return r.PostFormValue(name) != "" }

func (c *Console) setVariable(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := r.PostFormValue("value")
	secret := checked(r, "secret")
	ch := deploy.ConfigChange{AllEnvs: checked(r, "all_envs"), Restart: !checked(r, "hold")}
	// A secret left empty on the page keeps its value: it could not be shown to begin with.
	if value == "" && checked(r, "was_secret") {
		c.showVars(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: fmt.Sprintf("Give %s a new value: a secret's cannot be read back to keep.", name)})
		return
	}
	ch.Set = []deploy.VarValue{{Name: name, Value: value, Secret: secret, Build: checked(r, "build")}}
	c.configure(w, r, p, app, ch)
}

func (c *Console) unsetVariable(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	c.configure(w, r, p, app, deploy.ConfigChange{
		Unset: []string{r.PostFormValue("name")}, AllEnvs: checked(r, "all_envs"), Restart: !checked(r, "hold"),
	})
}

func (c *Console) releaseVariables(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	_, dep, err := c.deployer.ReleaseVars(lasting(r), app, names.Production, p.Name)
	if errors.Is(err, store.ErrNotFound) {
		c.showVars(w, r, p, app, http.StatusConflict, &failure{Message: "There is nothing to release them with: nothing is deployed yet."})
		return
	}
	var rollout *reconcile.RolloutError
	if errors.As(err, &rollout) {
		c.showVars(w, r, p, app, http.StatusUnprocessableEntity, &failure{Message: sentence(rollout.Message) + " The previous release keeps serving.", Output: rollout.Output})
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/variables?"+url.Values{"done": {"released"}, "released": {dep.ID}}.Encode(), http.StatusSeeOther)
}

// editorText writes an environment's own variables as the editor holds them.
func editorText(vars []store.Var) string {
	var b strings.Builder
	b.WriteString("# This environment's variables, one NAME=value on a line. A line taken out removes\n")
	b.WriteString("# the variable. A secret shows as " + secretMask + ": leave that as it is to keep its value.\n")
	var shared []string
	for _, v := range vars {
		switch {
		case v.AllEnvs:
			shared = append(shared, v.Name)
		case v.Secret:
			b.WriteString(v.Name + "=" + secretMask + "\n")
		default:
			b.WriteString(v.Name + "=" + dotenv.Quote(v.Value) + "\n")
		}
	}
	if len(shared) > 0 {
		b.WriteString("# Set for every environment, and not changed here: " + strings.Join(shared, ", ") + "\n")
	}
	return b.String()
}

// planEdit works out what a text would change: the change itself, and how to say so.
func planEdit(text string, current []store.Var, secretNew, buildNew bool) (deploy.ConfigChange, varsPreview, error) {
	var ch deploy.ConfigChange
	var preview varsPreview
	lines, err := dotenv.Parse(strings.NewReader(text))
	if err != nil {
		return ch, preview, err
	}
	had := map[string]store.Var{}
	for _, v := range current {
		if !v.AllEnvs {
			had[v.Name] = v
		}
	}
	for _, line := range lines {
		old, exists := had[line.Name]
		delete(had, line.Name)
		switch {
		case !exists && line.Value == secretMask:
			return ch, preview, fmt.Errorf("%s has no value to keep: give it one", line.Name)
		case exists && old.Secret && line.Value == secretMask:
			preview.Unchanged++
		case exists && !old.Secret && old.Value == line.Value:
			preview.Unchanged++
		case exists:
			ch.Set = append(ch.Set, deploy.VarValue{Name: line.Name, Value: line.Value, Secret: old.Secret, Build: old.Build})
			row := varChangeRow{Name: line.Name, Change: "changed", Secret: old.Secret}
			if !old.Secret {
				row.Before, row.After = old.Value, line.Value
			}
			preview.Rows = append(preview.Rows, row)
			preview.Changed++
		default:
			ch.Set = append(ch.Set, deploy.VarValue{Name: line.Name, Value: line.Value, Secret: secretNew, Build: buildNew})
			row := varChangeRow{Name: line.Name, Change: "added", Secret: secretNew}
			if !secretNew {
				row.After = line.Value
			}
			preview.Rows = append(preview.Rows, row)
			preview.Added++
		}
	}
	for _, v := range current {
		if _, gone := had[v.Name]; gone && !v.AllEnvs {
			ch.Unset = append(ch.Unset, v.Name)
			row := varChangeRow{Name: v.Name, Change: "removed", Secret: v.Secret}
			if !v.Secret {
				row.Before = v.Value
			}
			preview.Rows = append(preview.Rows, row)
			preview.Off++
		}
	}
	return ch, preview, nil
}

func (c *Console) editVariablesPage(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	app, ok := c.lookupApp(w, r, p)
	if !ok {
		return
	}
	current, err := c.db.Vars(r.Context(), app, names.Production)
	if err != nil {
		c.internal(w, err)
		return
	}
	c.showEditor(w, r, p, app, http.StatusOK, varsView{Editor: editorText(current)}, nil)
}

func (c *Console) showEditor(w http.ResponseWriter, r *http.Request, p auth.Principal, app string, code int, view varsView, failed *failure) {
	d, _, err := c.shellData(r, p, "apps", "Edit the variables of "+app)
	if err != nil {
		c.internal(w, err)
		return
	}
	d.Crumbs = []crumb{{"Apps", "/"}, {app, "/apps/" + url.PathEscape(app)}, {"Variables", "/apps/" + url.PathEscape(app) + "/variables"}}
	d.App = appView{Name: app, URL: c.appURL(app)}
	if _, _, err := c.db.CurrentRelease(r.Context(), app, names.Production); err == nil {
		view.HasRelease = true
	}
	d.Variables = view
	d.Failure = failed
	c.render(w, code, "variables-edit", d)
}

// previewVariables shows what a text would change, and asks.
func (c *Console) previewVariables(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	text := r.PostFormValue("text")
	view := varsView{Editor: text, SecretNew: checked(r, "secret_new"), BuildNew: checked(r, "build_new")}
	current, err := c.db.Vars(r.Context(), app, names.Production)
	if err != nil {
		c.internal(w, err)
		return
	}
	_, preview, err := planEdit(text, current, view.SecretNew, view.BuildNew)
	if err != nil {
		c.showEditor(w, r, p, app, http.StatusUnprocessableEntity, view, &failure{Message: sentence(err.Error())})
		return
	}
	view.Preview = &preview
	c.showEditor(w, r, p, app, http.StatusOK, view, nil)
}

// applyVariables makes the change a text describes.
func (c *Console) applyVariables(w http.ResponseWriter, r *http.Request, p auth.Principal, app string) {
	text := r.PostFormValue("text")
	view := varsView{Editor: text, SecretNew: checked(r, "secret_new"), BuildNew: checked(r, "build_new")}
	current, err := c.db.Vars(r.Context(), app, names.Production)
	if err != nil {
		c.internal(w, err)
		return
	}
	ch, _, err := planEdit(text, current, view.SecretNew, view.BuildNew)
	if err != nil {
		c.showEditor(w, r, p, app, http.StatusUnprocessableEntity, view, &failure{Message: sentence(err.Error())})
		return
	}
	if len(ch.Set) == 0 && len(ch.Unset) == 0 {
		http.Redirect(w, r, "/apps/"+url.PathEscape(app)+"/variables?done=none", http.StatusSeeOther)
		return
	}
	ch.Restart = !checked(r, "hold")
	c.configure(w, r, p, app, ch)
}
