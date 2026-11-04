package backup

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/schedule"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Keyring seals what a destination needs.
type Keyring interface {
	SealBackup(value string) []byte
	OpenBackup(sealed []byte) (string, error)
}

// Keyed is a master key.
type Keyed interface {
	Derive(purpose string) []byte
}

// Fingerprint says which master key something needs, and gives nothing of it away.
func Fingerprint(key Keyed) string {
	return hex.EncodeToString(key.Derive("backup fingerprint")[:8])
}

// ErrAlreadyRunning says that one backup at a time is enough.
var ErrAlreadyRunning = errors.New("a backup is already going on")

// keepRecords is how many past backups the server remembers.
const keepRecords = 200

// Service makes the backups that a schedule says, and those asked for.
type Service struct {
	DB             *store.DB
	Bus            *events.Bus
	Keyring        Keyring
	Log            *slog.Logger
	DataDir        string
	Version        string
	Domain         string
	KeyFingerprint string
	// Tick, if set, overrides how often it looks at the schedule (tests).
	Tick time.Duration

	mu      sync.Mutex
	running bool
	going   sync.WaitGroup
	// life is what ends a backup: the server stopping, and not whoever asked for it going away.
	life context.Context
}

// DefaultDestination is where backups go if nobody has said: on the server's own disk, which is a
// start and not a plan.
func (s *Service) DefaultDestination() string { return filepath.Join(s.DataDir, "backups", "archives") }

// Run makes what the schedule says until ctx ends. A backup that was due while the server was down
// is made once it is back.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.life = ctx
	s.mu.Unlock()
	if err := s.DB.EndInterruptedBackups(ctx, time.Now()); err != nil && ctx.Err() == nil {
		s.Log.Warn("note the backups that were interrupted", "err", err)
	}
	tick := s.Tick
	if tick == 0 {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			next, err := s.Next(ctx)
			if err != nil || next.IsZero() || next.After(now) {
				continue
			}
			if _, err := s.Start(ctx, "schedule", ""); err != nil && !errors.Is(err, ErrAlreadyRunning) && ctx.Err() == nil {
				s.Log.Warn("start a backup", "err", err)
			}
		}
	}
}

// Wait returns when what is going on has ended.
func (s *Service) Wait() { s.going.Wait() }

// Next says when the schedule next wants a backup, if it does.
func (s *Service) Next(ctx context.Context) (time.Time, error) {
	cfg, err := s.DB.BackupConfig(ctx)
	if err != nil || cfg.Schedule == "" {
		return time.Time{}, err
	}
	parsed, err := schedule.Parse(cfg.Schedule)
	if err != nil {
		return time.Time{}, err
	}
	since := cfg.UpdatedAt
	list, err := s.DB.Backups(ctx, 50)
	if err != nil {
		return time.Time{}, err
	}
	for _, b := range list {
		if b.Trigger == "schedule" && b.StartedAt.After(since) {
			since = b.StartedAt
			break
		}
	}
	return parsed.Next(since, time.Local), nil
}

// DestinationOf is where a configuration says.
func (s *Service) DestinationOf(cfg store.BackupConfig) (Destination, error) {
	dest := cfg.Destination
	if dest == "" {
		dest = s.DefaultDestination()
	}
	creds := Credentials{Endpoint: cfg.Endpoint, Region: cfg.Region, AccessKey: cfg.AccessKey}
	if len(cfg.SealedSecret) > 0 {
		secret, err := s.Keyring.OpenBackup(cfg.SealedSecret)
		if err != nil {
			return nil, err
		}
		creds.SecretKey = secret
	}
	return Parse(dest, creds)
}

// Start begins a backup, and returns at once with what it noted of it.
func (s *Service) Start(ctx context.Context, trigger, by string) (store.Backup, error) {
	cfg, err := s.DB.BackupConfig(ctx)
	if err != nil {
		return store.Backup{}, err
	}
	dest, err := s.DestinationOf(cfg)
	if err != nil {
		return store.Backup{}, err
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return store.Backup{}, ErrAlreadyRunning
	}
	s.running = true
	life := s.life
	s.mu.Unlock()
	if life == nil {
		life = context.WithoutCancel(ctx)
	}
	b := store.Backup{ID: ids.New(ids.Backup), StartedAt: time.Now().UTC(), Status: store.BackupRunning, Trigger: trigger, CreatedBy: by, Destination: dest.String()}
	if err := s.DB.AddBackup(ctx, b); err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		return b, err
	}
	s.going.Add(1)
	go func() {
		defer s.going.Done()
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		s.run(life, b, dest, cfg)
	}()
	return b, nil
}

func (s *Service) run(ctx context.Context, b store.Backup, dest Destination, cfg store.BackupConfig) {
	result, err := Run(ctx, dest, Options{DataDir: s.DataDir, KeyFingerprint: s.KeyFingerprint, Version: s.Version, Domain: s.Domain})
	done := context.WithoutCancel(ctx)
	b.FinishedAt = time.Now().UTC()
	if err != nil {
		b.Status, b.Error = store.BackupFailed, firstLine(err.Error())
		s.Log.Warn("a backup failed", "destination", dest.String(), "err", err)
		s.Bus.Publish(done, store.Event{Type: events.BackupFailed, Actor: b.CreatedBy, Data: map[string]any{"destination": dest.String(), "error": b.Error}})
	} else {
		b.Status, b.Name, b.SizeBytes = store.BackupDone, result.Name, result.Size
		s.Log.Info("a backup is made", "destination", dest.String(), "name", result.Name, "files", result.Files, "bytes", result.Size)
		s.Bus.Publish(done, store.Event{Type: events.BackupDone, Actor: b.CreatedBy, Data: map[string]any{"destination": dest.String(), "name": result.Name, "bytes": result.Size}})
		if b.Trigger == "schedule" {
			if err := Prune(done, dest, cfg.Keep); err != nil {
				s.Log.Warn("let go of old backups", "destination", dest.String(), "err", err)
			}
		}
	}
	if err := s.DB.FinishBackup(done, b); err != nil {
		s.Log.Warn("note how a backup went", "err", err)
	}
	if err := s.DB.PruneBackupRecords(done, keepRecords); err != nil {
		s.Log.Warn("let go of old records of backups", "err", err)
	}
}

// Form is what someone asks of where backups go. An empty secret key keeps the one there is.
type Form struct {
	Destination, Endpoint, Region, AccessKey, SecretKey string
	Schedule                                            string
	Keep                                                int
}

// Configure keeps what someone asks, if it can work.
func (s *Service) Configure(ctx context.Context, form Form, by string) error {
	cfg, err := s.configFrom(ctx, form)
	if err != nil {
		return err
	}
	cfg.UpdatedBy, cfg.UpdatedAt = by, time.Now().UTC()
	return s.DB.SetBackupConfig(ctx, cfg)
}

// CheckForm says what would stop Configure, changing nothing.
func (s *Service) CheckForm(ctx context.Context, form Form) error {
	_, err := s.configFrom(ctx, form)
	return err
}

func (s *Service) configFrom(ctx context.Context, form Form) (store.BackupConfig, error) {
	form.Destination = strings.TrimSpace(form.Destination)
	form.Schedule = strings.TrimSpace(form.Schedule)
	var none store.BackupConfig
	if form.Keep < 0 || form.Keep > 10000 {
		return none, &RefusedError{"how many to keep has to be between 0, which is all of them, and 10000"}
	}
	if form.Schedule != "" {
		if _, err := schedule.Parse(form.Schedule); err != nil {
			return none, &RefusedError{"the schedule " + err.Error()}
		}
	}
	old, err := s.DB.BackupConfig(ctx)
	if err != nil {
		return none, err
	}
	cfg := store.BackupConfig{Destination: form.Destination, Endpoint: strings.TrimSpace(form.Endpoint), Region: strings.TrimSpace(form.Region), AccessKey: strings.TrimSpace(form.AccessKey),
		SealedSecret: old.SealedSecret, Schedule: form.Schedule, Keep: form.Keep}
	if form.SecretKey != "" {
		cfg.SealedSecret = s.Keyring.SealBackup(form.SecretKey)
	}
	if strings.HasPrefix(form.Destination, "s3://") {
		if len(cfg.SealedSecret) == 0 {
			return none, &RefusedError{"an S3 destination needs a secret key"}
		}
	} else {
		cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SealedSecret = "", "", "", nil
		if form.Destination != "" && !filepath.IsAbs(form.Destination) {
			return none, &RefusedError{"a destination is a full path, or s3://bucket/prefix"}
		}
	}
	if _, err := s.DestinationOf(cfg); err != nil {
		return none, err
	}
	return cfg, nil
}

// Stored lists what is at the destination.
func (s *Service) Stored(ctx context.Context) ([]Stored, string, error) {
	cfg, err := s.DB.BackupConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	dest, err := s.DestinationOf(cfg)
	if err != nil {
		return nil, "", err
	}
	list, err := dest.List(ctx)
	return list, dest.String(), err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return fmt.Sprintf("%.300s", line)
}
