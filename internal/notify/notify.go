// Package notify tells people what happened (spec/07-observability.md, "Notifications"): by rules,
// over webhooks, chat webhooks and email, and in the console. One crash-looping app sends one
// message and not hundreds, then a word when it recovers.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// What a message can be, beside the events themselves.
const (
	// CrashLoop is several instance.exited in a short time.
	CrashLoop = "instance.crashloop"
	// Recovered says that what went wrong has come right. A rule asks for it by asking for
	// what went wrong.
	Recovered = "recovered"
	// Everything is what a rule names to have every alert.
	Everything = "*"
)

// RuleEvent is something a rule can ask for.
type RuleEvent struct {
	Kind, Label string
	// Default says that a new rule asks for it unless told otherwise.
	Default bool
}

// RuleEvents are what rules can ask for.
var RuleEvents = []RuleEvent{
	{events.DeploymentFailed, "A deployment fails", true},
	{CrashLoop, "An app keeps crashing", true},
	{events.CronFailed, "A cron job fails", true},
	{events.DiskLow, "The server's disk is nearly full", true},
	{events.BackupFailed, "A backup fails", true},
	{events.InstanceUnhealthy, "An instance is unhealthy", false},
	{events.DeploymentLive, "A deployment goes live", false},
}

// EventLabel says what a rule asks for, in words.
func EventLabel(kind string) string {
	if kind == Everything {
		return "Everything"
	}
	for _, e := range RuleEvents {
		if e.Kind == kind {
			return e.Label
		}
	}
	return kind
}

// Level says how a notification should look: "ok", "warn" or "fail".
func Level(kind string) string {
	switch kind {
	case Recovered, events.DeploymentLive:
		return "ok"
	case events.DiskLow, events.InstanceUnhealthy:
		return "warn"
	}
	return "fail"
}

// Keyring seals what channels need.
type Keyring interface {
	SealChannel(id string, value []byte) []byte
	OpenChannel(id string, sealed []byte) ([]byte, error)
}

// How much is too much.
const (
	defaultWindow  = 15 * time.Minute
	crashWindow    = 5 * time.Minute
	crashThreshold = 3
	// quietAfter is how long without an exit shows that a process has stopped crashing. It is
	// longer than what a supervisor waits before trying a process that keeps crashing again.
	quietAfter    = 10 * time.Minute
	retries       = 3
	keepInbox     = 90 * 24 * time.Hour
	diskLowAt     = 90.0
	diskOKAt      = 85.0
	maxNameLength = 64
)

// Service tells people.
type Service struct {
	DB      *store.DB
	Bus     *events.Bus
	Keyring Keyring
	Log     *slog.Logger
	// ConsoleURL is what links start with.
	ConsoleURL string
	// Allow lists addresses that channels may reach though they would otherwise be refused.
	Allow []netip.Prefix
	// DataDir is the volume whose space is watched.
	DataDir string
	// RetryAfter, if set, overrides how long to wait before trying again (tests).
	RetryAfter time.Duration
	// Window, if set, overrides how long alerts of a kind are held back after one that went (tests).
	Window time.Duration
	// DiskUsage, if set, overrides how the data volume's use is read, as a percentage (tests).
	DiskUsage func() (float64, error)

	mu sync.Mutex
	// recent is what went lately, by what would be held back.
	recent map[string]*sentAlert
	// exits has when each process of each environment last exited, and crashing those that an
	// alert went out about, by "app/env/process".
	exits    map[string][]time.Time
	crashing map[string]crashingProcess
	// unhealthy has the instances that an alert went out about, by "app/env/instance".
	unhealthy map[string]bool
	// failing has the environments whose last deployment failed, by "app/env".
	failing map[string]bool
	diskLow bool
	sending sync.WaitGroup
	// following, if set, is told when it is following the bus (tests).
	following func()
}

type sentAlert struct {
	at   time.Time
	held int
}

type crashingProcess struct{ app, env, process string }

// recovery is something that has come right, by what a rule asks for to hear that it went wrong.
type recovery struct{ kind, process, summary string }

// Run tells people about events until ctx ends. Wait says when what it is sending has ended too.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.recent, s.exits, s.crashing = map[string]*sentAlert{}, map[string][]time.Time{}, map[string]crashingProcess{}
	s.unhealthy, s.failing = map[string]bool{}, map[string]bool{}
	s.mu.Unlock()
	s.sending.Add(1)
	go func() {
		defer s.sending.Done()
		s.watch(ctx)
	}()
	for ctx.Err() == nil {
		sub := s.Bus.Subscribe()
		if s.following != nil {
			s.following()
		}
		s.follow(ctx, sub)
		sub.Close()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second): // it fell behind: go on from now
		}
	}
}

// Wait returns when all that was being sent has gone, or given up as its context ended.
func (s *Service) Wait() { s.sending.Wait() }

func (s *Service) follow(ctx context.Context, sub *events.Subscription) {
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-sub.C:
			if !ok {
				return
			}
			s.handle(ctx, e)
		}
	}
}

// watch keeps an eye on what no event says, and keeps the inbox from growing for ever.
func (s *Service) watch(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.checkDisk(ctx)
			s.checkQuiet(ctx, now.UTC())
			if err := s.DB.PruneNotifications(ctx, now.Add(-keepInbox)); err != nil && ctx.Err() == nil {
				s.Log.Warn("let go of old notifications", "err", err)
			}
		}
	}
}

func (s *Service) diskUsed() (float64, error) {
	if s.DiskUsage != nil {
		return s.DiskUsage()
	}
	free, total, err := platform.DiskSpace(s.DataDir)
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, errors.New("the volume has no size")
	}
	return float64(total-free) / float64(total) * 100, nil
}

// checkDisk publishes when the data volume fills up, and again when it has room, with a gap between
// the two so that it does not go back and forth around one figure.
func (s *Service) checkDisk(ctx context.Context) {
	used, err := s.diskUsed()
	if err != nil {
		return
	}
	s.mu.Lock()
	was := s.diskLow
	s.diskLow = used >= diskLowAt || was && used >= diskOKAt
	now := s.diskLow
	s.mu.Unlock()
	switch {
	case now && !was:
		s.Bus.Publish(ctx, store.Event{Type: events.DiskLow, Data: map[string]any{"percent": int(used)}})
	case was && !now:
		s.Bus.Publish(ctx, store.Event{Type: events.DiskOK, Data: map[string]any{"percent": int(used)}})
	}
}

// consider says what an event comes to: alerts, by what a rule would ask for, and what has come
// right. An instance turning healthy says that it answers, and not that it has stopped crashing, so
// that a process crashing recovers by a new release, or by staying up (see checkQuiet).
func (s *Service) consider(e store.Event) (alerts []string, recovered []recovery) {
	where := e.App + "/" + e.Env
	process, _ := e.Data["process"].(string)
	instance, _ := e.Data["instance"].(string)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Type {
	case events.InstanceExited:
		key := where + "/" + process
		list := append(s.exits[key], e.At)
		for len(list) > 0 && e.At.Sub(list[0]) > crashWindow {
			list = list[1:]
		}
		s.exits[key] = list
		if len(list) >= crashThreshold {
			s.crashing[key] = crashingProcess{e.App, e.Env, process}
			alerts = append(alerts, CrashLoop)
		}
	case events.InstanceUnhealthy:
		s.unhealthy[where+"/"+instance] = true
		alerts = append(alerts, events.InstanceUnhealthy)
	case events.InstanceHealthy:
		if s.unhealthy[where+"/"+instance] {
			delete(s.unhealthy, where+"/"+instance)
			if !s.anyUnhealthy(where) {
				recovered = append(recovered, recovery{kind: events.InstanceUnhealthy, summary: "Its instances are healthy again"})
			}
		}
	case events.DeploymentFailed:
		s.failing[where] = true
		alerts = append(alerts, events.DeploymentFailed)
	case events.DeploymentLive:
		if s.failing[where] {
			delete(s.failing, where)
			recovered = append(recovered, recovery{kind: events.DeploymentFailed, summary: "A deployment went live after one that failed"})
		}
		// A new release means new instances, so that what was wrong with the old ones is over.
		for key, c := range s.crashing {
			if strings.HasPrefix(key, where+"/") {
				delete(s.crashing, key)
				recovered = append(recovered, recovery{kind: CrashLoop, process: c.process, summary: "A new release replaced the process " + c.process + ", which kept crashing"})
			}
		}
		if s.anyUnhealthy(where) {
			for key := range s.unhealthy {
				if strings.HasPrefix(key, where+"/") {
					delete(s.unhealthy, key)
				}
			}
			recovered = append(recovered, recovery{kind: events.InstanceUnhealthy, summary: "A new release replaced its unhealthy instances"})
		}
		for key := range s.exits {
			if strings.HasPrefix(key, where+"/") {
				delete(s.exits, key)
			}
		}
		alerts = append(alerts, events.DeploymentLive)
	case events.CronFailed:
		alerts = append(alerts, events.CronFailed)
	case events.DiskLow:
		s.failing[diskKey] = true
		alerts = append(alerts, events.DiskLow)
	case events.BackupFailed:
		s.failing[backupKey] = true
		alerts = append(alerts, events.BackupFailed)
	case events.BackupDone:
		if s.failing[backupKey] {
			delete(s.failing, backupKey)
			recovered = append(recovered, recovery{kind: events.BackupFailed, summary: "A backup worked after one that failed"})
		}
	case events.DiskOK:
		if s.failing[diskKey] {
			delete(s.failing, diskKey)
			recovered = append(recovered, recovery{kind: events.DiskLow, summary: "The server's disk has room again"})
		}
	}
	return alerts, recovered
}

const (
	diskKey   = "disk"
	backupKey = "backup"
)

func (s *Service) anyUnhealthy(where string) bool {
	for key := range s.unhealthy {
		if strings.HasPrefix(key, where+"/") {
			return true
		}
	}
	return false
}

// checkQuiet says when a process that kept crashing has gone a while without exiting, and lets go of
// what it knew of those no longer exiting at all.
func (s *Service) checkQuiet(ctx context.Context, now time.Time) {
	s.mu.Lock()
	var quiet []crashingProcess
	for key, list := range s.exits {
		if len(list) > 0 && now.Sub(list[len(list)-1]) <= quietAfter {
			continue
		}
		delete(s.exits, key)
		if c, ok := s.crashing[key]; ok {
			delete(s.crashing, key)
			quiet = append(quiet, c)
		}
	}
	s.mu.Unlock()
	for _, c := range quiet {
		e := store.Event{App: c.app, Env: c.env, At: now, Data: map[string]any{"process": c.process}}
		s.tellRecovered(ctx, e, recovery{kind: CrashLoop, process: c.process, summary: fmt.Sprintf("The process %s has not crashed for %d minutes", c.process, int(quietAfter.Minutes()))})
	}
}

func (s *Service) handle(ctx context.Context, e store.Event) {
	alerts, recovered := s.consider(e)
	for _, kind := range alerts {
		s.dispatch(ctx, e, kind, Message{Kind: kind, App: e.App, Env: e.Env, Summary: summaryOf(kind, e), Link: s.linkOf(e), At: e.At, Data: e.Data})
	}
	for _, r := range recovered {
		s.tellRecovered(ctx, e, r)
	}
}

func (s *Service) tellRecovered(ctx context.Context, e store.Event, r recovery) {
	e.Data = maps.Clone(e.Data)
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	if r.process != "" {
		e.Data["process"] = r.process
	}
	s.dispatch(ctx, e, r.kind, Message{Kind: Recovered, App: e.App, Env: e.Env, Summary: r.summary, Link: s.linkOf(e), At: e.At, Data: map[string]any{"recovered_from": r.kind}})
}

func summaryOf(kind string, e store.Event) string {
	if kind == CrashLoop {
		if process, _ := e.Data["process"].(string); process != "" {
			return "The process " + process + " keeps crashing, and is being restarted"
		}
		return "It keeps crashing, and is being restarted"
	}
	return events.Summary(e)
}

func (s *Service) linkOf(e store.Event) string {
	if id, _ := e.Data["deployment"].(string); id != "" {
		return s.ConsoleURL + "/deployments/" + url.PathEscape(id)
	}
	if id, _ := e.Data["run"].(string); id != "" && e.App != "" {
		return s.ConsoleURL + "/apps/" + url.PathEscape(e.App) + "/runs/" + url.PathEscape(id)
	}
	if e.App != "" {
		link := s.ConsoleURL + "/apps/" + url.PathEscape(e.App)
		if e.Env != "" && e.Env != "production" {
			link += "?env=" + url.QueryEscape(e.Env)
		}
		return link
	}
	if strings.HasPrefix(e.Type, "backup.") {
		return s.ConsoleURL + "/settings/backups"
	}
	return s.ConsoleURL + "/settings/system"
}

func (s *Service) window() time.Duration {
	if s.Window > 0 {
		return s.Window
	}
	return defaultWindow
}

// admit says whether an alert may go now and, if it may, how many like it have been held back since
// the last that went.
func (s *Service) admit(key string, at time.Time) (held int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.recent[key]
	if last != nil && at.Sub(last.at) < s.window() {
		last.held++
		return 0, false
	}
	if last != nil {
		held = last.held
	}
	s.recent[key] = &sentAlert{at: at}
	for k, v := range s.recent {
		if at.Sub(v.at) > 4*s.window() {
			delete(s.recent, k)
		}
	}
	return held, true
}

// repeats says whether an alert is held back after one goes: what can happen over and over by itself.
// A deployment happens because someone asked for one, and whoever did is to hear how it went.
func repeats(kind string) bool {
	return kind != events.DeploymentFailed && kind != events.DeploymentLive
}

// dispatch sends a message to all it should go to, unless one like it went a moment ago. kind is
// what a rule asks for to have it. That something has recovered makes it news if it goes wrong
// again, unless it has been going back and forth.
func (s *Service) dispatch(ctx context.Context, e store.Event, kind string, m Message) {
	key := strings.Join([]string{kind, e.App, e.Env}, "/")
	if kind == CrashLoop {
		process, _ := e.Data["process"].(string)
		key += "/" + process
	}
	if job, _ := e.Data["job"].(string); job != "" {
		key += "/" + job
	}
	if repeats(kind) {
		if m.Kind == Recovered {
			if _, ok := s.admit("recovered/"+key, e.At); !ok {
				return
			}
			s.mu.Lock()
			delete(s.recent, key)
			s.mu.Unlock()
		} else {
			held, ok := s.admit(key, e.At)
			if !ok {
				return
			}
			m.Suppressed = held
		}
	}
	s.toChannels(ctx, e, kind, m)
	s.tellPeople(ctx, e, kind, m)
}

func (s *Service) toChannels(ctx context.Context, e store.Event, kind string, m Message) {
	rules, err := s.DB.NotificationRules(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Warn("read the notification rules", "err", err)
		}
		return
	}
	project := ""
	if e.App != "" {
		if p, err := s.DB.AppProject(ctx, e.App); err == nil {
			project = p.Name
		}
	}
	sent := map[string]bool{}
	for _, r := range rules {
		if sent[r.ChannelID] || !ruleApplies(r, kind, e.App, project) {
			continue
		}
		sent[r.ChannelID] = true
		s.sending.Add(1)
		go func(id string) {
			defer s.sending.Done()
			c, err := s.DB.NotificationChannel(ctx, id)
			if err != nil {
				return
			}
			if err := s.send(ctx, c, m); err != nil && ctx.Err() == nil {
				s.Log.Warn("send a notification", "channel", c.Name, "err", err)
			}
		}(r.ChannelID)
	}
}

// ruleApplies says whether a rule is for an alert: for what it names, of the app, its project or
// the whole server. What is of the server as a whole, such as its disk, is only for rules of the
// whole server.
func ruleApplies(r store.NotificationRule, kind, app, project string) bool {
	if !slices.Contains(r.Events, kind) && !slices.Contains(r.Events, Everything) {
		return false
	}
	switch {
	case r.App != "":
		return r.App == app
	case r.Project != "":
		return r.Project == project
	}
	return true
}

// send delivers a message to a channel, trying again a little later if it does not go, and notes
// how it went.
func (s *Service) send(ctx context.Context, c store.NotificationChannel, m Message) error {
	cfg, err := s.configOf(c)
	if err != nil {
		return err
	}
	wait := s.RetryAfter
	if wait == 0 {
		wait = 10 * time.Second
	}
	for attempt := 1; ; attempt++ {
		err = s.deliver(ctx, c.Kind, cfg, m)
		if err == nil || attempt == retries {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait * time.Duration(attempt)):
		}
	}
	s.noteResult(ctx, c, err)
	return err
}

func (s *Service) noteResult(ctx context.Context, c store.NotificationChannel, err error) {
	failure := ""
	if err != nil {
		failure = err.Error()
	}
	if serr := s.DB.SetChannelResult(ctx, c.ID, time.Now().UTC(), failure); serr != nil && ctx.Err() == nil {
		s.Log.Warn("note how a notification went", "err", serr)
	}
}

func (s *Service) configOf(c store.NotificationChannel) (Config, error) {
	var cfg Config
	plain, err := s.Keyring.OpenChannel(c.ID, c.Sealed)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(plain, &cfg)
}

// AddChannel keeps a channel, sealed. It answers with the secret that signs a webhook, which is
// never shown again.
func (s *Service) AddChannel(ctx context.Context, name, kind string, cfg Config, by string) (store.NotificationChannel, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxNameLength || strings.ContainsFunc(name, unicode.IsControl) {
		return store.NotificationChannel{}, "", &InvalidError{fmt.Sprintf("A channel needs a name of up to %d characters.", maxNameLength)}
	}
	if err := cfg.Check(kind); err != nil {
		return store.NotificationChannel{}, "", err
	}
	plain, err := json.Marshal(cfg)
	if err != nil {
		return store.NotificationChannel{}, "", err
	}
	id := ids.New(ids.Channel)
	c := store.NotificationChannel{ID: id, Name: name, Kind: kind, Sealed: s.Keyring.SealChannel(id, plain), CreatedBy: by, CreatedAt: time.Now().UTC()}
	if err := s.DB.CreateNotificationChannel(ctx, c); err != nil {
		return store.NotificationChannel{}, "", err
	}
	return c, cfg.Secret, nil
}

// Target says where a channel goes, without what is secret in its address.
func (s *Service) Target(c store.NotificationChannel) string {
	cfg, err := s.configOf(c)
	if err != nil {
		return "It cannot be opened"
	}
	if c.Kind == KindEmail {
		return strings.Join(cfg.To, ", ")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// AddRule keeps a rule: for an app, or a project, or the whole server.
func (s *Service) AddRule(ctx context.Context, channel, project, app string, kinds []string, by string) (store.NotificationRule, error) {
	if project != "" && app != "" {
		return store.NotificationRule{}, &InvalidError{"A rule is for a project or an app, not both."}
	}
	if len(kinds) == 0 {
		return store.NotificationRule{}, &InvalidError{"A rule needs something to ask for."}
	}
	for _, k := range kinds {
		known := k == Everything
		for _, e := range RuleEvents {
			known = known || e.Kind == k
		}
		if !known {
			return store.NotificationRule{}, &InvalidError{fmt.Sprintf("%q is not something a rule can ask for.", k)}
		}
	}
	c, err := s.DB.NotificationChannel(ctx, channel)
	if err != nil {
		return store.NotificationRule{}, err
	}
	r := store.NotificationRule{ID: ids.New(ids.Rule), ChannelID: c.ID, ChannelName: c.Name, Project: project, App: app, Events: kinds, CreatedBy: by, CreatedAt: time.Now().UTC()}
	return r, s.DB.CreateNotificationRule(ctx, r)
}

// Test sends a channel something to show that it works, and says how it went, at once.
func (s *Service) Test(ctx context.Context, channel, by string) (store.NotificationChannel, error) {
	c, err := s.DB.NotificationChannel(ctx, channel)
	if err != nil {
		return c, err
	}
	cfg, err := s.configOf(c)
	if err != nil {
		return c, err
	}
	m := Message{Kind: "test", Summary: "A test, from " + by + ": this channel works", Link: s.ConsoleURL, At: time.Now().UTC(), Data: map[string]any{}}
	err = s.deliver(ctx, c.Kind, cfg, m)
	s.noteResult(ctx, c, err)
	return c, err
}

// toldInConsole are the alerts people are told of in the console, whatever the rules say.
var toldInConsole = []string{events.DeploymentFailed, CrashLoop, events.CronFailed, events.DiskLow, events.BackupFailed}

// tellPeople puts an alert in the console of those it concerns: a project's maintainers, whoever
// pushed a deployment that failed and, for all of it, the server's administrators.
func (s *Service) tellPeople(ctx context.Context, e store.Event, kind string, m Message) {
	if !slices.Contains(toldInConsole, kind) {
		return
	}
	people, err := s.concerned(ctx, e)
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Warn("work out whom to tell", "err", err)
		}
		return
	}
	link := strings.TrimPrefix(m.Link, s.ConsoleURL)
	list := make([]store.Notification, 0, len(people))
	for _, id := range people {
		list = append(list, store.Notification{ID: ids.New(ids.Notification), UserID: id, Kind: m.Kind, App: e.App, Env: e.Env, Summary: m.Summary, Link: link, CreatedAt: e.At})
	}
	if err := s.DB.AddNotifications(ctx, list); err != nil && ctx.Err() == nil {
		s.Log.Warn("add notifications", "err", err)
	}
}

func (s *Service) concerned(ctx context.Context, e store.Event) ([]string, error) {
	// Who maintains the app's project: those who are made so, and everyone else if its project says so.
	everyone := ""
	roles := map[string]string{}
	if e.App != "" {
		p, err := s.DB.AppProject(ctx, e.App)
		if err == nil {
			everyone = p.EveryoneRole
			members, err := s.DB.Members(ctx, p.Name)
			if err != nil {
				return nil, fmt.Errorf("members: %w", err)
			}
			for _, m := range members {
				roles[m.UserID] = m.Role
			}
		}
	}
	var people []string
	for after := ""; ; {
		users, err := s.DB.Users(ctx, after, 500)
		if err != nil {
			return nil, fmt.Errorf("users: %w", err)
		}
		for _, u := range users {
			if u.Disabled() {
				continue
			}
			role, member := roles[u.ID]
			if !member {
				role = everyone
			}
			// Whoever pushed a deployment is told of it, whatever else they are.
			if u.Role == store.RoleAdmin || role == store.RoleMaintainer || e.Actor != "" && strings.EqualFold(u.Email, e.Actor) {
				people = append(people, u.ID)
			}
		}
		if len(users) < 500 {
			return people, nil
		}
		after = users[len(users)-1].Email
	}
}
