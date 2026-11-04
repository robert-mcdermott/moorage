// Package events records what happens on a server, and passes it on as it does:
// to the activity timeline, and to everyone following the server's one stream
// (spec/07-observability.md, "Events and activity").
package events

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// The kinds of events.
const (
	DeploymentQueued     = "deployment.queued"
	DeploymentStarted    = "deployment.started"
	DeploymentLive       = "deployment.live"
	DeploymentFailed     = "deployment.failed"
	DeploymentSuperseded = "deployment.superseded"
	DeploymentCanceled   = "deployment.canceled"
	ReleaseCreated       = "release.created"
	InstanceExited       = "instance.exited"
	InstanceUnhealthy    = "instance.unhealthy"
	InstanceHealthy      = "instance.healthy"
	InstanceReplaced     = "instance.replaced"
	ConfigChanged        = "config.changed"
	ScaleChanged         = "scale.changed"
	AppRestarted         = "app.restarted"
	AppRenamed           = "app.renamed"
	AppDeleted           = "app.deleted"
	EnvironmentCreated   = "environment.created"
	EnvironmentDeleted   = "environment.deleted"
	EnvironmentLocked    = "environment.locked"
	EnvironmentUnlocked  = "environment.unlocked"
	EnvironmentSlept     = "environment.slept"
	EnvironmentWoke      = "environment.woke"
	AccessChanged        = "access.changed"
	ShareCreated         = "share.created"
	ShareRevoked         = "share.revoked"
	CronFailed           = "cron.failed"
	BackupDone           = "backup.done"
	BackupFailed         = "backup.failed"
	ThresholdCrossed     = "threshold.crossed"
	ThresholdOK          = "threshold.ok"
	DiskLow              = "server.disk_low"
	DiskOK               = "server.disk_ok"
	DomainAdded          = "domain.added"
	DomainVerified       = "domain.verified"
	DomainRemoved        = "domain.removed"
	ServerStarted        = "server.started"
)

// Retention is how long events are kept.
const Retention = 90 * 24 * time.Hour

// buffer is how far a follower may fall behind. One that falls further loses its
// subscription, and catches up from the store when it reconnects.
const buffer = 256

// Bus records events and passes them on. A nil Bus does nothing, for what runs
// without one in tests.
type Bus struct {
	db  *store.DB
	log *slog.Logger

	mu     sync.Mutex
	subs   map[*Subscription]struct{}
	closed bool
}

// New returns a bus.
func New(db *store.DB, log *slog.Logger) *Bus {
	return &Bus{db: db, log: log, subs: map[*Subscription]struct{}{}}
}

// Subscription is a follower's view of the bus.
type Subscription struct {
	C   <-chan store.Event
	c   chan store.Event
	bus *Bus
}

// Subscribe starts following events. The channel closes when the bus does, or if
// the follower falls too far behind.
func (b *Bus) Subscribe() *Subscription {
	c := make(chan store.Event, buffer)
	s := &Subscription{C: c, c: c, bus: b}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(c)
		return s
	}
	b.subs[s] = struct{}{}
	return s
}

// Close stops following.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	if _, ok := s.bus.subs[s]; ok {
		delete(s.bus.subs, s)
		close(s.c)
	}
}

// Publish records an event and passes it on. A failure to record it is logged:
// what happened has happened, and the caller has its own work to finish.
func (b *Bus) Publish(ctx context.Context, e store.Event) {
	if b == nil {
		return
	}
	e.ID = ids.New(ids.Event)
	e.At = time.Now().UTC()
	if !Transient(e.Type) {
		if err := b.db.AddEvent(context.WithoutCancel(ctx), e); err != nil {
			b.log.Error("record an event", "type", e.Type, "err", err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.c <- e:
		default:
			delete(b.subs, s)
			close(s.c)
		}
	}
}

// Close ends every subscription: a server stopping should not wait for its
// followers to go away.
func (b *Bus) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for s := range b.subs {
		delete(b.subs, s)
		close(s.c)
	}
}

// Prune keeps the store to the retention period, until ctx ends.
func (b *Bus) Prune(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := b.db.PruneEvents(ctx, time.Now().Add(-Retention)); err != nil {
			if ctx.Err() == nil {
				b.log.Warn("prune the events", "err", err)
			}
		} else if n > 0 {
			b.log.Info("removed old events", "events", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Transient reports whether events of a type are only for those following now, and
// not worth keeping: a deployment that starts is queued a moment before, and ends
// in its result.
func Transient(kind string) bool { return kind == DeploymentStarted }

// Level says how an event of a type should look: "ok", "info", "warn" or "fail".
func Level(kind string) string {
	switch kind {
	case DeploymentLive, InstanceHealthy, DomainVerified:
		return "ok"
	case DeploymentFailed, CronFailed:
		return "fail"
	case DiskLow, ThresholdCrossed:
		return "warn"
	case ThresholdOK:
		return "ok"
	case BackupFailed:
		return "fail"
	case BackupDone:
		return "ok"
	case InstanceExited, InstanceUnhealthy, InstanceReplaced:
		return "warn"
	case DeploymentQueued, DeploymentStarted, ReleaseCreated, ConfigChanged, ScaleChanged, AppRestarted, AppRenamed, EnvironmentCreated, EnvironmentUnlocked, EnvironmentWoke, AccessChanged, ShareCreated, ShareRevoked, DomainAdded, DomainRemoved:
		return "info"
	case AppDeleted, EnvironmentDeleted, EnvironmentLocked:
		return "warn"
	}
	return ""
}

func text(e store.Event, key string) string {
	s, _ := e.Data[key].(string)
	return s
}

func number(e store.Event, key string) int {
	switch n := e.Data[key].(type) {
	case int:
		return n
	case float64: // from the store, as JSON
		return int(n)
	}
	return 0
}

func names(e store.Event, key string) []string {
	switch list := e.Data[key].(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func thresholdWords(metric string) (name, unit string) {
	switch metric {
	case "error-rate":
		return "The error rate", "%"
	case "latency-p95":
		return "The 95th percentile of latency", " ms"
	case "memory":
		return "Memory", "% of its limit"
	case "cpu":
		return "CPU", "% of one core"
	}
	return metric, ""
}

// Summary says in a few words what happened, without saying where: a timeline
// shows the app beside it.
func Summary(e store.Event) string {
	by := ""
	if e.Actor != "" {
		by = " by " + e.Actor
	}
	switch e.Type {
	case DeploymentQueued:
		return "Deployment queued" + by + source(e)
	case DeploymentStarted:
		return "Deployment started" + source(e)
	case DeploymentLive:
		return fmt.Sprintf("Deployed v%d%s", number(e, "version"), by)
	case DeploymentFailed:
		if msg := text(e, "error"); msg != "" {
			return "Deployment failed: " + msg
		}
		return "Deployment failed"
	case DeploymentSuperseded:
		return "Deployment superseded by a newer one"
	case DeploymentCanceled:
		return "Deployment canceled"
	case ReleaseCreated:
		switch reason := text(e, "reason"); reason {
		case "promote":
			return fmt.Sprintf("Promoted%s: v%d now serves in %s", by, number(e, "version"), e.Env)
		case "rollback":
			return fmt.Sprintf("Rolled back%s: v%d now serves", by, number(e, "version"))
		case "config":
			return fmt.Sprintf("Release v%d, for a change of variables%s", number(e, "version"), by)
		default:
			return fmt.Sprintf("Release v%d is current", number(e, "version"))
		}
	case InstanceExited:
		what := fmt.Sprintf("status %d", number(e, "code"))
		if sig := text(e, "signal"); sig == "OOM" {
			what = "out of memory"
		} else if sig != "" {
			what = "signal " + sig
		}
		return fmt.Sprintf("%s exited (%s) and restarted", processName(e), what)
	case InstanceUnhealthy:
		return processName(e) + " is unhealthy, and gets no traffic"
	case InstanceHealthy:
		return processName(e) + " is healthy again"
	case InstanceReplaced:
		return processName(e) + " was lost, and is being replaced"
	case ConfigChanged:
		changed := append(append([]string{}, names(e, "set")...), names(e, "unset")...)
		return "Variables changed" + by + ": " + strings.Join(changed, ", ")
	case ScaleChanged:
		return "Scaled to " + text(e, "formation") + by
	case AppRestarted:
		if p := text(e, "process"); p != "" {
			return "Restarted " + p + by
		}
		return "Restarted" + by
	case AppRenamed:
		return "Renamed from " + text(e, "from") + by
	case AppDeleted:
		return "Deleted" + by
	case EnvironmentCreated:
		if branch := text(e, "branch"); branch != "" {
			return "Environment " + e.Env + " made, for the branch " + branch
		}
		return "Environment " + e.Env + " made"
	case EnvironmentDeleted:
		return "Environment " + e.Env + " removed" + by
	case BackupDone:
		return "A backup is at " + text(e, "destination")
	case BackupFailed:
		return "A backup failed: " + text(e, "error")
	case ThresholdCrossed:
		name, unit := thresholdWords(text(e, "metric"))
		return fmt.Sprintf("%s has been above %v%s for %v minutes, and is %v%s", name, e.Data["above"], unit, e.Data["minutes"], e.Data["value"], unit)
	case ThresholdOK:
		name, unit := thresholdWords(text(e, "metric"))
		return fmt.Sprintf("%s is back under %v%s, at %v%s", name, e.Data["above"], unit, e.Data["value"], unit)
	case DiskLow:
		return fmt.Sprintf("The server's data volume is %v%% full", e.Data["percent"])
	case DiskOK:
		return fmt.Sprintf("The server's data volume has room again, at %v%% full", e.Data["percent"])
	case CronFailed:
		return "The cron job " + text(e, "job") + " failed: " + text(e, "error")
	case DomainAdded:
		return text(e, "domain") + " attached to " + e.Env + by
	case DomainVerified:
		return text(e, "domain") + " points here, and serves " + e.Env
	case DomainRemoved:
		return text(e, "domain") + " detached from " + e.Env + by
	case AccessChanged:
		return e.Env + "'s gate is now " + text(e, "mode") + by
	case ShareCreated:
		return "A share link to " + e.Env + " was made" + by
	case ShareRevoked:
		return "A share link to " + e.Env + " was revoked" + by
	case EnvironmentSlept:
		if e.Actor == "" {
			return e.Env + " went to sleep: nothing was using it"
		}
		return e.Env + " put to sleep" + by
	case EnvironmentWoke:
		if e.Actor == "" {
			return e.Env + " woke for a request"
		}
		return e.Env + " woken" + by
	case EnvironmentLocked:
		return e.Env + " locked" + by + ": it takes no deployments"
	case EnvironmentUnlocked:
		return e.Env + " unlocked" + by
	case ServerStarted:
		if v := text(e, "version"); v != "" {
			return "The server started, version " + v
		}
		return "The server started"
	}
	return e.Type
}

func source(e store.Event) string {
	if s := text(e, "source"); s != "" {
		return ": " + s
	}
	return ""
}

func processName(e store.Event) string {
	if p := text(e, "process"); p != "" {
		return fmt.Sprintf("%s.%d", p, number(e, "index"))
	}
	return "An instance"
}
