package console

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// notificationsView is the pages of notifications.
type notificationsView struct {
	// What someone has been told.
	Items  []inboxItem
	Unread int
	// The channels and rules.
	Channels       []channelRow
	Rules          []ruleRow
	Events         []notify.RuleEvent
	Kinds          []channelKind
	Projects, Apps []string
	// A channel just made, with what signs its messages, shown this once.
	NewName, NewSecret string
	// What the form said, when it was refused.
	Form channelForm
}

type inboxItem struct {
	store.Notification
	Read         bool
	Level, Where string
}

type channelRow struct {
	store.NotificationChannel
	Target, KindLabel string
}

type channelKind struct{ Value, Label string }

// channelKinds are the kinds of channel, as the page calls them.
var channelKinds = []channelKind{
	{notify.KindWebhook, "Webhook, signed"},
	{notify.KindSlack, "Slack"},
	{notify.KindTeams, "Microsoft Teams"},
	{notify.KindDiscord, "Discord"},
	{notify.KindEmail, "Email"},
}

func kindLabel(kind string) string {
	for _, k := range channelKinds {
		if k.Value == kind {
			return k.Label
		}
	}
	return kind
}

type channelForm struct{ Name, Kind, Host, Port, Username, From, To string }

type ruleRow struct {
	ID, Scope, Channel string
	Events             []string
}

func ruleRowOf(r store.NotificationRule) ruleRow {
	row := ruleRow{ID: r.ID, Channel: r.ChannelName, Scope: "The whole server"}
	switch {
	case r.App != "":
		row.Scope = "The app " + r.App
	case r.Project != "":
		row.Scope = "All of " + r.Project
	}
	for _, e := range r.Events {
		row.Events = append(row.Events, notify.EventLabel(e))
	}
	return row
}

// inboxOf is what someone has been told, of what they can still see.
func (c *Console) inboxOf(r *http.Request, p auth.Principal, visible func(string) bool) ([]inboxItem, int, error) {
	if p.Local() {
		return nil, 0, nil
	}
	list, err := c.db.Notifications(r.Context(), p.UserID, 100)
	if err != nil {
		return nil, 0, err
	}
	items, unread := make([]inboxItem, 0, len(list)), 0
	for _, n := range list {
		if n.App != "" && !visible(n.App) {
			continue
		}
		item := inboxItem{Notification: n, Read: !n.ReadAt.IsZero(), Level: notify.Level(n.Kind)}
		if n.App != "" {
			item.Where = appAddress(n.App, n.Env, "", nil)
		}
		if !item.Read {
			unread++
		}
		items = append(items, item)
	}
	return items, unread, nil
}

func (c *Console) unread(r *http.Request, p auth.Principal, visible func(string) bool) (int, error) {
	_, n, err := c.inboxOf(r, p, visible)
	return n, err
}

func (c *Console) inbox(w http.ResponseWriter, r *http.Request) {
	p, ok := c.requireUser(w, r)
	if !ok {
		return
	}
	d, _, err := c.shellData(r, p, "notifications", "Notifications")
	if err != nil {
		c.internal(w, err)
		return
	}
	visible, err := c.auth.Visible(r.Context(), p)
	if err != nil {
		c.internal(w, err)
		return
	}
	if d.Notifications.Items, d.Notifications.Unread, err = c.inboxOf(r, p, visible); err != nil {
		c.internal(w, err)
		return
	}
	c.render(w, http.StatusOK, "notifications", d)
}

func (c *Console) readInbox(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !p.Local() {
		if err := c.db.ReadNotifications(r.Context(), p.UserID, time.Now().UTC()); err != nil {
			c.internal(w, err)
			return
		}
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

func (c *Console) showNotificationSettings(w http.ResponseWriter, r *http.Request, code int, view notificationsView, failed *failure) {
	_, d, ok := c.adminData(w, r, "notifications", "Notifications")
	if !ok {
		return
	}
	channels, err := c.db.NotificationChannels(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, ch := range channels {
		view.Channels = append(view.Channels, channelRow{NotificationChannel: ch, Target: c.notifier.Target(ch), KindLabel: kindLabel(ch.Kind)})
	}
	rules, err := c.db.NotificationRules(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, rule := range rules {
		view.Rules = append(view.Rules, ruleRowOf(rule))
	}
	projects, err := c.db.Projects(r.Context())
	if err != nil {
		c.internal(w, err)
		return
	}
	for _, project := range projects {
		view.Projects = append(view.Projects, project.Name)
	}
	view.Apps, view.Events, view.Kinds = d.AppNames, notify.RuleEvents, channelKinds
	d.Notifications, d.Failure = view, failed
	c.render(w, code, "settings-notifications", d)
}

func (c *Console) notificationSettings(w http.ResponseWriter, r *http.Request) {
	c.showNotificationSettings(w, r, http.StatusOK, notificationsView{}, nil)
}

func (c *Console) addChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	form := channelForm{
		Name: strings.TrimSpace(r.PostFormValue("name")), Kind: r.PostFormValue("kind"), Host: strings.TrimSpace(r.PostFormValue("host")),
		Port: strings.TrimSpace(r.PostFormValue("port")), Username: strings.TrimSpace(r.PostFormValue("username")),
		From: strings.TrimSpace(r.PostFormValue("from")), To: strings.TrimSpace(r.PostFormValue("to")),
	}
	refuse := func(code int, message string) {
		c.showNotificationSettings(w, r, code, notificationsView{Form: form}, &failure{Message: message})
	}
	port := 0
	if form.Port != "" {
		n, err := strconv.Atoi(form.Port)
		if err != nil || n < 1 || n > 65535 {
			refuse(http.StatusUnprocessableEntity, "A port is a number up to 65535.")
			return
		}
		port = n
	}
	var to []string
	for _, address := range strings.FieldsFunc(form.To, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		to = append(to, address)
	}
	cfg := notify.Config{URL: strings.TrimSpace(r.PostFormValue("url")), Host: form.Host, Port: port, Username: form.Username, Password: r.PostFormValue("password"), From: form.From, To: to}
	channel, secret, err := c.notifier.AddChannel(r.Context(), form.Name, form.Kind, cfg, p.Name)
	var invalid *notify.InvalidError
	switch {
	case errors.As(err, &invalid):
		refuse(http.StatusUnprocessableEntity, invalid.Msg)
		return
	case errors.Is(err, store.ErrChannelTaken):
		refuse(http.StatusConflict, "There is already a channel of that name.")
		return
	case err != nil:
		c.internal(w, err)
		return
	}
	c.log.Info("added a notification channel", "channel", channel.Name, "kind", channel.Kind, "by", p.Name)
	c.showNotificationSettings(w, r, http.StatusCreated, notificationsView{NewName: channel.Name, NewSecret: secret}, nil)
}

func (c *Console) testChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	_, err := c.notifier.Test(r.Context(), r.PostFormValue("channel"), p.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.showNotificationSettings(w, r, http.StatusNotFound, notificationsView{}, &failure{Message: "There is no such channel."})
	case err != nil:
		c.showNotificationSettings(w, r, http.StatusBadGateway, notificationsView{}, &failure{Message: "It did not go: " + sentence(err.Error())})
	default:
		http.Redirect(w, r, "/settings/notifications?done=tested", http.StatusSeeOther)
	}
}

func (c *Console) removeChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	id := r.PostFormValue("channel")
	if err := c.db.DeleteNotificationChannel(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	c.log.Info("removed a notification channel", "channel", id, "by", p.Name)
	http.Redirect(w, r, "/settings/notifications?done=channel-removed", http.StatusSeeOther)
}

func (c *Console) addServerRule(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	project, app := "", ""
	if scope := r.PostFormValue("scope"); scope != "" {
		kind, name, _ := strings.Cut(scope, ":")
		switch kind {
		case "project":
			project = name
		case "app":
			app = name
		}
	}
	_, err := c.notifier.AddRule(r.Context(), r.PostFormValue("channel"), project, app, r.PostForm["events"], p.Name)
	if refused := ruleFailure(err); refused != nil {
		c.showNotificationSettings(w, r, http.StatusUnprocessableEntity, notificationsView{}, refused)
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/settings/notifications?done=rule", http.StatusSeeOther)
}

// ruleFailure says what is wrong with a rule, if that is what went wrong.
func ruleFailure(err error) *failure {
	var invalid *notify.InvalidError
	switch {
	case errors.As(err, &invalid):
		return &failure{Message: invalid.Msg}
	case errors.Is(err, store.ErrNotFound):
		return &failure{Message: "There is no such channel, project or app."}
	}
	return nil
}

func (c *Console) removeServerRule(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if err := c.db.DeleteNotificationRule(r.Context(), r.PostFormValue("rule")); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, "/settings/notifications?done=rule-removed", http.StatusSeeOther)
}

// projectRules are a project's rules, and the channels a new one can use.
func (c *Console) projectRules(r *http.Request, project string, apps []string) ([]ruleRow, []string, error) {
	rules, err := c.db.NotificationRules(r.Context())
	if err != nil {
		return nil, nil, err
	}
	var rows []ruleRow
	for _, rule := range rules {
		if ruleOfProject(rule, project, apps) {
			rows = append(rows, ruleRowOf(rule))
		}
	}
	channels, err := c.db.NotificationChannels(r.Context())
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(channels))
	for _, ch := range channels {
		names = append(names, ch.Name)
	}
	return rows, names, nil
}

func ruleOfProject(rule store.NotificationRule, project string, apps []string) bool {
	if rule.Project == project {
		return true
	}
	for _, a := range apps {
		if rule.App == a {
			return true
		}
	}
	return false
}

func (c *Console) projectAddress(project, done string) string {
	return "/projects/" + url.PathEscape(project) + "?done=" + done + "#notifications"
}

func (c *Console) addProjectRule(w http.ResponseWriter, r *http.Request, p auth.Principal, project store.Project) {
	scope, app := project.Name, r.PostFormValue("app")
	if app != "" {
		// The app has to be one of this project's: being its maintainer says nothing of any other.
		of, err := c.db.AppProject(r.Context(), app)
		if err != nil || of.ID != project.ID {
			c.showProject(w, r, http.StatusUnprocessableEntity, projectView{}, &failure{Message: "That app is not in this project."})
			return
		}
		scope = ""
	}
	_, err := c.notifier.AddRule(r.Context(), r.PostFormValue("channel"), scope, app, r.PostForm["events"], p.Name)
	if refused := ruleFailure(err); refused != nil {
		c.showProject(w, r, http.StatusUnprocessableEntity, projectView{}, refused)
		return
	}
	if err != nil {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, c.projectAddress(project.Name, "rule"), http.StatusSeeOther)
}

func (c *Console) removeProjectRule(w http.ResponseWriter, r *http.Request, p auth.Principal, project store.Project) {
	rule, err := c.db.NotificationRule(r.Context(), r.PostFormValue("rule"))
	if err == nil {
		// Only a rule of this project: being its maintainer says nothing of any other.
		allowed := rule.Project == project.Name
		if rule.App != "" {
			of, err := c.db.AppProject(r.Context(), rule.App)
			allowed = err == nil && of.ID == project.ID
		}
		if !allowed {
			c.message(w, http.StatusForbidden, data{Title: "Refused", Heading: "You may not do that", Message: "That rule is not of this project."})
			return
		}
		err = c.db.DeleteNotificationRule(r.Context(), rule.ID)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		c.internal(w, err)
		return
	}
	http.Redirect(w, r, c.projectAddress(project.Name, "rule-removed"), http.StatusSeeOther)
}
