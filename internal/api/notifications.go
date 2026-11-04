package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func (s *Server) notifyFailed(w http.ResponseWriter, err error) bool {
	var invalid *notify.InvalidError
	switch {
	case err == nil:
		return false
	case errors.As(err, &invalid):
		problem(w, http.StatusUnprocessableEntity, "invalid_notification", invalid.Msg)
	case errors.Is(err, store.ErrChannelTaken):
		problem(w, http.StatusConflict, "channel_taken", sentenceOf(err))
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", sentenceOf(err))
	default:
		s.internal(w, err)
	}
	return true
}

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	doc := apitypes.Inbox{Items: []apitypes.Notification{}}
	if p.Local() {
		writeJSON(w, http.StatusOK, doc)
		return
	}
	list, err := s.db.Notifications(r.Context(), p.UserID, 100)
	if err != nil {
		s.internal(w, err)
		return
	}
	visible, err := s.auth.Visible(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, n := range list {
		// What they have since lost sight of is no longer theirs to read.
		if n.App != "" && !visible(n.App) || n.App == "" && p.App != "" {
			continue
		}
		item := apitypes.Notification{ID: n.ID, Kind: n.Kind, App: n.App, Env: n.Env, Summary: n.Summary, At: n.CreatedAt}
		if n.Link != "" {
			item.Link = s.cfg.PublicURL(s.cfg.ConsoleHost) + n.Link
		}
		if n.ReadAt.IsZero() {
			doc.Unread++
		} else {
			at := n.ReadAt
			item.ReadAt = &at
		}
		doc.Items = append(doc.Items, item)
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) readInbox(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if !p.Local() {
		if err := s.db.ReadNotifications(r.Context(), p.UserID, time.Now().UTC()); err != nil {
			s.internal(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) channelDoc(c store.NotificationChannel, admin bool) apitypes.NotificationChannel {
	doc := apitypes.NotificationChannel{ID: c.ID, Name: c.Name, Kind: c.Kind}
	if admin {
		created := c.CreatedAt
		doc.Target, doc.CreatedBy, doc.CreatedAt, doc.Error = s.notifier.Target(c), c.CreatedBy, &created, c.Error
		if !c.SentAt.IsZero() {
			sent := c.SentAt
			doc.SentAt = &sent
		}
	}
	return doc
}

func (s *Server) channels(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.NotificationChannels(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	p := who(r)
	docs := []apitypes.NotificationChannel{}
	for _, c := range list {
		docs = append(docs, s.channelDoc(c, p.Admin && p.Can(auth.ScopeAdmin)))
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NewNotificationChannel
	if !decode(w, r, &req) {
		return
	}
	cfg := notify.Config{URL: req.URL, Host: req.Host, Port: req.Port, Username: req.Username, Password: req.Password, From: req.From, To: req.To}
	c, secret, err := s.notifier.AddChannel(r.Context(), req.Name, req.Kind, cfg, who(r).Name)
	if s.notifyFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, apitypes.CreatedChannel{NotificationChannel: s.channelDoc(c, true), Secret: secret})
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.NotificationChannel(r.Context(), r.PathValue("channel"))
	if s.notifyFailed(w, err) {
		return
	}
	if s.notifyFailed(w, s.db.DeleteNotificationChannel(r.Context(), c.ID)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	_, err := s.notifier.Test(r.Context(), r.PathValue("channel"), who(r).Name)
	if errors.Is(err, store.ErrNotFound) {
		s.notifyFailed(w, err)
		return
	}
	if err != nil {
		problem(w, http.StatusBadGateway, "channel_failed", "It did not go: "+sentenceOf(err))
		return
	}
	c, err := s.db.NotificationChannel(r.Context(), r.PathValue("channel"))
	if s.notifyFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, s.channelDoc(c, true))
}

func ruleDoc(rule store.NotificationRule) apitypes.NotificationRule {
	return apitypes.NotificationRule{ID: rule.ID, Channel: rule.ChannelName, Project: rule.Project, App: rule.App, Events: rule.Events, CreatedBy: rule.CreatedBy, CreatedAt: rule.CreatedAt}
}

// ruleAccess says what someone may do with a rule of a project or an app, or the whole server.
func (s *Server) ruleAccess(r *http.Request, project, app string) (auth.Access, error) {
	p := who(r)
	switch {
	case app != "":
		return s.auth.AppAccess(r.Context(), p, app)
	case project != "":
		found, err := s.db.ProjectByName(r.Context(), project)
		if errors.Is(err, store.ErrNotFound) {
			return auth.Access{}, nil
		}
		if err != nil {
			return auth.Access{}, err
		}
		return s.auth.ProjectAccess(r.Context(), p, found)
	}
	return auth.Access{See: p.Admin, Manage: p.Admin}, nil
}

func (s *Server) rules(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.NotificationRules(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	p := who(r)
	docs := []apitypes.NotificationRule{}
	for _, rule := range list {
		access, err := s.ruleAccess(r, rule.Project, rule.App)
		if err != nil {
			s.internal(w, err)
			return
		}
		if !access.See || p.App != "" && rule.App != p.App {
			continue
		}
		docs = append(docs, ruleDoc(rule))
	}
	writeJSON(w, http.StatusOK, docs)
}

const ruleRefused = "A rule of the whole server takes an admin, and one of a project or an app takes a maintainer of it."

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NewNotificationRule
	if !decode(w, r, &req) {
		return
	}
	access, err := s.ruleAccess(r, req.Project, req.App)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !access.Manage {
		problem(w, http.StatusForbidden, "forbidden", ruleRefused)
		return
	}
	rule, err := s.notifier.AddRule(r.Context(), req.Channel, req.Project, req.App, req.Events, who(r).Name)
	if s.notifyFailed(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, ruleDoc(rule))
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	rule, err := s.db.NotificationRule(r.Context(), r.PathValue("rule"))
	if s.notifyFailed(w, err) {
		return
	}
	access, err := s.ruleAccess(r, rule.Project, rule.App)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !access.Manage {
		if !access.See {
			problem(w, http.StatusNotFound, "not_found", "There is no such rule that you can see.")
			return
		}
		problem(w, http.StatusForbidden, "forbidden", ruleRefused)
		return
	}
	if s.notifyFailed(w, s.db.DeleteNotificationRule(r.Context(), rule.ID)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
